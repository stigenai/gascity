package omnigent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestAPIClientSessionLifecycleUsesExternalWorkspaceAndTypedEvents(t *testing.T) {
	var mu sync.Mutex
	var requests []string
	server := newOmnigentHTTPTestServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		requests = append(requests, r.Method+" "+r.URL.Path)
		mu.Unlock()
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/health":
			writeClientTestJSON(t, w, http.StatusOK, map[string]any{"status": "ok"})
		case r.Method == http.MethodGet && r.URL.Path == "/v1/agents":
			writeClientTestJSON(t, w, http.StatusOK, map[string]any{
				"data":     []map[string]any{{"id": "ag_primary", "name": "claude-primary", "harness": "claude-sdk"}},
				"has_more": false,
			})
		case r.Method == http.MethodPost && r.URL.Path == "/v1/sessions":
			var body createSessionRequest
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Errorf("decode create: %v", err)
			}
			if body.AgentID != "ag_primary" || body.HostType != "external" {
				t.Errorf("create body = %#v, want external ag_primary", body)
			}
			if body.Workspace != "/work/assigned" || body.Git != nil || body.HostID != "host_local_123" {
				t.Errorf("create placement = %#v, want assigned workspace on supervised local host", body)
			}
			writeClientTestJSON(t, w, http.StatusCreated, map[string]any{
				"id": "conv_abc123", "agent_id": "ag_primary", "agent_name": "claude-primary",
				"status": "idle", "workspace": body.Workspace, "labels": body.Labels, "items": []any{},
			})
		case r.Method == http.MethodGet && r.URL.Path == "/v1/sessions/conv_abc123":
			writeClientTestJSON(t, w, http.StatusOK, map[string]any{
				"id": "conv_abc123", "agent_id": "ag_primary", "agent_name": "claude-primary",
				"status": "idle", "workspace": "/work/assigned",
				"labels": map[string]string{"gascity.profile": "claude-primary"}, "items": []any{},
			})
		case r.Method == http.MethodPost && r.URL.Path == "/v1/sessions/conv_abc123/events":
			var body sessionEventRequest
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Errorf("decode event: %v", err)
			}
			if body.Type != "message" || body.Data.Role != "user" || len(body.Data.Content) != 1 || body.Data.Content[0].Text != "hello" {
				t.Errorf("event body = %#v, want typed user message", body)
			}
			writeClientTestJSON(t, w, http.StatusAccepted, map[string]any{"queued": true})
		case r.Method == http.MethodPatch && r.URL.Path == "/v1/sessions/conv_abc123":
			writeClientTestJSON(t, w, http.StatusOK, map[string]any{
				"id": "conv_abc123", "agent_id": "ag_primary", "status": "idle",
				"workspace": "/work/assigned", "labels": map[string]string{"gascity.profile.active": "1"}, "items": []any{},
			})
		case r.Method == http.MethodPost && r.URL.Path == "/v1/sessions/conv_abc123/switch-agent":
			writeClientTestJSON(t, w, http.StatusOK, map[string]any{
				"id": "conv_abc123", "agent_id": "ag_backup", "agent_name": "claude-secondary",
				"status": "idle", "workspace": "/work/assigned", "labels": map[string]string{}, "items": []any{},
			})
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	client, err := NewAPIClient(server.URL, server.Client())
	if err != nil {
		t.Fatal(err)
	}
	client, err = client.withLocalHost("host_local_123")
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if err := client.Health(ctx); err != nil {
		t.Fatalf("Health: %v", err)
	}
	agents, err := client.ListAgents(ctx)
	if err != nil || len(agents) != 1 || agents[0].Name != "claude-primary" {
		t.Fatalf("ListAgents = %#v, %v", agents, err)
	}
	session, err := client.CreateSession(ctx, CreateSessionInput{
		AgentID: "ag_primary", Workspace: "/work/assigned", Title: "worker",
		Labels: map[string]string{"gascity.profile": "claude-primary"},
	})
	if err != nil || session.ID != "conv_abc123" {
		t.Fatalf("CreateSession = %#v, %v", session, err)
	}
	if _, err := client.GetSession(ctx, session.ID); err != nil {
		t.Fatalf("GetSession: %v", err)
	}
	queued, err := client.PostMessage(ctx, session.ID, "hello")
	if err != nil || !queued {
		t.Fatalf("PostMessage = %v, %v", queued, err)
	}
	if _, err := client.UpdateLabels(ctx, session.ID, map[string]string{"gascity.profile.active": "1"}); err != nil {
		t.Fatalf("UpdateLabels: %v", err)
	}
	if _, err := client.SwitchAgent(ctx, session.ID, "ag_backup"); err != nil {
		t.Fatalf("SwitchAgent: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(requests) != 7 {
		t.Fatalf("requests = %v, want 7 operations", requests)
	}
}

