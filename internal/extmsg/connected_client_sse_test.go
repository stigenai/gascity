package extmsg

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestConnectedClientSSEEventRoundTrip(t *testing.T) {
	t.Parallel()

	createdAt := time.Date(2026, 6, 19, 19, 32, 39, 0, time.UTC)
	heartbeatAt := time.Date(2026, 6, 19, 19, 45, 0, 0, time.UTC)
	retryAfter := int64(5000)

	message := SSEMessageEvent{
		Version:   "1",
		Event:     "message",
		Text:      "Hello from the session.",
		SessionID: "s-abc123",
		Conversation: ConversationRef{
			ScopeID:        "scope-a",
			Provider:       "llm-client",
			AccountID:      "client-bead-1",
			ConversationID: "conv-uuid-1",
			Kind:           ConversationThread,
		},
		Sequence:  42,
		CreatedAt: createdAt,
	}
	messageBody := mustMarshalConnectedClientSSEPayload(t, message)
	assertConnectedClientSSEFields(t, messageBody, map[string]any{
		"version":    "1",
		"event":      "message",
		"text":       "Hello from the session.",
		"session_id": "s-abc123",
		"sequence":   float64(42),
		"created_at": "2026-06-19T19:32:39Z",
	})
	messageConversation := mustJSONMap(t, mustJSONMap(t, messageBody)["conversation"])
	for _, field := range []string{"provider", "account_id", "conversation_id"} {
		if _, ok := messageConversation[field]; !ok {
			t.Fatalf("message conversation missing %q: %s", field, messageBody)
		}
	}
	var decodedMessage SSEMessageEvent
	mustUnmarshalConnectedClientSSEPayload(t, messageBody, &decodedMessage)
	if decodedMessage.Version != "1" || decodedMessage.Event != "message" || decodedMessage.Sequence != 42 {
		t.Fatalf("decoded message = %#v, want version/event/sequence preserved", decodedMessage)
	}
	if decodedMessage.Text != message.Text || decodedMessage.SessionID != message.SessionID {
		t.Fatalf("decoded message text/session_id = %q/%q, want %q/%q", decodedMessage.Text, decodedMessage.SessionID, message.Text, message.SessionID)
	}
	if decodedMessage.Conversation.Provider != "llm-client" || decodedMessage.Conversation.AccountID != "client-bead-1" || decodedMessage.Conversation.ConversationID != "conv-uuid-1" {
		t.Fatalf("decoded message conversation = %#v", decodedMessage.Conversation)
	}
	if !decodedMessage.CreatedAt.Equal(createdAt) {
		t.Fatalf("decoded message CreatedAt = %s, want %s", decodedMessage.CreatedAt, createdAt)
	}

	heartbeat := SSEHeartbeatEvent{
		Version: "1",
		Event:   "heartbeat",
		TS:      heartbeatAt,
	}
	heartbeatBody := mustMarshalConnectedClientSSEPayload(t, heartbeat)
	assertConnectedClientSSEFields(t, heartbeatBody, map[string]any{
		"version": "1",
		"event":   "heartbeat",
		"ts":      "2026-06-19T19:45:00Z",
	})
	var decodedHeartbeat SSEHeartbeatEvent
	mustUnmarshalConnectedClientSSEPayload(t, heartbeatBody, &decodedHeartbeat)
	if decodedHeartbeat.Version != "1" || decodedHeartbeat.Event != "heartbeat" || !decodedHeartbeat.TS.Equal(heartbeatAt) {
		t.Fatalf("decoded heartbeat = %#v, want payload preserved", decodedHeartbeat)
	}

	retryableError := SSEErrorEvent{
		Version:      "1",
		Event:        "error",
		Code:         "session_stopped",
		Message:      "Session stopped.",
		Retryable:    true,
		RetryAfterMs: &retryAfter,
	}
	retryableBody := mustMarshalConnectedClientSSEPayload(t, retryableError)
	assertConnectedClientSSEFields(t, retryableBody, map[string]any{
		"version":        "1",
		"event":          "error",
		"code":           "session_stopped",
		"message":        "Session stopped.",
		"retryable":      true,
		"retry_after_ms": float64(5000),
	})
	var decodedRetryable SSEErrorEvent
	mustUnmarshalConnectedClientSSEPayload(t, retryableBody, &decodedRetryable)
	if decodedRetryable.RetryAfterMs == nil || *decodedRetryable.RetryAfterMs != retryAfter {
		t.Fatalf("decoded retry_after_ms = %#v, want %d", decodedRetryable.RetryAfterMs, retryAfter)
	}

	terminalError := SSEErrorEvent{
		Version:   "1",
		Event:     "error",
		Code:      "token_revoked",
		Message:   "The client token has been revoked.",
		Retryable: false,
	}
	terminalBody := mustMarshalConnectedClientSSEPayload(t, terminalError)
	assertConnectedClientSSEFields(t, terminalBody, map[string]any{
		"version":   "1",
		"event":     "error",
		"code":      "token_revoked",
		"message":   "The client token has been revoked.",
		"retryable": false,
	})
	if _, ok := mustJSONMap(t, terminalBody)["retry_after_ms"]; ok {
		t.Fatalf("non-retryable error JSON must omit retry_after_ms: %s", terminalBody)
	}
	var decodedTerminal SSEErrorEvent
	mustUnmarshalConnectedClientSSEPayload(t, terminalBody, &decodedTerminal)
	if decodedTerminal.Retryable || decodedTerminal.RetryAfterMs != nil {
		t.Fatalf("decoded terminal error = %#v, want non-retryable with nil retry_after_ms", decodedTerminal)
	}
}

