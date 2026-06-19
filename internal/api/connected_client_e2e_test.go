package api

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/extmsg"
)

const (
	connectedClientProvider       = "llm-client"
	connectedClientConversationID = "conv-e2e-1"
)

func TestConnectedClientEndpointsAreRegisteredInOpenAPI(t *testing.T) {
	for _, source := range []struct {
		name string
		spec map[string]any
	}{
		{name: "committed", spec: readCommittedOpenAPISpec(t)},
		{name: "live-supervisor", spec: readLiveSupervisorOpenAPISpec(t)},
	} {
		t.Run(source.name, func(t *testing.T) {
			paths, ok := source.spec["paths"].(map[string]any)
			if !ok {
				t.Fatal("OpenAPI paths missing")
			}
			assertOpenAPIOperation(t, paths, http.MethodPost, "/v0/extmsg/clients")
			assertOpenAPIOperation(t, paths, http.MethodPost, "/v0/extmsg/inbound")
			assertOpenAPIOperation(t, paths, http.MethodGet, "/v0/extmsg/clients/{account_id}/conversations/{conversation_id}/subscribe")
		})
	}
}

func TestConnectedClientSubscribeInboundReplyFlow(t *testing.T) {
	fs, services, sessionInfo, baseURL, client := setupConnectedClientAPITest(t)

	reg := registerConnectedClient(t, client, baseURL)
	ref := connectedClientConversationRef(reg.ClientID, connectedClientConversationID)
	bindConnectedClientConversation(t, services, ref, sessionInfo.ID)

	stream, cancel := openConnectedClientSubscribe(t, client, baseURL, reg, "")
	defer cancel()
	defer func() { _ = stream.Body.Close() }()

	postConnectedClientInboundTurn(t, client, baseURL, reg, connectedClientConversationID, "status please")
	reply := postConnectedClientSessionReply(t, client, baseURL, fs, sessionInfo.ID, ref, "city is healthy")
	if !reply.Receipt.Delivered {
		t.Fatalf("session reply Delivered = false, want true; response=%+v", reply)
	}

	frame := readConnectedClientSSEFrame(t, stream)
	if frame.Event != "message" {
		t.Fatalf("SSE event = %q, want message; frame=%+v", frame.Event, frame)
	}
	if !frame.HasID {
		t.Fatalf("message frame missing id: %+v", frame)
	}
	payload := decodeConnectedClientSSEData(t, frame)
	assertConnectedClientMessagePayload(t, payload, connectedClientMessageWant{
		Text:           "city is healthy",
		SessionID:      sessionInfo.ID,
		AccountID:      reg.ClientID,
		ConversationID: connectedClientConversationID,
	})
	if got := fmt.Sprint(payload["sequence"]); got != frame.ID {
		t.Fatalf("payload sequence = %v, want frame id %q", payload["sequence"], frame.ID)
	}
}

func TestConnectedClientReconnectReplaysMissedRepliesAfterLastEventID(t *testing.T) {
	fs, services, sessionInfo, baseURL, client := setupConnectedClientAPITest(t)

	reg := registerConnectedClient(t, client, baseURL)
	ref := connectedClientConversationRef(reg.ClientID, connectedClientConversationID)
	bindConnectedClientConversation(t, services, ref, sessionInfo.ID)

	firstStream, firstCancel := openConnectedClientSubscribe(t, client, baseURL, reg, "")
	firstReply := postConnectedClientSessionReply(t, client, baseURL, fs, sessionInfo.ID, ref, "first reply")
	if !firstReply.Receipt.Delivered {
		t.Fatalf("first reply Delivered = false, want true; response=%+v", firstReply)
	}
	firstFrame := readConnectedClientSSEFrame(t, firstStream)
	firstCancel()
	_ = firstStream.Body.Close()
	if firstFrame.Event != "message" || firstFrame.ID == "" {
		t.Fatalf("first frame = %+v, want message with id", firstFrame)
	}

	missedReply := postConnectedClientSessionReply(t, client, baseURL, fs, sessionInfo.ID, ref, "missed while disconnected")
	if missedReply.TranscriptEntry == nil {
		t.Fatalf("disconnected reply missing TranscriptEntry; no-subscriber replies must be replayable: %+v", missedReply)
	}

	replayStream, replayCancel := openConnectedClientSubscribe(t, client, baseURL, reg, firstFrame.ID)
	defer replayCancel()
	defer func() { _ = replayStream.Body.Close() }()

	replayed := readConnectedClientSSEFrame(t, replayStream)
	if replayed.Event != "message" {
		t.Fatalf("replayed event = %q, want message; frame=%+v", replayed.Event, replayed)
	}
	payload := decodeConnectedClientSSEData(t, replayed)
	assertConnectedClientMessagePayload(t, payload, connectedClientMessageWant{
		Text:           "missed while disconnected",
		SessionID:      sessionInfo.ID,
		AccountID:      reg.ClientID,
		ConversationID: connectedClientConversationID,
	})
	if replayed.ID == "" || replayed.ID == firstFrame.ID {
		t.Fatalf("replayed id = %q, want a later sequence than first id %q", replayed.ID, firstFrame.ID)
	}
}

