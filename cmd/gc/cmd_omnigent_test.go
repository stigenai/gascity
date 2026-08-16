package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/omnigent"
)

func newOmnigentCLITestServer(handler http.Handler) *httptest.Server {
	return httptest.NewServer(handler)
}

func TestOmnigentCommandExposesServeWithoutRemoteOrInstallControls(t *testing.T) {
	cmd := newOmnigentCmd(&bytes.Buffer{}, &bytes.Buffer{})
	if cmd.Use != "omnigent" {
		t.Fatalf("Use = %q", cmd.Use)
	}
	serve, _, err := cmd.Find([]string{"serve"})
	if err != nil || serve == cmd {
		t.Fatalf("serve command missing: %v", err)
	}
	attach, _, err := cmd.Find([]string{"attach"})
	if err != nil || attach == cmd {
		t.Fatalf("attach command missing: %v", err)
	}
	for _, forbidden := range []string{"install", "upgrade", "host", "remote", "fork", "kubernetes", "daytona"} {
		if found, _, _ := cmd.Find([]string{forbidden}); found != cmd {
			t.Fatalf("unexpected omnigent subcommand %q", forbidden)
		}
	}
	for _, expected := range []string{"serve", "attach", "explain", "status", "doctor"} {
		found, _, err := cmd.Find([]string{expected})
		if err != nil || found == cmd {
			t.Fatalf("missing omnigent subcommand %q: %v", expected, err)
		}
	}
}

func TestOmnigentAttachRejectsRemoteCityBeforeLocalServiceLookup(t *testing.T) {
	t.Setenv("GC_HOME", t.TempDir())
	addProdContext(t)
	setProdContextFlag(t)

	var stdout, stderr bytes.Buffer
	cmd := newOmnigentAttachCmd(&stdout, &stderr)
	cmd.SetArgs([]string{"--profile", "claude-primary"})
	err := cmd.Execute()
	if err == nil || !strings.Contains(err.Error(), "resolve local city") || !strings.Contains(err.Error(), "does not support a remote city") {
		t.Fatalf("attach remote error = %v", err)
	}
	if strings.Contains(err.Error(), "controller unavailable") || strings.Contains(err.Error(), "local service proxy") {
		t.Fatalf("remote attach reached local service lookup: %v", err)
	}
}