func TestConnectedClientSSEFrameIDRules(t *testing.T) {
	t.Parallel()

	messageFrame, err := FormatConnectedClientSSEMessage(SSEMessageEvent{
		Version:      "1",
		Event:        "message",
		Text:         "reply",
		SessionID:    "s-abc123",
		Conversation: connectedClientSSETestConversation(),
		Sequence:     42,
		CreatedAt:    time.Date(2026, 6, 19, 19, 32, 39, 0, time.UTC),
	})
	if err != nil {
		t.Fatalf("FormatConnectedClientSSEMessage: %v", err)
	}
	message := parseConnectedClientSSETestFrame(t, string(messageFrame))
	if !message.HasID || message.ID != "42" {
		t.Fatalf("message SSE id = %q (present=%v), want decimal sequence 42; frame:\n%s", message.ID, message.HasID, messageFrame)
	}
	if message.Event != "message" {
		t.Fatalf("message SSE event = %q, want message; frame:\n%s", message.Event, messageFrame)
	}
	if got := mustJSONMap(t, []byte(message.Data))["sequence"]; got != float64(42) {
		t.Fatalf("message data sequence = %#v, want 42; data=%s", got, message.Data)
	}

	heartbeatFrame, err := FormatConnectedClientSSEHeartbeat(SSEHeartbeatEvent{
		Version: "1",
		Event:   "heartbeat",
		TS:      time.Date(2026, 6, 19, 19, 45, 0, 0, time.UTC),
	})
	if err != nil {
		t.Fatalf("FormatConnectedClientSSEHeartbeat: %v", err)
	}
	heartbeat := parseConnectedClientSSETestFrame(t, string(heartbeatFrame))
	if heartbeat.HasID {
		t.Fatalf("heartbeat SSE id = %q, want omitted; frame:\n%s", heartbeat.ID, heartbeatFrame)
	}
	if heartbeat.Event != "heartbeat" {
		t.Fatalf("heartbeat SSE event = %q, want heartbeat; frame:\n%s", heartbeat.Event, heartbeatFrame)
	}

	retryAfter := int64(5000)
	errorFrame, err := FormatConnectedClientSSEError(SSEErrorEvent{
		Version:      "1",
		Event:        "error",
		Code:         "session_stopped",
		Message:      "Session stopped.",
		Retryable:    true,
		RetryAfterMs: &retryAfter,
	})
	if err != nil {
		t.Fatalf("FormatConnectedClientSSEError: %v", err)
	}
	errorEvent := parseConnectedClientSSETestFrame(t, string(errorFrame))
	if !errorEvent.HasID || errorEvent.ID != "error" {
		t.Fatalf("error SSE id = %q (present=%v), want literal error sentinel; frame:\n%s", errorEvent.ID, errorEvent.HasID, errorFrame)
	}
	if errorEvent.Event != "error" {
		t.Fatalf("error SSE event = %q, want error; frame:\n%s", errorEvent.Event, errorFrame)
	}
}

