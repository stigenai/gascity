package omnigent

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

func TestOpenAttachmentFreshSubscribesBeforeSnapshotAndDetachPreservesConversation(t *testing.T) {
	catalog := loadFailoverCatalog(t)
	fake := newAttachmentAPIFake(t, initialFailoverLabels(t, catalog))
	client, err := NewAPIClient(fake.server.URL, fake.server.Client())
	if err != nil {
		t.Fatal(err)
	}
	attachment, err := OpenAttachment(context.Background(), client, catalog, AttachmentOpenInput{
		ProfileID: "claude-primary", Workspace: "/work/assigned", Title: "worker",
	})
	if err != nil {
		t.Fatalf("OpenAttachment: %v", err)
	}
	if !attachment.Fresh || attachment.ConversationID != "conv_attach" || attachment.State.ActiveProfileID != "claude-primary" {
		t.Fatalf("attachment = %#v", attachment)
	}
	if err := attachment.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	fake.mu.Lock()
	defer fake.mu.Unlock()
	if fake.creates != 1 || fake.destructiveRequests != 0 {
		t.Fatalf("creates=%d destructive=%d", fake.creates, fake.destructiveRequests)
	}
	if len(fake.operations) < 3 || fake.operations[0] != "agents" || fake.operations[1] != "create" || fake.operations[2] != "stream" {
		t.Fatalf("operations = %v, want stream established before snapshot", fake.operations)
	}
	for i, operation := range fake.operations {
		if operation == "snapshot" && i < 2 {
			t.Fatalf("snapshot preceded stream: %v", fake.operations)
		}
	}
}

func TestOpenAttachmentResumeNeverCreatesAndMissingFailsVisibly(t *testing.T) {
	catalog := loadFailoverCatalog(t)
	fake := newAttachmentAPIFake(t, initialFailoverLabels(t, catalog))
	fake.missing = true
	client, err := NewAPIClient(fake.server.URL, fake.server.Client())
	if err != nil {
		t.Fatal(err)
	}
	_, err = OpenAttachment(context.Background(), client, catalog, AttachmentOpenInput{
		ProfileID: "claude-primary", ConversationID: "conv_missing", Workspace: "/work/assigned",
	})
	apiErr := &APIError{}
	ok := errors.As(err, &apiErr)
	if !ok || apiErr.StatusCode != http.StatusNotFound {
		t.Fatalf("error = %T %v, want typed 404", err, err)
	}
	fake.mu.Lock()
	defer fake.mu.Unlock()
	if fake.creates != 0 {
		t.Fatalf("resume created %d replacement conversations", fake.creates)
	}
}

func TestResolveAttachmentRejectsMalformedStoredConversationBeforeRequest(t *testing.T) {
	requests := 0
	server := newOmnigentHTTPTestServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests++
		http.Error(w, "unexpected request", http.StatusInternalServerError)
	}))
	defer server.Close()
	client, err := NewAPIClient(server.URL, server.Client())
	if err != nil {
		t.Fatal(err)
	}
	_, err = client.ResolveAttachment(context.Background(), AttachmentOpenInput{
		ProfileID: "claude-primary", ConversationID: "malformed conversation/id", Workspace: "/work/assigned",
	})
	if err == nil || requests != 0 {
		t.Fatalf("error=%v requests=%d, want local validation failure", err, requests)
	}
}