func TestRenderOmnigentCLIReportGoldenShowsProvenanceWithoutSecrets(t *testing.T) {
	report := omnigentCLIReport{
		SchemaVersion: "1", CityPath: "/city",
		Selection: &omnigent.ProfileSelection{ID: "claude-primary", Source: omnigent.ProfileSourceExplicit},
		Selected: &omnigent.ProfileDiagnostic{PublicProfile: omnigent.PublicProfile{
			ID: "claude-primary", Blurb: "Primary local profile.", Harness: "claude-sdk",
			Backend: "compatible", Availability: "available", Chain: []string{"claude-primary", "claude-secondary"},
		}},
		Sidecar: omnigent.LocalStatus{
			Mode: "local", Ready: true,
			Pin: omnigent.LocalPinStatus{
				Executable: "omnigent", ResolvedPath: "/opt/local/bin/omnigent", PackageVersion: "0.8.2",
				Commit: strings.Repeat("a", 40), SHA256: "sha256:" + strings.Repeat("b", 64),
			},
			Profiles: []omnigent.ProfileDiagnostic{{PublicProfile: omnigent.PublicProfile{
				ID: "claude-primary", Blurb: "Primary local profile.", Harness: "claude-sdk",
				Backend: "compatible", Availability: "available", Chain: []string{"claude-primary", "claude-secondary"},
			}}},
			Conversation: &omnigent.ConversationStatus{
				ID: "conv_durable", Status: "idle", Outcome: omnigent.OutcomeExists, Workspace: "/work/rig", ActiveProfileID: "claude-primary", ActiveIndex: 0,
				LastTransition: &omnigent.ProfileTransition{
					FromProfileID: "claude-primary", ToProfileID: "claude-secondary",
					Reason: omnigent.FailoverRateLimit, At: time.Date(2026, 8, 15, 12, 0, 0, 0, time.UTC),
				},
				Policy: &omnigent.PolicyStatus{RequestID: "policy-1", Kind: "tool.approval", State: "pending", Pending: true, MailBound: true},
			},
		},
		Session: &omnigentSessionStatus{
			ID: "ga-session", Provider: "omnigent", Runtime: "herdr", RuntimeSession: "city--worker",
			Workspace: "/work/rig", ConversationID: "conv_durable",
		},
	}
	var output bytes.Buffer
	if err := renderOmnigentCLIReport(&output, report); err != nil {
		t.Fatal(err)
	}
	want := "mode: local\n" +
		"ready: true\n" +
		"city: /city\n" +
		"binary.executable: omnigent\n" +
		"binary.path: /opt/local/bin/omnigent\n" +
		"binary.version: 0.8.2\n" +
		"binary.commit: " + strings.Repeat("a", 40) + "\n" +
		"binary.sha256: sha256:" + strings.Repeat("b", 64) + "\n" +
		"selected.id: claude-primary\n" +
		"selected.source: explicit\n" +
		"selected.blurb: Primary local profile.\n" +
		"selected.harness: claude-sdk\n" +
		"selected.backend: compatible\n" +
		"selected.chain: claude-primary,claude-secondary\n" +
		"selected.availability: available\n" +
		"profile.claude-primary: harness=claude-sdk backend=compatible availability=available blurb=\"Primary local profile.\" missing_auth=\n" +
		"session.id: ga-session\n" +
		"session.provider: omnigent\n" +
		"session.runtime: herdr\n" +
		"session.runtime_view: city--worker\n" +
		"session.workspace: /work/rig\n" +
		"session.conversation: conv_durable\n" +
		"conversation.status: idle\n" +
		"conversation.outcome: exists\n" +
		"conversation.active_profile: claude-primary\n" +
		"conversation.active_index: 0\n" +
		"conversation.exhausted: false\n" +
		"conversation.failover: claude-primary->claude-secondary category=rate_limit at=2026-08-15T12:00:00Z\n" +
		"conversation.policy: request=policy-1 kind=tool.approval state=pending pending=true mail_bound=true\n"
	if output.String() != want {
		t.Fatalf("diagnostic output changed:\n--- got ---\n%s--- want ---\n%s", output.String(), want)
	}
	for _, forbidden := range []string{"sk-secret", "ANTHROPIC_API_KEY=", "AWS_SECRET_ACCESS_KEY="} {
		if strings.Contains(output.String(), forbidden) {
			t.Fatalf("output leaked %q", forbidden)
		}
	}
}

func TestSelectOmnigentDiagnosticReportsNamedMissingAuthWithoutValues(t *testing.T) {
	profiles := []omnigent.ProfileDiagnostic{{
		PublicProfile:      omnigent.PublicProfile{ID: "claude-primary", Availability: "unavailable"},
		MissingEnvironment: []string{"ANTHROPIC_PROFILE_PRIMARY"},
	}}
	selection, profile, err := selectOmnigentDiagnostic("claude-primary", profiles, func(string) (string, bool) {
		return "sk-secret-must-not-appear", true
	})
	if err != nil || selection.ID != "claude-primary" || profile.MissingEnvironment[0] != "ANTHROPIC_PROFILE_PRIMARY" {
		t.Fatalf("selection=%#v profile=%#v error=%v", selection, profile, err)
	}
	encoded, _ := json.Marshal(profile)
	if strings.Contains(string(encoded), "sk-secret") {
		t.Fatalf("profile diagnostic leaked auth value: %s", encoded)
	}
}