func TestConnectedClientNoSubscriberReplyRecordsAndLaterSubscribeReplays(t *testing.T) {
	fs, services, sessionInfo, baseURL, client := setupConnectedClientAPITest(t)

	reg := registerConnectedClient(t, client, baseURL)
	ref := connectedClientConversationRef(reg.ClientID, connectedClientConversationID)
	bindConnectedClientConversation(t, services, ref, sessionInfo.ID)

	result := postConnectedClientSessionReply(t, client, baseURL, fs, sessionInfo.ID, ref, "queued before subscriber")
	if result.Receipt.Delivered {
		t.Fatalf("no-subscriber reply Delivered = true, want false; response=%+v", result)
	}
	if result.TranscriptEntry == nil {
		t.Fatalf("no-subscriber reply missing TranscriptEntry; response=%+v", result)
	}
	if result.TranscriptEntry.Sequence == 0 {
		t.Fatalf("no-subscriber transcript sequence = 0, want replay cursor; response=%+v", result)
	}

	stream, cancel := openConnectedClientSubscribe(t, client, baseURL, reg, "0")
	defer cancel()
	defer func() { _ = stream.Body.Close() }()

	frame := readConnectedClientSSEFrame(t, stream)
	if frame.Event != "message" {
		t.Fatalf("replayed no-subscriber event = %q, want message; frame=%+v", frame.Event, frame)
	}
	payload := decodeConnectedClientSSEData(t, frame)
	assertConnectedClientMessagePayload(t, payload, connectedClientMessageWant{
		Text:           "queued before subscriber",
		SessionID:      sessionInfo.ID,
		AccountID:      reg.ClientID,
		ConversationID: connectedClientConversationID,
	})
}

func setupConnectedClientAPITest(t *testing.T) (*fakeState, *extmsg.Services, sessionInfoForConnectedClient, string, *http.Client) {
	t.Helper()

	fs := newSessionFakeState(t)
	services := extmsg.NewServices(fs.cityBeadStore)
	fs.extmsgSvc = &services
	fs.adapterReg = extmsg.NewAdapterRegistry()
	sessionInfo := createTestSession(t, fs.cityBeadStore, fs.sp, "Connected Client Session")

	server := httptest.NewServer(newTestCityHandler(t, fs))
	t.Cleanup(server.Close)
	return fs, &services, sessionInfoForConnectedClient{
		ID: sessionInfo.ID,
	}, server.URL, server.Client()
}

type sessionInfoForConnectedClient struct {
	ID string
}

type connectedClientRegistration struct {
	ClientID string `json:"client_id"`
	Token    string `json:"token"`
	Created  bool   `json:"created"`
}

func registerConnectedClient(t *testing.T, client *http.Client, baseURL string) connectedClientRegistration {
	t.Helper()

	body := map[string]any{
		"credential": "connected-client-e2e-credential",
	}
	data, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal registration body: %v", err)
	}
	req, err := http.NewRequest(http.MethodPost, baseURL+"/v0/extmsg/clients", bytes.NewReader(data))
	if err != nil {
		t.Fatalf("new registration request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-GC-Request", "true")

	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("POST /v0/extmsg/clients: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("POST /v0/extmsg/clients status = %d, want 200; body=%s", resp.StatusCode, body)
	}
	var out connectedClientRegistration
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("decode registration response: %v", err)
	}
	if out.ClientID == "" || out.Token == "" {
		t.Fatalf("registration response missing client_id/token: %+v", out)
	}
	return out
}