func TestConnectedClientLastEventIDParsing(t *testing.T) {
	t.Parallel()

	for _, tt := range []struct {
		name     string
		header   string
		wantSeq  int64
		wantOkay bool
	}{
		{name: "absent", header: "", wantOkay: false},
		{name: "numeric cursor", header: "42", wantSeq: 42, wantOkay: true},
		{name: "zero padded numeric cursor", header: "00042", wantSeq: 42, wantOkay: true},
		{name: "error sentinel", header: "error", wantOkay: false},
		{name: "non numeric", header: "not-a-number", wantOkay: false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			gotSeq, gotOkay := ParseConnectedClientLastEventID(tt.header)
			if gotSeq != tt.wantSeq || gotOkay != tt.wantOkay {
				t.Fatalf("ParseConnectedClientLastEventID(%q) = (%d, %v), want (%d, %v)", tt.header, gotSeq, gotOkay, tt.wantSeq, tt.wantOkay)
			}
		})
	}
}

func TestConnectedClientSSEErrorRetryFraming(t *testing.T) {
	t.Parallel()

	retryAfter := int64(5000)
	retryableFrame, err := FormatConnectedClientSSEError(SSEErrorEvent{
		Version:      "1",
		Event:        "error",
		Code:         "session_stopped",
		Message:      "Session stopped.",
		Retryable:    true,
		RetryAfterMs: &retryAfter,
	})
	if err != nil {
		t.Fatalf("FormatConnectedClientSSEError(retryable): %v", err)
	}
	retryable := parseConnectedClientSSETestFrame(t, string(retryableFrame))
	if retryable.Retry != "5000" {
		t.Fatalf("retryable error retry line = %q, want 5000; frame:\n%s", retryable.Retry, retryableFrame)
	}
	assertConnectedClientLineBefore(t, retryable.Lines, "retry: 5000", "event: error")
	retryableData := mustJSONMap(t, []byte(retryable.Data))
	if retryableData["retryable"] != true || retryableData["retry_after_ms"] != float64(5000) {
		t.Fatalf("retryable error data = %#v, want retryable true with retry_after_ms 5000", retryableData)
	}

	zeroRetryAfter := int64(0)
	immediateRetryFrame, err := FormatConnectedClientSSEError(SSEErrorEvent{
		Version:      "1",
		Event:        "error",
		Code:         "idle_timeout",
		Message:      "Idle timeout.",
		Retryable:    true,
		RetryAfterMs: &zeroRetryAfter,
	})
	if err != nil {
		t.Fatalf("FormatConnectedClientSSEError(immediate retry): %v", err)
	}
	immediateRetry := parseConnectedClientSSETestFrame(t, string(immediateRetryFrame))
	if immediateRetry.Retry != "0" {
		t.Fatalf("retryable zero-delay error retry line = %q, want 0; frame:\n%s", immediateRetry.Retry, immediateRetryFrame)
	}

	terminalFrame, err := FormatConnectedClientSSEError(SSEErrorEvent{
		Version:   "1",
		Event:     "error",
		Code:      "token_revoked",
		Message:   "The client token has been revoked.",
		Retryable: false,
	})
	if err != nil {
		t.Fatalf("FormatConnectedClientSSEError(non-retryable): %v", err)
	}
	terminal := parseConnectedClientSSETestFrame(t, string(terminalFrame))
	if terminal.Retry != "" {
		t.Fatalf("non-retryable error retry line = %q, want omitted; frame:\n%s", terminal.Retry, terminalFrame)
	}
	if _, ok := mustJSONMap(t, []byte(terminal.Data))["retry_after_ms"]; ok {
		t.Fatalf("non-retryable error data must omit retry_after_ms: %s", terminal.Data)
	}
}