func TestRunOmnigentAttachPreservesInputAndOrderedLiveOutput(t *testing.T) {
	const prompt = " fix $(touch /tmp/nope); 'quotes' 世界 "
	messageReceived := make(chan string, 1)
	streamOpened := make(chan struct{})
	persisted := make(chan struct{})
	failoverObserved := make(chan struct{})
	var streamOnce sync.Once
	server := newOmnigentCLITestServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/gascity/v1/profiles":
			writeOmnigentTestJSON(t, w, http.StatusOK, []omnigent.PublicProfile{{
				ID: "claude-primary", DisplayName: "Claude", Blurb: "Primary local profile.", Harness: "claude-sdk",
				Backend: "compatible", Network: "external-model", Availability: "available", Chain: []string{"claude-primary"},
			}})
		case r.Method == http.MethodPost && r.URL.Path == "/gascity/v1/attachments":
			writeOmnigentTestJSON(t, w, http.StatusOK, omnigent.AttachmentDescriptor{
				ConversationID: "conv_attach", ProfileID: "claude-primary", Fresh: true,
				ActiveProfile: "claude-primary", ActiveIndex: 0,
			})
		case r.Method == http.MethodGet && r.URL.Path == "/v1/sessions/conv_attach/stream":
			select {
			case <-persisted:
			default:
				t.Error("stream opened before fresh conversation identity was persisted")
			}
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(http.StatusOK)
			w.(http.Flusher).Flush()
			streamOnce.Do(func() { close(streamOpened) })
			select {
			case <-r.Context().Done():
				return
			case <-messageReceived:
			}
			_, _ = io.WriteString(w, "data: {\"type\":\"response.error\",\"source\":\"llm\",\"conversation_id\":\"conv_attach\",\"sequence_number\":1,\"error\":{\"code\":\"429\",\"message\":\"must stay local\"}}\n\n")
			w.(http.Flusher).Flush()
			select {
			case <-r.Context().Done():
				return
			case <-failoverObserved:
			}
			_, _ = io.WriteString(w, "data: {\"type\":\"output_delta\",\"conversation_id\":\"conv_attach\",\"sequence_number\":2,\"delta\":\"live 世界\"}\n\n")
			_, _ = io.WriteString(w, "data: [DONE]\n\n")
		case r.Method == http.MethodPost && r.URL.Path == "/gascity/v1/failover":
			close(failoverObserved)
			writeOmnigentTestJSON(t, w, http.StatusOK, omnigent.FailoverObservationResult{
				ActiveProfileID: "claude-secondary", ActiveIndex: 1,
				Transition: &omnigent.ProfileTransition{
					FromProfileID: "claude-primary", FromBlurb: "Primary local profile.",
					ToProfileID: "claude-secondary", ToBlurb: "Backup local profile.",
					Reason: omnigent.FailoverRateLimit, At: time.Date(2026, 8, 15, 18, 0, 0, 0, time.UTC),
				},
			})
			w.(http.Flusher).Flush()
		case r.Method == http.MethodGet && r.URL.Path == "/v1/sessions/conv_attach":
			writeOmnigentTestJSON(t, w, http.StatusOK, omnigent.Session{
				ID: "conv_attach", Status: "idle", Workspace: "/work/assigned",
				Labels: map[string]string{
					"gascity.omnigent.failover.version": "1", "gascity.omnigent.profile.chain": `["claude-primary"]`,
					"gascity.omnigent.profile.active": "0", "gascity.omnigent.failover.exhausted": "false",
				},
				Items: []omnigent.SessionItem{{ID: "history", Role: "assistant", Content: []omnigent.ContentBlock{{Type: "output_text", Text: "history"}}}},
			})
		case r.Method == http.MethodPost && r.URL.Path == "/v1/sessions/conv_attach/events":
			var request struct {
				Type string `json:"type"`
				Data struct {
					Content []omnigent.ContentBlock `json:"content"`
				} `json:"data"`
			}
			if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
				t.Errorf("decode event: %v", err)
			}
			if request.Type != "message" || len(request.Data.Content) != 1 {
				t.Errorf("event = %#v", request)
			}
			messageReceived <- request.Data.Content[0].Text
			writeOmnigentTestJSON(t, w, http.StatusAccepted, map[string]bool{"queued": true})
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	client, err := omnigent.NewAPIClient(server.URL, server.Client())
	if err != nil {
		t.Fatal(err)
	}
	reader, writer := io.Pipe()
	defer func() {
		if err := writer.Close(); err != nil {
			t.Errorf("close input writer: %v", err)
		}
	}()
	var stdout, stderr bytes.Buffer
	done := make(chan error, 1)
	go func() {
		done <- runOmnigentAttach(context.Background(), client, omnigent.AttachmentOpenInput{
			ProfileID: "claude-primary", Workspace: "/work/assigned", Title: "worker",
		}, func(candidate string) (string, error) {
			if candidate != "conv_attach" {
				t.Errorf("persist candidate = %q", candidate)
			}
			close(persisted)
			return candidate, nil
		}, nil, reader, &stdout, &stderr, make(chan os.Signal))
	}()
	select {
	case <-streamOpened:
	case <-time.After(2 * time.Second):
		t.Fatal("stream was not opened")
	}
	if _, err := io.WriteString(writer, prompt+"\n"); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("attach did not finish")
	}
	select {
	case got := <-messageReceived:
		if got != prompt {
			t.Fatalf("message = %q", got)
		}
	default:
		// The stream handler consumed the channel value; the exact request was
		// already checked before it released the stream.
	}
	if got := stdout.String(); got != "history\nlive 世界" {
		t.Fatalf("stdout = %q", got)
	}
	if !strings.Contains(stderr.String(), "conversation=conv_attach profile=claude-primary") {
		t.Fatalf("stderr = %q", stderr.String())
	}
	if !strings.Contains(stderr.String(), `blurb="Primary local profile."`) || !strings.Contains(stderr.String(), "fallback_chain=claude-primary") {
		t.Fatalf("active profile discovery missing from stderr = %q", stderr.String())
	}
	if !strings.Contains(stderr.String(), "failover from=claude-primary") || !strings.Contains(stderr.String(), "to=claude-secondary") || !strings.Contains(stderr.String(), "Backup local profile.") {
		t.Fatalf("failover status missing from stderr = %q", stderr.String())
	}
}