func TestNormalizeWorkspaceResolvesExistingSymlink(t *testing.T) {
	realRoot := t.TempDir()
	linkRoot := filepath.Join(t.TempDir(), "workspace-link")
	if err := os.Symlink(realRoot, linkRoot); err != nil {
		t.Fatal(err)
	}
	got, err := normalizeWorkspace(linkRoot)
	if err != nil {
		t.Fatal(err)
	}
	want, err := filepath.EvalSymlinks(realRoot)
	if err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatalf("normalizeWorkspace = %q, want %q", got, want)
	}
}

type blockingEventBody struct {
	started   chan struct{}
	closed    chan struct{}
	startOnce sync.Once
	closeOnce sync.Once
}

func (b *blockingEventBody) Read([]byte) (int, error) {
	b.startOnce.Do(func() { close(b.started) })
	<-b.closed
	return 0, errors.New("closed")
}

func (b *blockingEventBody) Close() error {
	b.closeOnce.Do(func() { close(b.closed) })
	return nil
}

func TestEventStreamCloseSafelyUnblocksConcurrentConsumer(t *testing.T) {
	body := &blockingEventBody{started: make(chan struct{}), closed: make(chan struct{})}
	stream := &EventStream{body: body}
	done := make(chan error, 1)
	go func() {
		done <- stream.Consume(context.Background(), func(StreamEvent) error { return nil })
	}()
	select {
	case <-body.started:
	case <-time.After(time.Second):
		t.Fatal("consumer did not start")
	}
	if err := stream.Close(); err != nil {
		t.Fatal(err)
	}
	if err := stream.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
	select {
	case err := <-done:
		if err == nil || !strings.Contains(err.Error(), "closed") {
			t.Fatalf("Consume error = %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("consumer remained blocked after Close")
	}
}

func TestObserveFailoverForwardsOnlyBoundedMachineClassifiers(t *testing.T) {
	server := newOmnigentHTTPTestServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/gascity/v1/failover" {
			http.NotFound(w, r)
			return
		}
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		encoded, _ := json.Marshal(body)
		for _, forbidden := range []string{"provider prose secret", "message", "authorization", "credential"} {
			if strings.Contains(strings.ToLower(string(encoded)), forbidden) {
				t.Fatalf("observation leaked %q: %s", forbidden, encoded)
			}
		}
		if body["conversation_id"] != "conv_safe" || body["error_code"] != "429" || body["source"] != "llm" {
			t.Fatalf("observation = %s", encoded)
		}
		writeClientTestJSON(t, w, http.StatusOK, FailoverObservationResult{Ignored: true})
	}))
	defer server.Close()
	client, err := NewAPIClient(server.URL, server.Client())
	if err != nil {
		t.Fatal(err)
	}
	sequence := int64(9)
	result, err := client.ObserveFailover(context.Background(), "conv_safe", 0, StreamEvent{
		Type: "response.error", Source: "llm", SequenceNumber: &sequence,
		Error: &StreamError{Code: "429", Message: "provider prose secret with credential"},
	})
	if err != nil || !result.Ignored {
		t.Fatalf("result=%#v error=%v", result, err)
	}
}

func TestLocalStatusRejectsForbiddenRemoteMode(t *testing.T) {
	server := newOmnigentHTTPTestServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		writeClientTestJSON(t, w, http.StatusOK, LocalStatus{Mode: "hosted", Ready: true})
	}))
	defer server.Close()
	client, err := NewAPIClient(server.URL, server.Client())
	if err != nil {
		t.Fatal(err)
	}
	_, err = client.LocalStatus(context.Background(), "")
	if err == nil || !strings.Contains(err.Error(), "forbidden mode") || !strings.Contains(err.Error(), "local") {
		t.Fatalf("error = %v", err)
	}
}