func TestOpenAttachmentResumeUsesStoredChainAndRejectsWorkspaceOrArchiveMismatch(t *testing.T) {
	t.Run("stored chain survives catalog edit", func(t *testing.T) {
		catalog := loadFailoverCatalog(t)
		labels := initialFailoverLabels(t, catalog)
		primary := catalog.profiles["claude-primary"]
		primary.Fallbacks = nil
		catalog.profiles["claude-primary"] = primary
		fake := newAttachmentAPIFake(t, labels)
		client, err := NewAPIClient(fake.server.URL, fake.server.Client())
		if err != nil {
			t.Fatal(err)
		}
		attachment, err := OpenAttachment(context.Background(), client, catalog, AttachmentOpenInput{
			ProfileID: "claude-primary", ConversationID: "conv_attach", Workspace: "/work/assigned",
		})
		if err != nil {
			t.Fatalf("OpenAttachment: %v", err)
		}
		defer func() {
			if err := attachment.Close(); err != nil {
				t.Errorf("close attachment: %v", err)
			}
		}()
		if len(attachment.State.Chain) != 2 {
			t.Fatalf("stored chain = %#v", attachment.State.Chain)
		}
	})

	for _, tt := range []struct {
		name      string
		workspace string
		archived  bool
	}{
		{name: "workspace", workspace: "/work/other"},
		{name: "archived", workspace: "/work/assigned", archived: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			catalog := loadFailoverCatalog(t)
			fake := newAttachmentAPIFake(t, initialFailoverLabels(t, catalog))
			fake.workspace = tt.workspace
			fake.archived = tt.archived
			client, err := NewAPIClient(fake.server.URL, fake.server.Client())
			if err != nil {
				t.Fatal(err)
			}
			if _, err := OpenAttachment(context.Background(), client, catalog, AttachmentOpenInput{
				ProfileID: "claude-primary", ConversationID: "conv_attach", Workspace: "/work/assigned",
			}); err == nil {
				t.Fatal("OpenAttachment succeeded, want fail-closed mismatch")
			}
			fake.mu.Lock()
			defer fake.mu.Unlock()
			if fake.creates != 0 {
				t.Fatalf("mismatch created %d conversations", fake.creates)
			}
		})
	}
}

func TestAttachmentPaneRemainsSubscribedAcrossStickyFailover(t *testing.T) {
	catalog := loadFailoverCatalog(t)
	fake := newAttachmentAPIFake(t, initialFailoverLabels(t, catalog))
	client, err := NewAPIClient(fake.server.URL, fake.server.Client())
	if err != nil {
		t.Fatal(err)
	}
	attachment, err := OpenAttachment(context.Background(), client, catalog, AttachmentOpenInput{
		ProfileID: "claude-primary", ConversationID: "conv_attach", Workspace: "/work/assigned",
	})
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := attachment.Close(); err != nil {
			t.Errorf("close attachment: %v", err)
		}
	}()
	controller, err := NewFailoverController(client, catalog, time.Now)
	if err != nil {
		t.Fatal(err)
	}
	sequence := int64(70)
	result, err := controller.Advance(context.Background(), attachment.ConversationID, attachment.State.ActiveIndex, errorStreamEvent(&sequence, "429", 0))
	if err != nil {
		t.Fatalf("Advance: %v", err)
	}
	if result.Transition == nil || result.State.ActiveProfileID != "claude-secondary" {
		t.Fatalf("result = %#v", result)
	}
	if attachment.Stream == nil {
		t.Fatal("profile failover detached the operator stream")
	}
	fake.mu.Lock()
	defer fake.mu.Unlock()
	if fake.switches != 1 || fake.destructiveRequests != 0 {
		t.Fatalf("switches=%d destructive=%d", fake.switches, fake.destructiveRequests)
	}
}

func TestEventStreamRejectsOversizedEvent(t *testing.T) {
	server := newOmnigentHTTPTestServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		for range maxSSEEventBytes + 1 {
			_, _ = w.Write([]byte("x"))
		}
		_, _ = w.Write([]byte("\n\n"))
	}))
	defer server.Close()
	client, err := NewAPIClient(server.URL, server.Client())
	if err != nil {
		t.Fatal(err)
	}
	err = client.ConsumeStream(context.Background(), "conv_large", func(StreamEvent) error { return nil })
	if err == nil {
		t.Fatal("ConsumeStream accepted oversized event")
	}
}