func bindConnectedClientConversation(t *testing.T, services *extmsg.Services, ref extmsg.ConversationRef, sessionID string) {
	t.Helper()

	caller := extmsg.Caller{Kind: extmsg.CallerController, ID: "connected-client-e2e"}
	if _, err := services.Bindings.Bind(context.Background(), caller, extmsg.BindInput{
		Conversation: ref,
		SessionID:    sessionID,
		Now:          time.Now().UTC(),
	}); err != nil {
		t.Fatalf("bind connected-client conversation: %v", err)
	}
}

func openConnectedClientSubscribe(t *testing.T, client *http.Client, baseURL string, reg connectedClientRegistration, lastEventID string) (*http.Response, context.CancelFunc) {
	t.Helper()

	ctx, cancel := context.WithCancel(context.Background())
	subscribeURL := fmt.Sprintf("%s/v0/extmsg/clients/%s/conversations/%s/subscribe",
		baseURL,
		url.PathEscape(reg.ClientID),
		url.PathEscape(connectedClientConversationID),
	)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, subscribeURL, nil)
	if err != nil {
		cancel()
		t.Fatalf("new subscribe request: %v", err)
	}
	req.Header.Set("Accept", "text/event-stream")
	req.Header.Set("X-GC-Client-Token", reg.Token)
	if lastEventID != "" {
		req.Header.Set("Last-Event-ID", lastEventID)
	}

	resp, err := client.Do(req)
	if err != nil {
		cancel()
		t.Fatalf("GET connected-client subscribe: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		cancel()
		t.Fatalf("GET connected-client subscribe status = %d, want 200; body=%s", resp.StatusCode, body)
	}
	if got := resp.Header.Get("Content-Type"); !strings.HasPrefix(got, "text/event-stream") {
		_ = resp.Body.Close()
		cancel()
		t.Fatalf("subscribe Content-Type = %q, want text/event-stream", got)
	}
	return resp, cancel
}

func postConnectedClientInboundTurn(t *testing.T, client *http.Client, baseURL string, reg connectedClientRegistration, conversationID, text string) {
	t.Helper()

	body := map[string]any{
		"provider":        connectedClientProvider,
		"account_id":      reg.ClientID,
		"conversation_id": conversationID,
		"kind":            "dm",
		"actor": map[string]any{
			"id":           "client-test-user",
			"display_name": "Connected Client",
			"is_bot":       false,
		},
		"text": text,
	}
	_ = postConnectedClientJSON(t, client, http.MethodPost, baseURL+"/v0/extmsg/inbound", reg.Token, body, http.StatusAccepted)
}

type connectedClientOutboundResult struct {
	Receipt struct {
		MessageID    string                 `json:"MessageID"`
		Conversation extmsg.ConversationRef `json:"Conversation"`
		Delivered    bool                   `json:"Delivered"`
		FailureKind  string                 `json:"FailureKind"`
		Metadata     map[string]string      `json:"Metadata"`
	} `json:"Receipt"`
	TranscriptEntry *struct {
		Sequence        int64  `json:"Sequence"`
		Text            string `json:"Text"`
		SourceSessionID string `json:"SourceSessionID"`
	} `json:"TranscriptEntry"`
}

func postConnectedClientSessionReply(t *testing.T, client *http.Client, baseURL string, fs *fakeState, sessionID string, ref extmsg.ConversationRef, text string) connectedClientOutboundResult {
	t.Helper()

	body := map[string]any{
		"session_id":      sessionID,
		"conversation":    ref,
		"text":            text,
		"idempotency_key": "connected-client-e2e:" + text,
	}
	respBody := postConnectedClientJSON(t, client, http.MethodPost, baseURL+cityURL(fs, "/extmsg/outbound"), "", body, http.StatusOK)

	var out connectedClientOutboundResult
	if err := json.Unmarshal(respBody, &out); err != nil {
		t.Fatalf("decode outbound response: %v\nbody=%s", err, respBody)
	}
	return out
}

func postConnectedClientJSON(t *testing.T, client *http.Client, method, rawURL, token string, body any, wantStatus int) []byte {
	t.Helper()

	data, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal request body: %v", err)
	}
	req, err := http.NewRequest(method, rawURL, bytes.NewReader(data))
	if err != nil {
		t.Fatalf("new %s request: %v", method, err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-GC-Request", "true")
	if token != "" {
		req.Header.Set("X-GC-Client-Token", token)
	}

	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, rawURL, err)
	}
	defer func() { _ = resp.Body.Close() }()
	respBody, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != wantStatus {
		t.Fatalf("%s %s status = %d, want %d; body=%s", method, rawURL, resp.StatusCode, wantStatus, respBody)
	}
	return respBody
}

