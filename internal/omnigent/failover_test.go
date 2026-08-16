package omnigent

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

func TestClassifyFailoverEventRequiresTerminalSequencedLLMFailure(t *testing.T) {
	sequence := int64(42)
	tests := []struct {
		name   string
		event  StreamEvent
		want   FailoverReason
		accept bool
	}{
		{name: "authentication status", event: errorStreamEvent(&sequence, "401", 0), want: FailoverAuthentication, accept: true},
		{name: "authentication semantic code", event: errorStreamEvent(&sequence, "authentication_error", 0), want: FailoverAuthentication, accept: true},
		{name: "rate limit status", event: errorStreamEvent(&sequence, "429", 0), want: FailoverRateLimit, accept: true},
		{name: "rate limit semantic code", event: errorStreamEvent(&sequence, "rate_limit_exceeded", 0), want: FailoverRateLimit, accept: true},
		{name: "backend status", event: errorStreamEvent(&sequence, "503", 0), want: FailoverBackendUnavailable, accept: true},
		{name: "backend semantic code", event: errorStreamEvent(&sequence, "connection_error", 0), want: FailoverBackendUnavailable, accept: true},
		{name: "detail status", event: errorStreamEvent(&sequence, "unknown", 429), want: FailoverRateLimit, accept: true},
		{name: "omnigent retry owns retry", event: StreamEvent{Type: "response.retry", Source: "llm", SequenceNumber: &sequence, Error: &StreamError{Code: "429"}}},
		{name: "tool error", event: StreamEvent{Type: "response.error", Source: "tool", SequenceNumber: &sequence, Error: &StreamError{Code: "429"}}},
		{name: "missing sequence", event: errorStreamEvent(nil, "429", 0)},
		{name: "zero sequence", event: errorStreamEvent(new(int64), "429", 0)},
		{name: "unknown classifier", event: errorStreamEvent(&sequence, "context_length_exceeded", 0)},
		{name: "prose is not parsed", event: StreamEvent{Type: "response.error", Source: "llm", SequenceNumber: &sequence, Error: &StreamError{Code: "unknown", Message: "401 unauthorized"}}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			signal, ok := ClassifyFailoverEvent(tt.event)
			if ok != tt.accept {
				t.Fatalf("ClassifyFailoverEvent accepted = %v, want %v: %#v", ok, tt.accept, signal)
			}
			if ok && signal.Reason != tt.want {
				t.Fatalf("reason = %q, want %q", signal.Reason, tt.want)
			}
		})
	}
}

func TestBindProfileChainRejectsMissingAmbiguousAndWrongHarnessAgents(t *testing.T) {
	catalog := loadFailoverCatalog(t)
	valid := []Agent{
		{ID: "ag_primary", Name: "claude-primary", Harness: "claude-sdk"},
		{ID: "ag_backup", Name: "claude-secondary", Harness: "claude-sdk"},
	}
	chain, err := BindProfileChain(catalog, "claude-primary", valid)
	if err != nil {
		t.Fatalf("BindProfileChain: %v", err)
	}
	if len(chain) != 2 || chain[0].AgentID != "ag_primary" || chain[1].AgentID != "ag_backup" {
		t.Fatalf("bound chain = %#v", chain)
	}

	tests := []struct {
		name   string
		agents []Agent
	}{
		{name: "missing", agents: valid[:1]},
		{name: "ambiguous", agents: append(append([]Agent(nil), valid...), Agent{ID: "ag_other", Name: "claude-primary", Harness: "claude-sdk"})},
		{name: "wrong harness", agents: []Agent{{ID: "ag_primary", Name: "claude-primary", Harness: "codex"}, valid[1]}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := BindProfileChain(catalog, "claude-primary", tt.agents); err == nil {
				t.Fatal("BindProfileChain succeeded, want error")
			}
		})
	}
}