func TestAPIClientStreamParsesOrderedEventsAndDone(t *testing.T) {
	server := newOmnigentHTTPTestServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/sessions/conv_stream/stream" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		if _, err := fmt.Fprint(w,
			"event: session.status\n",
			"data: {\"type\":\"session.status\",\"conversation_id\":\"conv_stream\",\"status\":\"running\"}\n\n",
			"event: response.output_text.delta\n",
			"data: {\"type\":\"response.output_text.delta\",\"delta\":\"hello\"}\n\n",
			"event: response.error\n",
			"data: {\"type\":\"response.error\",\"source\":\"llm\",\"error\":{\"code\":\"rate_limit\",\"message\":\"limited\"}}\n\n",
			"data: [DONE]\n\n",
		); err != nil {
			t.Errorf("write SSE fixture: %v", err)
		}
	}))
	defer server.Close()
	client, err := NewAPIClient(server.URL, server.Client())
	if err != nil {
		t.Fatal(err)
	}
	var got []StreamEvent
	err = client.ConsumeStream(context.Background(), "conv_stream", func(event StreamEvent) error {
		got = append(got, event)
		return nil
	})
	if err != nil {
		t.Fatalf("ConsumeStream: %v", err)
	}
	if len(got) != 3 || got[0].Status != "running" || got[1].Delta != "hello" {
		t.Fatalf("events = %#v", got)
	}
	if got[2].Error == nil || got[2].Error.Code != "rate_limit" || got[2].Source != "llm" {
		t.Fatalf("error event = %#v", got[2])
	}
}

func TestAPIClientControlIsTypedRepeatableAndContextBound(t *testing.T) {
	var mu sync.Mutex
	var controls []string
	server := newOmnigentHTTPTestServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request sessionEventRequest
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Errorf("decode control: %v", err)
		}
		mu.Lock()
		controls = append(controls, request.Type)
		mu.Unlock()
		writeClientTestJSON(t, w, http.StatusAccepted, map[string]bool{"queued": false})
	}))
	defer server.Close()
	client, err := NewAPIClient(server.URL, server.Client())
	if err != nil {
		t.Fatal(err)
	}
	for _, control := range []string{"interrupt", "stop_session", "stop_session"} {
		if err := client.PostControl(context.Background(), "conv_control", control); err != nil {
			t.Fatalf("PostControl(%q): %v", control, err)
		}
	}
	if err := client.PostControl(context.Background(), "conv_control", "delete"); err == nil {
		t.Fatal("unsupported destructive control succeeded")
	}
	mu.Lock()
	defer mu.Unlock()
	if strings.Join(controls, ",") != "interrupt,stop_session,stop_session" {
		t.Fatalf("controls = %v", controls)
	}

	hanging := newOmnigentHTTPTestServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-time.After(200 * time.Millisecond):
		}
	}))
	defer func() {
		hanging.CloseClientConnections()
		hanging.Close()
	}()
	hangingClient, err := NewAPIClient(hanging.URL, hanging.Client())
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Millisecond)
	defer cancel()
	if err := hangingClient.PostControl(ctx, "conv_control", "interrupt"); err == nil || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("hanging control error = %v", err)
	}
}

func TestAPIClientErrorsAreTypedBoundedAndDoNotExposeResponseBody(t *testing.T) {
	server := newOmnigentHTTPTestServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		writeClientTestJSON(t, w, http.StatusUnauthorized, map[string]any{
			"error": map[string]any{"code": "unauthorized", "message": strings.Repeat("x", 1000)},
		})
	}))
	defer server.Close()
	client, err := NewAPIClient(server.URL, server.Client())
	if err != nil {
		t.Fatal(err)
	}
	_, err = client.GetSession(context.Background(), "conv_missing")
	apiErr := &APIError{}
	ok := errors.As(err, &apiErr)
	if !ok {
		t.Fatalf("error = %T %v, want *APIError", err, err)
	}
	if apiErr.StatusCode != http.StatusUnauthorized || apiErr.Code != "unauthorized" {
		t.Fatalf("APIError = %#v", apiErr)
	}
	if len(apiErr.Message) > maxErrorMessageBytes+3 {
		t.Fatalf("APIError message len = %d, want bounded", len(apiErr.Message))
	}
}