func TestRunOmnigentAttachConcurrentFreshLaunchConvergesOnPersistedWinner(t *testing.T) {
	var attachmentCalls int
	var stoppedCandidate bool
	server := newOmnigentCLITestServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/gascity/v1/profiles":
			writeOmnigentTestJSON(t, w, http.StatusOK, []omnigent.PublicProfile{{
				ID: "profile", DisplayName: "Profile", Blurb: "Local.", Harness: "codex",
				Backend: "openai", Network: "external-model", Availability: "available", Chain: []string{"profile"},
			}})
		case r.Method == http.MethodPost && r.URL.Path == "/gascity/v1/attachments":
			attachmentCalls++
			id, fresh := "conv_loser", true
			if attachmentCalls == 2 {
				id, fresh = "conv_winner", false
			}
			writeOmnigentTestJSON(t, w, http.StatusOK, omnigent.AttachmentDescriptor{
				ConversationID: id, ProfileID: "profile", Fresh: fresh, ActiveProfile: "profile", ActiveIndex: 0,
			})
		case r.Method == http.MethodPost && r.URL.Path == "/v1/sessions/conv_loser/events":
			stoppedCandidate = true
			writeOmnigentTestJSON(t, w, http.StatusAccepted, map[string]bool{"queued": true})
		case r.Method == http.MethodGet && r.URL.Path == "/v1/sessions/conv_winner/stream":
			if !stoppedCandidate {
				t.Error("losing fresh conversation was not stopped before winner attachment")
			}
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = io.WriteString(w, "data: [DONE]\n\n")
		case r.Method == http.MethodGet && r.URL.Path == "/v1/sessions/conv_winner":
			writeOmnigentTestJSON(t, w, http.StatusOK, omnigent.Session{
				ID: "conv_winner", Workspace: "/work/assigned", Labels: map[string]string{
					"gascity.omnigent.failover.version": "1", "gascity.omnigent.profile.chain": `["profile"]`,
					"gascity.omnigent.profile.active": "0", "gascity.omnigent.failover.exhausted": "false",
				},
			})
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	client, err := omnigent.NewAPIClient(server.URL, server.Client())
	if err != nil {
		t.Fatal(err)
	}
	err = runOmnigentAttach(context.Background(), client, omnigent.AttachmentOpenInput{
		ProfileID: "profile", Workspace: "/work/assigned",
	}, func(candidate string) (string, error) {
		if candidate != "conv_loser" {
			t.Fatalf("candidate = %q", candidate)
		}
		return "conv_winner", nil
	}, nil, strings.NewReader(""), io.Discard, io.Discard, make(chan os.Signal))
	if err != nil {
		t.Fatal(err)
	}
	if attachmentCalls != 2 || !stoppedCandidate {
		t.Fatalf("attachment calls=%d stopped=%t", attachmentCalls, stoppedCandidate)
	}
}