func TestFailoverControllerAdvancesOnceStaysStickyAndExhausts(t *testing.T) {
	catalog := loadFailoverCatalog(t)
	fake := newFailoverAPIFake(t, initialFailoverLabels(t, catalog))
	client, err := NewAPIClient(fake.server.URL, fake.server.Client())
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 8, 15, 17, 30, 0, 0, time.UTC)
	controller, err := NewFailoverController(client, catalog, func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	sequence := int64(10)
	result, err := controller.Advance(context.Background(), "conv_failover", 0, errorStreamEvent(&sequence, "401", 0))
	if err != nil {
		t.Fatalf("Advance: %v", err)
	}
	if result.Transition == nil || result.Transition.FromProfileID != "claude-primary" || result.Transition.ToProfileID != "claude-secondary" {
		t.Fatalf("transition = %#v", result.Transition)
	}
	if result.Transition.FromBlurb == "" || result.Transition.ToBlurb == "" || result.Transition.Reason != FailoverAuthentication || !result.Transition.At.Equal(now) {
		t.Fatalf("transition metadata = %#v", result.Transition)
	}
	fake.mu.Lock()
	for key, want := range map[string]string{
		failoverLastFromLabel: "claude-primary", failoverLastToLabel: "claude-secondary",
		failoverLastReasonLabel: string(FailoverAuthentication), failoverLastAtLabel: now.Format(time.RFC3339Nano),
	} {
		if got := fake.labels[key]; got != want {
			fake.mu.Unlock()
			t.Fatalf("status label %s = %q, want %q", key, got, want)
		}
	}
	fake.mu.Unlock()

	// A duplicate consumer still holding the old expected index cannot move
	// the conversation a second time.
	duplicate, err := controller.Advance(context.Background(), "conv_failover", 0, errorStreamEvent(&sequence, "401", 0))
	if err != nil {
		t.Fatalf("duplicate Advance: %v", err)
	}
	if !duplicate.Ignored || duplicate.State.ActiveIndex != 1 {
		t.Fatalf("duplicate result = %#v", duplicate)
	}

	nextSequence := int64(11)
	exhausted, err := controller.Advance(context.Background(), "conv_failover", 1, errorStreamEvent(&nextSequence, "503", 0))
	if err != nil {
		t.Fatalf("exhaustion Advance: %v", err)
	}
	if !exhausted.Exhausted || exhausted.Transition != nil || exhausted.State.ActiveIndex != 1 {
		t.Fatalf("exhaustion result = %#v", exhausted)
	}
	if got := fake.switchCount(); got != 1 {
		t.Fatalf("switch count = %d, want 1", got)
	}
}

func TestFailoverControllerSerializesConcurrentAdvance(t *testing.T) {
	catalog := loadFailoverCatalog(t)
	fake := newFailoverAPIFake(t, initialFailoverLabels(t, catalog))
	client, err := NewAPIClient(fake.server.URL, fake.server.Client())
	if err != nil {
		t.Fatal(err)
	}
	controller, err := NewFailoverController(client, catalog, time.Now)
	if err != nil {
		t.Fatal(err)
	}
	sequence := int64(20)
	start := make(chan struct{})
	errs := make(chan error, 2)
	for range 2 {
		go func() {
			<-start
			_, err := controller.Advance(context.Background(), "conv_failover", 0, errorStreamEvent(&sequence, "429", 0))
			errs <- err
		}()
	}
	close(start)
	for range 2 {
		if err := <-errs; err != nil {
			t.Fatal(err)
		}
	}
	if got := fake.switchCount(); got != 1 {
		t.Fatalf("switch count = %d, want exactly one", got)
	}
}