type attachmentAPIFake struct {
	t                   *testing.T
	server              *httptest.Server
	mu                  sync.Mutex
	labels              map[string]string
	agentID             string
	workspace           string
	archived            bool
	missing             bool
	creates             int
	switches            int
	destructiveRequests int
	operations          []string
}

func newAttachmentAPIFake(t *testing.T, labels map[string]string) *attachmentAPIFake {
	t.Helper()
	f := &attachmentAPIFake{t: t, labels: cloneStringMap(labels), agentID: "ag_primary", workspace: "/work/assigned"}
	f.server = newOmnigentHTTPTestServer(http.HandlerFunc(f.serveHTTP))
	t.Cleanup(f.server.Close)
	return f
}

func (f *attachmentAPIFake) serveHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodGet && r.URL.Path == "/v1/sessions/conv_attach/stream" {
		f.record("stream")
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		w.(http.Flusher).Flush()
		<-r.Context().Done()
		return
	}
	if r.Method == http.MethodGet && r.URL.Path == "/v1/sessions/conv_missing/stream" {
		f.record("missing-stream")
		writeClientTestJSON(f.t, w, http.StatusNotFound, map[string]any{"error": map[string]string{"code": "not_found", "message": "missing"}})
		return
	}
	switch {
	case r.Method == http.MethodGet && r.URL.Path == "/v1/agents":
		f.record("agents")
		writeClientTestJSON(f.t, w, http.StatusOK, map[string]any{
			"data": []Agent{
				{ID: "ag_primary", Name: "claude-primary", Harness: "claude-sdk"},
				{ID: "ag_backup", Name: "claude-secondary", Harness: "claude-sdk"},
			},
			"has_more": false,
		})
	case r.Method == http.MethodPost && r.URL.Path == "/v1/sessions":
		f.mu.Lock()
		f.creates++
		f.operations = append(f.operations, "create")
		f.mu.Unlock()
		var body createSessionRequest
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			f.t.Errorf("decode create: %v", err)
		}
		f.mu.Lock()
		f.labels = cloneStringMap(body.Labels)
		f.mu.Unlock()
		f.writeSession(w)
	case r.Method == http.MethodGet && r.URL.Path == "/v1/sessions/conv_attach":
		f.record("snapshot")
		f.writeSession(w)
	case r.Method == http.MethodPost && r.URL.Path == "/v1/sessions/conv_attach/switch-agent":
		var body struct {
			AgentID string `json:"agent_id"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			f.t.Errorf("decode switch: %v", err)
		}
		f.mu.Lock()
		f.agentID = body.AgentID
		f.switches++
		f.mu.Unlock()
		f.writeSession(w)
	case r.Method == http.MethodPatch && r.URL.Path == "/v1/sessions/conv_attach":
		var body struct {
			Labels map[string]string `json:"labels"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			f.t.Errorf("decode labels: %v", err)
		}
		f.mu.Lock()
		for key, value := range body.Labels {
			f.labels[key] = value
		}
		f.mu.Unlock()
		f.writeSession(w)
	case r.Method == http.MethodDelete || r.Method == http.MethodPost && r.URL.Path == "/v1/sessions/conv_attach/events":
		f.mu.Lock()
		f.destructiveRequests++
		f.mu.Unlock()
		writeClientTestJSON(f.t, w, http.StatusOK, map[string]bool{"ok": true})
	default:
		http.NotFound(w, r)
	}
}

func (f *attachmentAPIFake) writeSession(w http.ResponseWriter) {
	f.mu.Lock()
	session := Session{
		ID: "conv_attach", AgentID: f.agentID, AgentName: f.agentID, Status: "idle",
		Archived: f.archived, Workspace: f.workspace, Labels: cloneStringMap(f.labels), Items: []SessionItem{},
	}
	f.mu.Unlock()
	writeClientTestJSON(f.t, w, http.StatusOK, session)
}

func (f *attachmentAPIFake) record(operation string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.operations = append(f.operations, operation)
}