func TestAPIClientRedactsSecretsAtHTTPAndStreamEdges(t *testing.T) {
	const sentinel = "SENTINEL-OMNIGENT-SECRET"
	server := newOmnigentHTTPTestServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/health":
			writeClientTestJSON(t, w, http.StatusBadGateway, map[string]any{"error": map[string]string{
				"code":    "backend_unavailable",
				"message": "token=" + sentinel + " https://operator:" + sentinel + "@backend.example/v1",
			}})
		case "/v1/sessions/conv_redacted/stream":
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = fmt.Fprintf(w, "data: {\"type\":\"response.error\",\"error\":{\"code\":\"auth_failure\",\"message\":\"bearer %s\"}}\n\ndata: [DONE]\n\n", sentinel)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	client, err := NewAPIClient(server.URL, server.Client())
	if err != nil {
		t.Fatal(err)
	}
	if err := client.Health(context.Background()); err == nil || strings.Contains(err.Error(), sentinel) || !strings.Contains(err.Error(), "[redacted]") {
		t.Fatalf("HTTP error = %v", err)
	}
	stream, err := client.Subscribe(context.Background(), "conv_redacted")
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := stream.Close(); err != nil {
			t.Errorf("close stream: %v", err)
		}
	}()
	if err := stream.Consume(context.Background(), func(event StreamEvent) error {
		if event.Error == nil || strings.Contains(event.Error.Message, sentinel) || !strings.Contains(event.Error.Message, "[redacted]") {
			t.Fatalf("stream error = %#v", event.Error)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestAPIClientResolveProfileValidatesPublicDiscovery(t *testing.T) {
	profiles := []PublicProfile{
		{
			ID: "claude-primary", DisplayName: "Claude primary", Blurb: "Primary compatible backend.",
			Harness: "claude-sdk", Backend: "primary-gateway", Network: "external-model",
			Availability: "available", Chain: []string{"claude-primary", "claude-secondary"},
		},
		{
			ID: "claude-secondary", DisplayName: "Claude secondary", Blurb: "Independent backup backend.",
			Harness: "claude-sdk", Backend: "backup-gateway", Network: "external-model",
			Availability: "unavailable", Chain: []string{"claude-secondary"},
		},
	}
	server := newOmnigentHTTPTestServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/gascity/v1/profiles" {
			http.NotFound(w, r)
			return
		}
		writeClientTestJSON(t, w, http.StatusOK, profiles)
	}))
	defer server.Close()
	client, err := NewAPIClient(server.URL, server.Client())
	if err != nil {
		t.Fatal(err)
	}

	profile, err := client.ResolveProfile(context.Background(), "claude-primary")
	if err != nil || profile.ID != "claude-primary" || len(profile.Chain) != 2 {
		t.Fatalf("ResolveProfile(primary) = %#v, %v", profile, err)
	}
	if _, err := client.ResolveProfile(context.Background(), "claude-secondary"); err == nil || !strings.Contains(err.Error(), "unavailable") {
		t.Fatalf("ResolveProfile(unavailable) error = %v", err)
	}
	if _, err := client.ResolveProfile(context.Background(), "missing"); err == nil || !strings.Contains(err.Error(), "unknown") {
		t.Fatalf("ResolveProfile(missing) error = %v", err)
	}
}