func TestConcurrentCityEndpointsKeepCollidingConversationIDsIsolated(t *testing.T) {
	const cities = 12
	type lane struct {
		controller *FailoverController
		fake       *failoverAPIFake
		advance    bool
	}
	lanes := make([]lane, 0, cities)
	for i := 0; i < cities; i++ {
		catalog := loadFailoverCatalog(t)
		fake := newFailoverAPIFake(t, initialFailoverLabels(t, catalog))
		client, err := NewAPIClient(fake.server.URL, fake.server.Client())
		if err != nil {
			t.Fatal(err)
		}
		controller, err := NewFailoverController(client, catalog, time.Now)
		if err != nil {
			t.Fatal(err)
		}
		lanes = append(lanes, lane{controller: controller, fake: fake, advance: i%2 == 0})
	}

	var wg sync.WaitGroup
	errs := make(chan error, len(lanes))
	for i := range lanes {
		lane := lanes[i]
		wg.Add(1)
		go func() {
			defer wg.Done()
			if !lane.advance {
				_, err := lane.controller.Reconcile(context.Background(), "conv_failover")
				errs <- err
				return
			}
			sequence := int64(1)
			_, err := lane.controller.Advance(context.Background(), "conv_failover", 0, StreamEvent{
				Type: "response.error", Source: "llm", SequenceNumber: &sequence,
				Error: &StreamError{Code: "authentication_failed", Detail: &StreamErrorDetail{StatusCode: 401}},
			})
			errs <- err
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	for i, lane := range lanes {
		wantSwitches := 0
		if lane.advance {
			wantSwitches = 1
		}
		if got := lane.fake.switchCount(); got != wantSwitches {
			t.Fatalf("city lane %d switches=%d, want %d", i, got, wantSwitches)
		}
	}
}

func TestFailoverControllerReconcilesCrashAfterAgentSwitch(t *testing.T) {
	catalog := loadFailoverCatalog(t)
	labels := initialFailoverLabels(t, catalog)
	fake := newFailoverAPIFake(t, labels)
	fake.agentID = "ag_backup" // switch-agent committed; label PATCH did not.
	client, err := NewAPIClient(fake.server.URL, fake.server.Client())
	if err != nil {
		t.Fatal(err)
	}
	controller, err := NewFailoverController(client, catalog, time.Now)
	if err != nil {
		t.Fatal(err)
	}
	state, err := controller.Reconcile(context.Background(), "conv_failover")
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if state.ActiveIndex != 1 || state.ActiveProfileID != "claude-secondary" {
		t.Fatalf("state = %#v", state)
	}
	if got := fake.switchCount(); got != 0 {
		t.Fatalf("reconcile switched agent %d times", got)
	}
}

func TestFailoverControllerKeepsConversationChainAfterCatalogReorder(t *testing.T) {
	catalog := loadFailoverCatalog(t)
	labels := initialFailoverLabels(t, catalog)
	// This models an operator changing the fallback definition for future
	// conversations after this conversation already persisted its chain.
	primary := catalog.profiles["claude-primary"]
	primary.Fallbacks = nil
	catalog.profiles["claude-primary"] = primary

	fake := newFailoverAPIFake(t, labels)
	client, err := NewAPIClient(fake.server.URL, fake.server.Client())
	if err != nil {
		t.Fatal(err)
	}
	controller, err := NewFailoverController(client, catalog, time.Now)
	if err != nil {
		t.Fatal(err)
	}
	state, err := controller.Reconcile(context.Background(), "conv_failover")
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if len(state.Chain) != 2 || state.Chain[0].ID != "claude-primary" || state.Chain[1].ID != "claude-secondary" {
		t.Fatalf("stored chain changed with catalog fallback: %#v", state.Chain)
	}
}

func TestFailoverControllerRejectsMalformedOrForeignStateWithoutSwitching(t *testing.T) {
	catalog := loadFailoverCatalog(t)
	tests := []struct {
		name   string
		labels map[string]string
		agent  string
	}{
		{name: "missing chain", labels: map[string]string{}, agent: "ag_primary"},
		{name: "malformed chain", labels: map[string]string{failoverChainLabel: "not-json", failoverActiveLabel: "0"}, agent: "ag_primary"},
		{name: "changed chain", labels: map[string]string{failoverChainLabel: `["claude-secondary","claude-primary"]`, failoverActiveLabel: "0"}, agent: "ag_primary"},
		{name: "foreign agent", labels: initialFailoverLabels(t, catalog), agent: "ag_foreign"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fake := newFailoverAPIFake(t, tt.labels)
			fake.agentID = tt.agent
			client, err := NewAPIClient(fake.server.URL, fake.server.Client())
			if err != nil {
				t.Fatal(err)
			}
			controller, err := NewFailoverController(client, catalog, time.Now)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := controller.Reconcile(context.Background(), "conv_failover"); err == nil {
				t.Fatal("Reconcile succeeded, want fail-closed error")
			}
			if got := fake.switchCount(); got != 0 {
				t.Fatalf("switch count = %d, want 0", got)
			}
		})
	}
}

func errorStreamEvent(sequence *int64, code string, statusCode int) StreamEvent {
	event := StreamEvent{
		Type:           "response.error",
		Source:         "llm",
		SequenceNumber: sequence,
		Error:          &StreamError{Code: code, Message: "redacted fixture"},
	}
	if statusCode != 0 {
		event.Error.Detail = &StreamErrorDetail{StatusCode: statusCode}
	}
	return event
}

func loadFailoverCatalog(t *testing.T) *Catalog {
	t.Helper()
	root := t.TempDir()
	writeCatalogTestFile(t, root, "agents/primary.yaml", "name: claude-primary\n")
	writeCatalogTestFile(t, root, "agents/secondary.yaml", "name: claude-secondary\n")
	path := writeCatalogTestFile(t, root, "catalog.yaml", validCatalogHeader()+`profiles:
  claude-primary:
    display_name: Claude primary
    blurb: Primary compatible backend.
    harness: claude-sdk
    backend: primary-gateway
    network: external-model
    agent: agents/primary.yaml
    fallbacks: [claude-secondary]
  claude-secondary:
    display_name: Claude secondary
    blurb: Independent backup backend.
    harness: claude-sdk
    backend: backup-gateway
    network: external-model
    agent: agents/secondary.yaml
`)
	catalog, err := LoadCatalog(path)
	if err != nil {
		t.Fatal(err)
	}
	return catalog
}

func initialFailoverLabels(t *testing.T, catalog *Catalog) map[string]string {
	t.Helper()
	labels, err := InitialFailoverLabels(catalog, "claude-primary")
	if err != nil {
		t.Fatal(err)
	}
	return labels
}

type failoverAPIFake struct {
	t                  *testing.T
	server             *httptest.Server
	mu                 sync.Mutex
	agentID            string
	labels             map[string]string
	switches           int
	workspace          string
	policyResponses    int
	lastPolicyResponse sessionEventRequest
}

func newFailoverAPIFake(t *testing.T, labels map[string]string) *failoverAPIFake {
	t.Helper()
	f := &failoverAPIFake{
		t: t, agentID: "ag_primary", labels: cloneStringMap(labels), workspace: "/work/assigned",
	}
	f.server = newOmnigentHTTPTestServer(http.HandlerFunc(f.serveHTTP))
	t.Cleanup(f.server.Close)
	return f
}

func (f *failoverAPIFake) serveHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	switch {
	case r.Method == http.MethodGet && r.URL.Path == "/v1/agents":
		writeClientTestJSON(f.t, w, http.StatusOK, map[string]any{
			"data": []Agent{
				{ID: "ag_primary", Name: "claude-primary", Harness: "claude-sdk"},
				{ID: "ag_backup", Name: "claude-secondary", Harness: "claude-sdk"},
			},
			"has_more": false,
		})
	case r.Method == http.MethodGet && r.URL.Path == "/v1/sessions/conv_failover":
		f.writeSession(w)
	case r.Method == http.MethodPost && r.URL.Path == "/v1/sessions/conv_failover/switch-agent":
		var body struct {
			AgentID string `json:"agent_id"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			f.t.Errorf("decode switch-agent: %v", err)
		}
		if body.AgentID != "ag_backup" {
			f.t.Errorf("switch agent id = %q, want ag_backup", body.AgentID)
		}
		f.agentID = body.AgentID
		f.switches++
		f.writeSession(w)
	case r.Method == http.MethodPatch && r.URL.Path == "/v1/sessions/conv_failover":
		var body struct {
			Labels map[string]string `json:"labels"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			f.t.Errorf("decode labels: %v", err)
		}
		for key, value := range body.Labels {
			f.labels[key] = value
		}
		f.writeSession(w)
	case r.Method == http.MethodPost && r.URL.Path == "/v1/sessions/conv_failover/events":
		var body sessionEventRequest
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			f.t.Errorf("decode session event: %v", err)
		}
		f.policyResponses++
		f.lastPolicyResponse = body
		writeClientTestJSON(f.t, w, http.StatusAccepted, map[string]bool{"queued": true})
	default:
		http.Error(w, fmt.Sprintf("unexpected %s %s", r.Method, r.URL.Path), http.StatusNotFound)
	}
}

func (f *failoverAPIFake) writeSession(w http.ResponseWriter) {
	writeClientTestJSON(f.t, w, http.StatusOK, Session{
		ID: "conv_failover", AgentID: f.agentID, AgentName: f.agentID,
		Status: "idle", Workspace: f.workspace, Labels: cloneStringMap(f.labels), Items: []SessionItem{},
	})
}

func (f *failoverAPIFake) switchCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.switches
}

func (f *failoverAPIFake) policyResponseCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.policyResponses
}

func (f *failoverAPIFake) policyResponseSnapshot() (int, sessionEventRequest) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.policyResponses, f.lastPolicyResponse
}