func connectedClientConversationRef(accountID, conversationID string) extmsg.ConversationRef {
	return extmsg.ConversationRef{
		Provider:       connectedClientProvider,
		AccountID:      accountID,
		ConversationID: conversationID,
		Kind:           extmsg.ConversationDM,
	}
}

type connectedClientSSEFrame struct {
	ID    string
	HasID bool
	Event string
	Data  string
	Retry string
}

func readConnectedClientSSEFrame(t *testing.T, resp *http.Response) connectedClientSSEFrame {
	t.Helper()

	type result struct {
		frame connectedClientSSEFrame
		err   error
	}
	ch := make(chan result, 1)
	go func() {
		frame, err := parseConnectedClientSSEFrame(resp.Body)
		ch <- result{frame: frame, err: err}
	}()

	select {
	case res := <-ch:
		if res.err != nil {
			t.Fatalf("read SSE frame: %v", res.err)
		}
		return res.frame
	case <-time.After(2 * time.Second):
		_ = resp.Body.Close()
		t.Fatal("timed out waiting for connected-client SSE frame")
		return connectedClientSSEFrame{}
	}
}

func parseConnectedClientSSEFrame(r io.Reader) (connectedClientSSEFrame, error) {
	reader := bufio.NewReader(r)
	var frame connectedClientSSEFrame
	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			return frame, err
		}
		line = strings.TrimSuffix(strings.TrimSuffix(line, "\n"), "\r")
		if line == "" {
			return frame, nil
		}
		switch {
		case strings.HasPrefix(line, "id:"):
			frame.ID = strings.TrimSpace(strings.TrimPrefix(line, "id:"))
			frame.HasID = true
		case strings.HasPrefix(line, "event:"):
			frame.Event = strings.TrimSpace(strings.TrimPrefix(line, "event:"))
		case strings.HasPrefix(line, "data:"):
			if frame.Data != "" {
				frame.Data += "\n"
			}
			frame.Data += strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		case strings.HasPrefix(line, "retry:"):
			frame.Retry = strings.TrimSpace(strings.TrimPrefix(line, "retry:"))
		}
	}
}

func decodeConnectedClientSSEData(t *testing.T, frame connectedClientSSEFrame) map[string]any {
	t.Helper()

	var payload map[string]any
	if err := json.Unmarshal([]byte(frame.Data), &payload); err != nil {
		t.Fatalf("decode SSE data %q: %v", frame.Data, err)
	}
	return payload
}

type connectedClientMessageWant struct {
	Text           string
	SessionID      string
	AccountID      string
	ConversationID string
}

func assertConnectedClientMessagePayload(t *testing.T, payload map[string]any, want connectedClientMessageWant) {
	t.Helper()

	for key, wantValue := range map[string]string{
		"version":    "1",
		"event":      "message",
		"text":       want.Text,
		"session_id": want.SessionID,
	} {
		if got, _ := payload[key].(string); got != wantValue {
			t.Fatalf("payload[%q] = %q, want %q; payload=%#v", key, got, wantValue, payload)
		}
	}
	sequence, ok := payload["sequence"].(float64)
	if !ok || sequence <= 0 {
		t.Fatalf("payload sequence = %#v, want positive integer; payload=%#v", payload["sequence"], payload)
	}
	if got, _ := payload["created_at"].(string); got == "" {
		t.Fatalf("payload created_at missing; payload=%#v", payload)
	}
	conversation, ok := payload["conversation"].(map[string]any)
	if !ok {
		t.Fatalf("payload conversation = %#v, want object; payload=%#v", payload["conversation"], payload)
	}
	for key, wantValue := range map[string]string{
		"provider":        connectedClientProvider,
		"account_id":      want.AccountID,
		"conversation_id": want.ConversationID,
	} {
		if got, _ := conversation[key].(string); got != wantValue {
			t.Fatalf("conversation[%q] = %q, want %q; conversation=%#v", key, got, wantValue, conversation)
		}
	}
}

func assertOpenAPIOperation(t *testing.T, paths map[string]any, method, path string) {
	t.Helper()

	item, ok := paths[path].(map[string]any)
	if !ok {
		t.Fatalf("OpenAPI path %s missing", path)
	}
	if _, ok := item[strings.ToLower(method)]; !ok {
		t.Fatalf("OpenAPI path %s missing %s operation", path, method)
	}
}