func TestRunOmnigentAttachPersistenceFailureStopsFreshConversationBeforeStreaming(t *testing.T) {
	var stopped, streamed bool
	server := newOmnigentCLITestServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/gascity/v1/profiles":
			writeOmnigentTestJSON(t, w, http.StatusOK, []omnigent.PublicProfile{{
				ID: "profile", DisplayName: "Profile", Blurb: "Local.", Harness: "codex",
				Backend: "openai", Network: "external-model", Availability: "available", Chain: []string{"profile"},
			}})
		case r.Method == http.MethodPost && r.URL.Path == "/gascity/v1/attachments":
			writeOmnigentTestJSON(t, w, http.StatusOK, omnigent.AttachmentDescriptor{
				ConversationID: "conv_fresh", ProfileID: "profile", Fresh: true, ActiveProfile: "profile", ActiveIndex: 0,
			})
		case r.Method == http.MethodPost && r.URL.Path == "/v1/sessions/conv_fresh/events":
			stopped = true
			writeOmnigentTestJSON(t, w, http.StatusAccepted, map[string]bool{"queued": true})
		case r.URL.Path == "/v1/sessions/conv_fresh/stream":
			streamed = true
			http.Error(w, "unexpected", http.StatusInternalServerError)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	client, err := omnigent.NewAPIClient(server.URL, server.Client())
	if err != nil {
		t.Fatal(err)
	}
	err = runOmnigentAttach(context.Background(), client, omnigent.AttachmentOpenInput{
		ProfileID: "profile", Workspace: "/work/assigned",
	}, func(string) (string, error) { return "", errors.New("store unavailable") }, nil,
		strings.NewReader(""), io.Discard, io.Discard, make(chan os.Signal))
	if err == nil || !strings.Contains(err.Error(), "persist") || !stopped || streamed {
		t.Fatalf("error=%v stopped=%t streamed=%t", err, stopped, streamed)
	}
}