type connectedClientSSETestFrame struct {
	ID    string
	HasID bool
	Event string
	Data  string
	Retry string
	Lines []string
}

func connectedClientSSETestConversation() ConversationRef {
	return ConversationRef{
		ScopeID:        "scope-a",
		Provider:       "llm-client",
		AccountID:      "client-bead-1",
		ConversationID: "conv-uuid-1",
		Kind:           ConversationThread,
	}
}

func mustMarshalConnectedClientSSEPayload(t *testing.T, payload any) []byte {
	t.Helper()

	body, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("marshal %T: %v", payload, err)
	}
	return body
}

func mustUnmarshalConnectedClientSSEPayload(t *testing.T, body []byte, out any) {
	t.Helper()

	if err := json.Unmarshal(body, out); err != nil {
		t.Fatalf("unmarshal %T from %s: %v", out, body, err)
	}
}

func assertConnectedClientSSEFields(t *testing.T, body []byte, want map[string]any) {
	t.Helper()

	got := mustJSONMap(t, body)
	for key, wantValue := range want {
		if got[key] != wantValue {
			t.Fatalf("payload %s field %q = %#v, want %#v", body, key, got[key], wantValue)
		}
	}
}

func parseConnectedClientSSETestFrame(t *testing.T, raw string) connectedClientSSETestFrame {
	t.Helper()

	if !strings.HasSuffix(raw, "\n\n") {
		t.Fatalf("SSE frame must end with a blank line, got:\n%s", raw)
	}

	var frame connectedClientSSETestFrame
	for _, line := range strings.Split(strings.TrimSuffix(raw, "\n\n"), "\n") {
		frame.Lines = append(frame.Lines, line)
		switch {
		case strings.HasPrefix(line, "id: "):
			frame.ID = strings.TrimPrefix(line, "id: ")
			frame.HasID = true
		case strings.HasPrefix(line, "event: "):
			frame.Event = strings.TrimPrefix(line, "event: ")
		case strings.HasPrefix(line, "data: "):
			frame.Data = strings.TrimPrefix(line, "data: ")
		case strings.HasPrefix(line, "retry: "):
			frame.Retry = strings.TrimPrefix(line, "retry: ")
		}
	}
	if frame.Event == "" {
		t.Fatalf("SSE frame missing event line:\n%s", raw)
	}
	if frame.Data == "" {
		t.Fatalf("SSE frame missing data line:\n%s", raw)
	}
	return frame
}

func assertConnectedClientLineBefore(t *testing.T, lines []string, first, second string) {
	t.Helper()

	firstIndex := -1
	secondIndex := -1
	for i, line := range lines {
		if line == first {
			firstIndex = i
		}
		if line == second {
			secondIndex = i
		}
	}
	if firstIndex < 0 || secondIndex < 0 || firstIndex >= secondIndex {
		t.Fatalf("line order = %v, want %q before %q", lines, first, second)
	}
}

func mustJSONMap(t *testing.T, body any) map[string]any {
	t.Helper()

	switch typed := body.(type) {
	case []byte:
		var out map[string]any
		if err := json.Unmarshal(typed, &out); err != nil {
			t.Fatalf("decode JSON object from %s: %v", typed, err)
		}
		return out
	case map[string]any:
		return typed
	default:
		t.Fatalf("unsupported JSON map input %T", body)
		return nil
	}
}