func TestAPIClientListProfilesRejectsMalformedDiscovery(t *testing.T) {
	valid := PublicProfile{
		ID: "codex", DisplayName: "Codex", Blurb: "Codex through local authentication.",
		Harness: "codex", Backend: "openai", Network: "external-model",
		Availability: "available", Chain: []string{"codex"},
	}
	tests := []struct {
		name     string
		profiles []PublicProfile
		want     string
	}{
		{name: "duplicate id", profiles: []PublicProfile{valid, valid}, want: "duplicate profile"},
		{name: "empty metadata", profiles: []PublicProfile{{ID: "codex", Harness: "codex", Backend: "openai", Network: "external-model", Availability: "available", Chain: []string{"codex"}}}, want: "display_name"},
		{name: "unsupported harness", profiles: []PublicProfile{{ID: "codex", DisplayName: "Codex", Blurb: "Profile", Harness: "remote", Backend: "openai", Network: "external-model", Availability: "available", Chain: []string{"codex"}}}, want: "harness"},
		{name: "unsupported network", profiles: []PublicProfile{{ID: "codex", DisplayName: "Codex", Blurb: "Profile", Harness: "codex", Backend: "openai", Network: "hosted", Availability: "available", Chain: []string{"codex"}}}, want: "network"},
		{name: "unsupported availability", profiles: []PublicProfile{{ID: "codex", DisplayName: "Codex", Blurb: "Profile", Harness: "codex", Backend: "openai", Network: "external-model", Availability: "maybe", Chain: []string{"codex"}}}, want: "availability"},
		{name: "chain does not start at profile", profiles: []PublicProfile{{ID: "codex", DisplayName: "Codex", Blurb: "Profile", Harness: "codex", Backend: "openai", Network: "external-model", Availability: "available", Chain: []string{"other"}}}, want: "chain"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			server := newOmnigentHTTPTestServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				writeClientTestJSON(t, w, http.StatusOK, tt.profiles)
			}))
			defer server.Close()
			client, err := NewAPIClient(server.URL, server.Client())
			if err != nil {
				t.Fatal(err)
			}
			if _, err := client.ListProfiles(context.Background()); err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("ListProfiles error = %v, want %q", err, tt.want)
			}
		})
	}
}

func TestNewAPIClientRejectsNonLoopbackEndpoint(t *testing.T) {
	for _, endpoint := range []string{
		"https://omnigent.example.com", "http://0.0.0.0:6767", "http://192.0.2.1:6767", "file:///tmp/x",
		"http://user:pass@127.0.0.1:6767/svc", "http://127.0.0.1:6767/svc?token=x", "http://127.0.0.1:6767/svc#fragment",
		"http://127.0.0.1:6767/svc/../other", "http://127.0.0.1:6767/svc/%2e%2e/other",
	} {
		if _, err := NewAPIClient(endpoint, http.DefaultClient); err == nil {
			t.Errorf("NewAPIClient(%q) succeeded, want local-only rejection", endpoint)
		}
	}
}

func TestNewAPIClientPreservesSafeLoopbackPathPrefix(t *testing.T) {
	var gotPath string
	server := newOmnigentHTTPTestServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		writeClientTestJSON(t, w, http.StatusOK, map[string]string{"status": "ok"})
	}))
	defer server.Close()
	client, err := NewAPIClient(server.URL+"/v0/city/bright%20lights/svc/omnigent", server.Client())
	if err != nil {
		t.Fatal(err)
	}
	if err := client.Health(context.Background()); err != nil {
		t.Fatal(err)
	}
	if gotPath != "/v0/city/bright lights/svc/omnigent/health" {
		t.Fatalf("path = %q", gotPath)
	}
}

func TestNewUnixAPIClientUsesOnlyConfiguredSocket(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Unix socket unavailable")
	}
	// Darwin's sockaddr_un path is limited to 104 bytes, while t.TempDir's
	// test-derived path can exceed that before the socket name is appended.
	socketDir, err := os.MkdirTemp("", "og-sock-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(socketDir) })
	socket := filepath.Join(socketDir, "api.sock")
	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	server := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/health" {
			http.NotFound(w, r)
			return
		}
		writeClientTestJSON(t, w, http.StatusOK, map[string]string{"status": "ok"})
	})}
	done := make(chan error, 1)
	go func() { done <- server.Serve(listener) }()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = server.Shutdown(ctx)
		<-done
	})
	client, err := NewUnixAPIClient(socket)
	if err != nil {
		t.Fatal(err)
	}
	if err := client.Health(context.Background()); err != nil {
		t.Fatalf("Health over Unix socket: %v", err)
	}
}

func writeClientTestJSON(t *testing.T, w http.ResponseWriter, status int, value any) {
	t.Helper()
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(value); err != nil {
		t.Errorf("encode response: %v", err)
	}
}