func TestRunOmnigentAttachSIGTERMStopsDurableConversationWithoutClearingIdentity(t *testing.T) {
	streamOpened := make(chan struct{})
	stopped := make(chan struct{})
	server := newOmnigentCLITestServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/gascity/v1/profiles":
			writeOmnigentTestJSON(t, w, http.StatusOK, []omnigent.PublicProfile{{
				ID: "profile", DisplayName: "Profile", Blurb: "Local.", Harness: "codex",
				Backend: "openai", Network: "external-model", Availability: "available", Chain: []string{"profile"},
			}})
		case r.Method == http.MethodPost && r.URL.Path == "/gascity/v1/attachments":
			writeOmnigentTestJSON(t, w, http.StatusOK, omnigent.AttachmentDescriptor{
				ConversationID: "conv_durable", ProfileID: "profile", Fresh: false, ActiveProfile: "profile", ActiveIndex: 0,
			})
		case r.Method == http.MethodGet && r.URL.Path == "/v1/sessions/conv_durable/stream":
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(http.StatusOK)
			w.(http.Flusher).Flush()
			close(streamOpened)
			<-r.Context().Done()
		case r.Method == http.MethodGet && r.URL.Path == "/v1/sessions/conv_durable":
			writeOmnigentTestJSON(t, w, http.StatusOK, omnigent.Session{
				ID: "conv_durable", Workspace: "/work/assigned", Labels: map[string]string{
					"gascity.omnigent.failover.version": "1", "gascity.omnigent.profile.chain": `["profile"]`,
					"gascity.omnigent.profile.active": "0", "gascity.omnigent.failover.exhausted": "false",
				},
			})
		case r.Method == http.MethodPost && r.URL.Path == "/v1/sessions/conv_durable/events":
			close(stopped)
			writeOmnigentTestJSON(t, w, http.StatusAccepted, map[string]bool{"queued": true})
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	client, err := omnigent.NewAPIClient(server.URL, server.Client())
	if err != nil {
		t.Fatal(err)
	}
	interrupts := make(chan os.Signal, 1)
	done := make(chan error, 1)
	bindCalls := 0
	inputReader, inputWriter := io.Pipe()
	defer func() {
		if err := inputWriter.Close(); err != nil {
			t.Errorf("close input writer: %v", err)
		}
	}()
	go func() {
		done <- runOmnigentAttach(context.Background(), client, omnigent.AttachmentOpenInput{
			ProfileID: "profile", ConversationID: "conv_durable", Workspace: "/work/assigned",
		}, func(candidate string) (string, error) {
			bindCalls++
			return candidate, nil
		}, nil, inputReader, io.Discard, io.Discard, interrupts)
	}()
	select {
	case <-streamOpened:
	case <-time.After(2 * time.Second):
		t.Fatal("stream did not open")
	}
	interrupts <- syscall.SIGTERM
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("attach did not stop")
	}
	select {
	case <-stopped:
	default:
		t.Fatal("stop_session was not posted")
	}
	if bindCalls != 1 {
		t.Fatalf("identity bind calls=%d, want one durable replay", bindCalls)
	}
}

func TestReadOmnigentInputSupportsLargeLinesAndRejectsOversize(t *testing.T) {
	large := strings.Repeat("界", 100_000)
	results := make(chan omnigentInputResult)
	go readOmnigentInput(strings.NewReader(large+"\n"), results)
	result := <-results
	if result.err != nil || result.text != large {
		t.Fatalf("large input bytes=%d error=%v", len(result.text), result.err)
	}
	oversize := strings.Repeat("x", maxOmnigentInputBytes+1)
	results = make(chan omnigentInputResult)
	go readOmnigentInput(strings.NewReader(oversize), results)
	if result = <-results; result.err == nil || !strings.Contains(result.err.Error(), "exceeds") {
		t.Fatalf("oversize result = %#v", result)
	}
}

func TestOmnigentIdentityFromEnvironmentUsesOnlyExplicitNonSecretContext(t *testing.T) {
	env := map[string]string{
		"GC_SESSION_ID": "session-123", "GC_AGENT": "rig/worker", "GC_RIG": "rig", "GC_CITY": "bright-lights",
		"ANTHROPIC_API_KEY": "must-not-forward", "AWS_SECRET_ACCESS_KEY": "must-not-forward",
	}
	identity, err := omnigentIdentityFromLookup(func(name string) (string, bool) { value, ok := env[name]; return value, ok })
	if err != nil {
		t.Fatal(err)
	}
	if identity.SessionID != "session-123" || identity.Agent != "rig/worker" || identity.Rig != "rig" || identity.City != "bright-lights" {
		t.Fatalf("identity = %#v", identity)
	}
	encoded, _ := json.Marshal(identity)
	if strings.Contains(string(encoded), "must-not-forward") || strings.Contains(string(encoded), "ANTHROPIC") || strings.Contains(string(encoded), "AWS_") {
		t.Fatalf("identity leaked ambient credential context: %s", encoded)
	}
	delete(env, "GC_SESSION_ID")
	if _, err := omnigentIdentityFromLookup(func(name string) (string, bool) { value, ok := env[name]; return value, ok }); err == nil {
		t.Fatal("missing GC_SESSION_ID succeeded")
	}
}

func TestResolveOmnigentRequestedConversationUsesDurableIdentityExactly(t *testing.T) {
	for _, tt := range []struct {
		name, stored, explicit, want string
		wantErr                      bool
	}{
		{name: "restart", stored: "conv_durable", want: "conv_durable"},
		{name: "matching explicit", stored: "conv_durable", explicit: "conv_durable", want: "conv_durable"},
		{name: "first explicit resume", explicit: "conv_imported", want: "conv_imported"},
		{name: "fresh", want: ""},
		{name: "conflict", stored: "conv_durable", explicit: "conv_other", wantErr: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			got, err := resolveOmnigentRequestedConversation(tt.stored, tt.explicit, "gc-session")
			if tt.wantErr {
				if err == nil || !strings.Contains(err.Error(), "conflicts") {
					t.Fatalf("result=(%q, %v), want conflict", got, err)
				}
				return
			}
			if err != nil || got != tt.want {
				t.Fatalf("result=(%q, %v), want (%q, nil)", got, err, tt.want)
			}
		})
	}
}

func writeOmnigentTestJSON(t *testing.T, w http.ResponseWriter, status int, value any) {
	t.Helper()
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(value); err != nil {
		t.Errorf("encode response: %v", err)
	}
}

func TestRenderOmnigentSnapshotRendersCommittedTextOnce(t *testing.T) {
	snapshot := omnigent.Session{Items: []omnigent.SessionItem{
		{ID: "item_user", Role: "user", Content: []omnigent.ContentBlock{{Type: "input_text", Text: "hello 世界"}}},
		{ID: "item_assistant", Role: "assistant", Content: []omnigent.ContentBlock{{Type: "output_text", Text: "answer\nline two"}}},
		{ID: "item_assistant", Role: "assistant", Content: []omnigent.ContentBlock{{Type: "output_text", Text: "duplicate"}}},
	}}
	var output bytes.Buffer
	seen, err := renderOmnigentSnapshot(snapshot, &output)
	if err != nil {
		t.Fatal(err)
	}
	if got := output.String(); got != "> hello 世界\nanswer\nline two\n" {
		t.Fatalf("output = %q", got)
	}
	if !seen["item_user"] || !seen["item_assistant"] || len(seen) != 2 {
		t.Fatalf("seen = %#v", seen)
	}
}

func TestRenderOmnigentEventKeepsStdoutAndStderrSeparated(t *testing.T) {
	var stdout, stderr bytes.Buffer
	seen := map[string]bool{}
	if err := renderOmnigentEvent(omnigent.StreamEvent{Type: "delta", Source: "stdout", Delta: "out"}, seen, &stdout, &stderr); err != nil {
		t.Fatal(err)
	}
	if err := renderOmnigentEvent(omnigent.StreamEvent{Type: "delta", Source: "stderr", Delta: "err"}, seen, &stdout, &stderr); err != nil {
		t.Fatal(err)
	}
	if stdout.String() != "out" || stderr.String() != "err" {
		t.Fatalf("stdout=%q stderr=%q", stdout.String(), stderr.String())
	}
}

func TestOmnigentServeRequiresServiceOwnedAbsoluteEnvironment(t *testing.T) {
	t.Setenv("GC_SERVICE_STATE_ROOT", "")
	t.Setenv("GC_SERVICE_SOCKET", "")
	var stdout, stderr bytes.Buffer
	cmd := newOmnigentCmd(&stdout, &stderr)
	cmd.SetArgs([]string{"serve"})
	err := cmd.Execute()
	if err == nil || !strings.Contains(err.Error(), "GC_SERVICE_STATE_ROOT") {
		t.Fatalf("error = %v", err)
	}
	if stdout.Len() != 0 || strings.Contains(stderr.String(), "secret") {
		t.Fatalf("stdout=%q stderr=%q", stdout.String(), stderr.String())
	}
}
