package omnigent

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestPrepareSidecarBuildsPinnedLocalPlanWithMinimalEnvironment(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell fixture is Unix-specific")
	}
	root := t.TempDir()
	configDir := filepath.Join(root, "config")
	if err := os.MkdirAll(filepath.Join(configDir, "agents"), 0o700); err != nil {
		t.Fatal(err)
	}
	executable := filepath.Join(root, "omnigent")
	body := "#!/bin/sh\necho 'omnigent 0.10.0.dev0 (2aba5079)'\n"
	if err := os.WriteFile(executable, []byte(body), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(configDir, "agents", "primary.yaml"), []byte("name: claude-primary\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256([]byte(body))
	catalogText := fmt.Sprintf(`version: 1
omnigent:
  commit: 2aba5079d4d3a2a84d8c9927884fc4b8ce0eeecc
  package_version: 0.10.0.dev0
  executable: %s
  sha256: sha256:%x
profiles:
  claude-primary:
    display_name: Claude primary
    blurb: Compatible primary backend.
    harness: claude-sdk
    backend: compatible-gateway
    network: external-model
    agent: agents/primary.yaml
    environment: [HOME, CLAUDE_PRIMARY_TOKEN]
`, executable, sum)
	catalogPath := filepath.Join(configDir, "profiles.yaml")
	if err := os.WriteFile(catalogPath, []byte(catalogText), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", "/operator/home")
	t.Setenv("CLAUDE_PRIMARY_TOKEN", "fixture-secret-value")
	t.Setenv("UNRELATED_SECRET", "must-not-pass")

	prepared, err := PrepareSidecar(context.Background(), SidecarConfig{
		StateRoot: root, CatalogPath: catalogPath,
	}, 43123)
	if err != nil {
		t.Fatalf("PrepareSidecar: %v", err)
	}
	resolvedExecutable, err := filepath.EvalSymlinks(executable)
	if err != nil {
		t.Fatal(err)
	}
	if prepared.Plan.Executable != resolvedExecutable {
		t.Fatalf("executable = %q, want %q", prepared.Plan.Executable, resolvedExecutable)
	}
	args := prepared.Plan.Args
	joined := strings.Join(args, " ")
	for _, want := range []string{
		"server", "--host 127.0.0.1", "--port 43123", "--database-uri sqlite:///",
		"--conversation-database-uri sqlite:///", "--artifact-location", "--config", "--no-open", "--agent",
	} {
		if !strings.Contains(joined, want) {
			t.Fatalf("args missing %q: %v", want, args)
		}
	}
	for _, forbidden := range []string{"--background", "--managed", "--host 0.0.0.0"} {
		if strings.Contains(joined, forbidden) {
			t.Fatalf("args contain forbidden %q: %v", forbidden, args)
		}
	}
	env := envListMap(prepared.Plan.Env)
	if env["HOME"] != "/operator/home" || env["CLAUDE_PRIMARY_TOKEN"] != "fixture-secret-value" {
		t.Fatalf("explicit environment missing: keys=%v", sortedMapKeys(env))
	}
	if _, ok := env["UNRELATED_SECRET"]; ok {
		t.Fatal("unrelated ambient secret forwarded")
	}
	for key, want := range map[string]string{
		"OMNIGENT_CONFIG_HOME":          prepared.Paths.ConfigDir,
		"OMNIGENT_DATA_DIR":             prepared.Paths.DataDir,
		"OMNIGENT_NO_UPDATE_CHECK":      "1",
		"OMNIGENT_DISABLE_TELEMETRY":    "true",
		"OMNIGENT_TELEMETRY_ENABLED":    "0",
		"OMNIGENT_OTEL_CAPTURE_CONTENT": "0",
	} {
		if env[key] != want {
			t.Fatalf("env[%s] = %q, want %q", key, env[key], want)
		}
	}
	for _, path := range []string{prepared.Paths.Root, prepared.Paths.ConfigDir, prepared.Paths.DataDir, prepared.Paths.RunDir, prepared.Paths.SecretsDir} {
		info, err := os.Stat(path)
		if err != nil || info.Mode().Perm() != 0o700 {
			t.Fatalf("mode %s = %v, %v; want 0700", path, infoMode(info), err)
		}
	}
}

func TestSidecarHandlerProxiesHealthAndExposesOnlyPublicProfileMetadata(t *testing.T) {
	catalog := loadFailoverCatalog(t)
	upstream := newOmnigentHTTPTestServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/health" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"status":"ok"}`)
	}))
	defer upstream.Close()
	handler, err := NewSidecarHandler(catalog, upstream.URL, upstream.Client())
	if err != nil {
		t.Fatal(err)
	}
	proxy := newOmnigentHTTPTestServer(handler)
	defer proxy.Close()

	resp, err := proxy.Client().Get(proxy.URL + "/health")
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("health status = %d", resp.StatusCode)
	}
	resp, err = proxy.Client().Get(proxy.URL + "/gascity/v1/profiles")
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := resp.Body.Close(); err != nil {
			t.Errorf("close response body: %v", err)
		}
	}()
	var profiles []PublicProfile
	if err := json.NewDecoder(resp.Body).Decode(&profiles); err != nil {
		t.Fatal(err)
	}
	if len(profiles) != 2 || profiles[0].ID != "claude-primary" {
		t.Fatalf("profiles = %#v", profiles)
	}
	encoded, _ := json.Marshal(profiles)
	for _, forbidden := range []string{"TOKEN", "agent.yaml", "secret"} {
		if strings.Contains(string(encoded), forbidden) {
			t.Fatalf("profile response leaked %q: %s", forbidden, encoded)
		}
	}
}

func TestSidecarHandlerResolvesAttachmentWithPrivateCatalog(t *testing.T) {
	catalog := loadFailoverCatalog(t)
	upstream := newAttachmentAPIFake(t, initialFailoverLabels(t, catalog))
	handler, err := NewSidecarHandler(catalog, upstream.server.URL, upstream.server.Client())
	if err != nil {
		t.Fatal(err)
	}
	proxy := newOmnigentHTTPTestServer(handler)
	defer proxy.Close()
	client, err := NewAPIClient(proxy.URL, proxy.Client())
	if err != nil {
		t.Fatal(err)
	}
	descriptor, err := client.ResolveAttachment(context.Background(), AttachmentOpenInput{
		ProfileID: "claude-primary", Workspace: "/work/assigned", Title: "worker's $(literal) title",
		Identity: GasCityIdentity{SessionID: "session-123", Agent: "rig/worker", Rig: "rig", City: "bright-lights"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if descriptor.ConversationID != "conv_attach" || !descriptor.Fresh || descriptor.ProfileID != "claude-primary" || descriptor.ActiveProfile != "claude-primary" {
		t.Fatalf("descriptor = %#v", descriptor)
	}
	upstream.mu.Lock()
	defer upstream.mu.Unlock()
	if upstream.creates != 1 || upstream.destructiveRequests != 0 {
		t.Fatalf("creates=%d destructive=%d", upstream.creates, upstream.destructiveRequests)
	}
	for key, want := range map[string]string{
		"gascity.session.id": "session-123", "gascity.agent": "rig/worker", "gascity.rig": "rig", "gascity.city": "bright-lights",
	} {
		if got := upstream.labels[key]; got != want {
			t.Fatalf("label %s = %q, want %q", key, got, want)
		}
	}
}

func TestSidecarHandlerExactResume404NeverCreatesReplacement(t *testing.T) {
	catalog := loadFailoverCatalog(t)
	upstream := newAttachmentAPIFake(t, initialFailoverLabels(t, catalog))
	upstream.missing = true
	handler, err := NewSidecarHandler(catalog, upstream.server.URL, upstream.server.Client())
	if err != nil {
		t.Fatal(err)
	}
	proxy := newOmnigentHTTPTestServer(handler)
	defer proxy.Close()
	client, err := NewAPIClient(proxy.URL, proxy.Client())
	if err != nil {
		t.Fatal(err)
	}
	_, err = client.ResolveAttachment(context.Background(), AttachmentOpenInput{
		ProfileID: "claude-primary", ConversationID: "conv_missing", Workspace: "/work/assigned",
	})
	var apiErr *APIError
	if !errors.As(err, &apiErr) || apiErr.StatusCode != http.StatusNotFound || apiErr.Code != "conversation_not_found" {
		t.Fatalf("error = %T %v", err, err)
	}
	upstream.mu.Lock()
	defer upstream.mu.Unlock()
	if upstream.creates != 0 {
		t.Fatalf("resume created %d replacement conversations", upstream.creates)
	}
}

func TestSidecarHandlerOwnsTypedFailoverObservation(t *testing.T) {
	catalog := loadFailoverCatalog(t)
	upstream := newFailoverAPIFake(t, initialFailoverLabels(t, catalog))
	handler, err := NewSidecarHandler(catalog, upstream.server.URL, upstream.server.Client())
	if err != nil {
		t.Fatal(err)
	}
	proxy := newOmnigentHTTPTestServer(handler)
	defer proxy.Close()
	client, err := NewAPIClient(proxy.URL, proxy.Client())
	if err != nil {
		t.Fatal(err)
	}
	sequence := int64(41)
	result, err := client.ObserveFailover(context.Background(), "conv_failover", 0, StreamEvent{
		Type: "response.error", Source: "llm", SequenceNumber: &sequence,
		Error: &StreamError{Code: "429", Message: "credential-bearing provider prose must not cross the adapter"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Ignored || result.Exhausted || result.ActiveIndex != 1 || result.ActiveProfileID != "claude-secondary" {
		t.Fatalf("result = %#v", result)
	}
	if result.Transition == nil || result.Transition.FromProfileID != "claude-primary" || result.Transition.ToProfileID != "claude-secondary" || result.Transition.Reason != FailoverRateLimit {
		t.Fatalf("transition = %#v", result.Transition)
	}
	encoded, err := json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{"credential-bearing", "agent_id", "account", "cost"} {
		if strings.Contains(strings.ToLower(string(encoded)), forbidden) {
			t.Fatalf("failover result leaked %q: %s", forbidden, encoded)
		}
	}
	if got := upstream.switchCount(); got != 1 {
		t.Fatalf("switch count = %d", got)
	}
}

func TestSidecarHandlerReportsRedactedLocalStatusAndConversation(t *testing.T) {
	catalog := loadFailoverCatalog(t)
	upstream := newAttachmentAPIFake(t, initialFailoverLabels(t, catalog))
	upstream.mu.Lock()
	upstream.labels[policyVersionLabel] = policyStateVersion
	upstream.labels[policyRequestIDLabel] = "policy-status"
	upstream.labels[policyKindLabel] = "tool.approval"
	upstream.labels[policyStatusLabel] = "pending"
	upstream.labels[policyMailIDLabel] = "mail-status"
	upstream.mu.Unlock()
	handler, err := NewSidecarHandler(catalog, upstream.server.URL, upstream.server.Client())
	if err != nil {
		t.Fatal(err)
	}
	proxy := newOmnigentHTTPTestServer(handler)
	defer proxy.Close()
	client, err := NewAPIClient(proxy.URL, proxy.Client())
	if err != nil {
		t.Fatal(err)
	}
	status, err := client.LocalStatus(context.Background(), "conv_attach")
	if err != nil {
		t.Fatal(err)
	}
	if status.Mode != "local" || !status.Ready || status.Pin.Commit != catalog.Pin.Commit || len(status.Profiles) != 2 {
		t.Fatalf("status = %#v", status)
	}
	if status.Conversation == nil || status.Conversation.ID != "conv_attach" || status.Conversation.Workspace != "/work/assigned" || status.Conversation.ActiveProfileID != "claude-primary" {
		t.Fatalf("conversation status = %#v", status.Conversation)
	}
	if len(status.Conversation.Chain) != 2 || status.Conversation.Chain[1].ID != "claude-secondary" {
		t.Fatalf("conversation chain = %#v", status.Conversation.Chain)
	}
	if status.Conversation.Outcome != OutcomeExists || status.Conversation.Policy == nil || !status.Conversation.Policy.Pending || !status.Conversation.Policy.MailBound {
		t.Fatalf("conversation observable facts = %#v", status.Conversation)
	}
	encoded, err := json.Marshal(status)
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{"api_key", "token=", "password", "agent_path", "environment_value"} {
		if strings.Contains(strings.ToLower(string(encoded)), forbidden) {
			t.Fatalf("status leaked %q: %s", forbidden, encoded)
		}
	}
}

func TestSidecarStatusReportsStaleConversationWithoutCreatingReplacement(t *testing.T) {
	catalog := loadFailoverCatalog(t)
	upstream := newAttachmentAPIFake(t, initialFailoverLabels(t, catalog))
	upstream.missing = true
	handler, err := NewSidecarHandler(catalog, upstream.server.URL, upstream.server.Client())
	if err != nil {
		t.Fatal(err)
	}
	proxy := newOmnigentHTTPTestServer(handler)
	defer proxy.Close()
	client, err := NewAPIClient(proxy.URL, proxy.Client())
	if err != nil {
		t.Fatal(err)
	}
	_, err = client.LocalStatus(context.Background(), "conv_missing")
	var apiErr *APIError
	if !errors.As(err, &apiErr) || apiErr.StatusCode != http.StatusNotFound || apiErr.Code != "conversation_not_found" {
		t.Fatalf("error = %T %v", err, err)
	}
	upstream.mu.Lock()
	defer upstream.mu.Unlock()
	if upstream.creates != 0 {
		t.Fatalf("status created %d replacement conversations", upstream.creates)
	}
}

func TestSidecarPolicyEndpointsRoundTripOnlyForExplicitProfileRecipient(t *testing.T) {
	catalog := loadFailoverCatalog(t)
	profile := catalog.profiles["claude-primary"]
	profile.PolicyMailRecipient = "reviewer"
	catalog.profiles["claude-primary"] = profile
	upstream := newFailoverAPIFake(t, initialFailoverLabels(t, catalog))
	handler, err := NewSidecarHandler(catalog, upstream.server.URL, upstream.server.Client())
	if err != nil {
		t.Fatal(err)
	}
	proxy := newOmnigentHTTPTestServer(handler)
	defer proxy.Close()
	client, err := NewAPIClient(proxy.URL, proxy.Client())
	if err != nil {
		t.Fatal(err)
	}
	descriptor, err := client.OpenPolicyRequest(context.Background(), PolicyRequestInput{
		ConversationID: "conv_failover",
		Request:        PolicyRequest{RequestID: "policy-sidecar", Kind: "approval", Prompt: "Proceed?", Options: []string{"approve", "deny"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if descriptor.Recipient != "reviewer" || descriptor.Status != "pending" {
		t.Fatalf("descriptor = %#v", descriptor)
	}
	descriptor, err = client.BindPolicyMail(context.Background(), PolicyMailBinding{
		ConversationID: "conv_failover", RequestID: descriptor.RequestID, MailID: "mail-sidecar",
	})
	if err != nil || descriptor.MailID != "mail-sidecar" {
		t.Fatalf("bind = %#v error=%v", descriptor, err)
	}
	result, err := client.DeliverPolicyAnswer(context.Background(), PolicyAnswerInput{
		ConversationID: "conv_failover",
		Answer:         PolicyAnswer{RequestID: descriptor.RequestID, MailID: descriptor.MailID, Action: "approve"},
	})
	if err != nil || !result.Delivered {
		t.Fatalf("delivery = %#v error=%v", result, err)
	}
	if got := upstream.policyResponseCount(); got != 1 {
		t.Fatalf("policy responses = %d", got)
	}
}

func TestServeSidecarLifecycleRestartAndNoStateFiles(t *testing.T) {
	cfg := sidecarFixture(t, "healthy")
	run := func() {
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan error, 1)
		go func() { done <- ServeSidecar(ctx, cfg) }()
		waitSidecarSocket(t, cfg.SocketPath)
		client, err := NewUnixAPIClient(cfg.SocketPath)
		if err != nil {
			t.Fatal(err)
		}
		if err := client.Health(context.Background()); err != nil {
			t.Fatalf("Health: %v", err)
		}
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Fatalf("ServeSidecar cancellation: %v", err)
			}
		case <-time.After(3 * time.Second):
			t.Fatal("ServeSidecar did not stop after cancellation")
		}
		if _, err := os.Lstat(cfg.SocketPath); !os.IsNotExist(err) {
			t.Fatalf("socket remains after stop: %v", err)
		}
	}
	run()
	run() // same state and endpoint can restart without stale identity files.
	if err := filepath.Walk(cfg.StateRoot, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		name := strings.ToLower(info.Name())
		if strings.HasSuffix(name, ".pid") || strings.HasSuffix(name, ".lock") || strings.Contains(name, "status.json") {
			t.Errorf("unexpected identity/status file: %s", path)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestServeSidecarLifecycleFailureMatrix(t *testing.T) {
	t.Run("early exit", func(t *testing.T) {
		cfg := sidecarFixture(t, "exit")
		start := time.Now()
		err := ServeSidecar(context.Background(), cfg)
		if err == nil || !strings.Contains(err.Error(), "before readiness") || time.Since(start) > time.Second {
			t.Fatalf("error = %v elapsed=%s", err, time.Since(start))
		}
	})
	t.Run("slow start bounded", func(t *testing.T) {
		cfg := sidecarFixture(t, "slow")
		cfg.StartupTimeout = 120 * time.Millisecond
		cfg.ShutdownTimeout = 100 * time.Millisecond
		// ServeSidecar bounds graceful shutdown by ShutdownTimeout, then gives
		// the SIGKILL/reap path up to one second. Include both windows plus
		// scheduler headroom so host load cannot turn this boundedness proof
		// into a sub-second wall-clock flake.
		maxElapsed := cfg.StartupTimeout + cfg.ShutdownTimeout + time.Second + 500*time.Millisecond
		start := time.Now()
		err := ServeSidecar(context.Background(), cfg)
		if err == nil || !strings.Contains(err.Error(), "did not become ready") || time.Since(start) > maxElapsed {
			t.Fatalf("error = %v elapsed=%s", err, time.Since(start))
		}
	})
	t.Run("cancellation during start", func(t *testing.T) {
		cfg := sidecarFixture(t, "slow")
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan error, 1)
		go func() { done <- ServeSidecar(ctx, cfg) }()
		waitForTestFile(t, filepath.Join(cfg.StateRoot, "child.started"))
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Fatalf("canceled startup = %v, want clean stop", err)
			}
		case <-time.After(time.Second):
			t.Fatal("canceled startup hung")
		}
	})
	t.Run("stale socket replaced", func(t *testing.T) {
		cfg := sidecarFixture(t, "healthy")
		listener, err := net.Listen("unix", cfg.SocketPath)
		if err != nil {
			t.Fatal(err)
		}
		if err := listener.Close(); err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan error, 1)
		go func() { done <- ServeSidecar(ctx, cfg) }()
		waitSidecarSocket(t, cfg.SocketPath)
		cancel()
		if err := <-done; err != nil {
			t.Fatal(err)
		}
	})
	t.Run("active socket collision", func(t *testing.T) {
		cfg := sidecarFixture(t, "healthy")
		listener, err := net.Listen("unix", cfg.SocketPath)
		if err != nil {
			t.Fatal(err)
		}
		defer func() {
			if err := listener.Close(); err != nil {
				t.Errorf("close listener: %v", err)
			}
		}()
		err = ServeSidecar(context.Background(), cfg)
		if err == nil || !strings.Contains(err.Error(), "already active") {
			t.Fatalf("error = %v, want active collision", err)
		}
	})
	t.Run("loopback port collision", func(t *testing.T) {
		cfg := sidecarFixture(t, "healthy")
		listener, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		defer func() {
			if err := listener.Close(); err != nil {
				t.Errorf("close listener: %v", err)
			}
		}()
		cfg.LoopbackPort = listener.Addr().(*net.TCPAddr).Port
		// The child is this race-instrumented test binary; allow its runtime
		// startup to complete so the assertion observes the bind failure rather
		// than the parent's readiness deadline.
		cfg.StartupTimeout = 5 * time.Second
		err = ServeSidecar(context.Background(), cfg)
		if err == nil || !strings.Contains(err.Error(), "before readiness") {
			t.Fatalf("error = %v, want child bind failure", err)
		}
	})
	t.Run("child crash observed", func(t *testing.T) {
		cfg := sidecarFixture(t, "crash")
		port, err := reserveLoopbackPort()
		if err != nil {
			t.Fatal(err)
		}
		cfg.LoopbackPort = port
		done := make(chan error, 1)
		go func() { done <- ServeSidecar(context.Background(), cfg) }()
		waitSidecarSocket(t, cfg.SocketPath)
		request, err := http.NewRequestWithContext(context.Background(), http.MethodPost, fmt.Sprintf("http://127.0.0.1:%d/crash", port), nil)
		if err != nil {
			t.Fatal(err)
		}
		response, requestErr := localHTTPClient(time.Second).Do(request)
		if requestErr == nil {
			_ = response.Body.Close()
		}
		select {
		case err := <-done:
			if err == nil || !strings.Contains(err.Error(), "exited unexpectedly") {
				t.Fatalf("error = %v", err)
			}
		case <-time.After(2 * time.Second):
			t.Fatal("child crash not observed")
		}
	})
}

func TestServeSidecarCancellationDoesNotTouchUnrelatedProcess(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("process-group fixture is Unix-specific")
	}
	unrelated := exec.Command("/bin/sh", "-c", "sleep 5")
	unrelated.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := unrelated.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = syscall.Kill(-unrelated.Process.Pid, syscall.SIGKILL)
		_ = unrelated.Wait()
	})
	cfg := sidecarFixture(t, "healthy")
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- ServeSidecar(ctx, cfg) }()
	waitSidecarSocket(t, cfg.SocketPath)
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if err := syscall.Kill(unrelated.Process.Pid, 0); err != nil {
		t.Fatalf("unrelated process was affected: %v", err)
	}
}

func TestSidecarChildHelper(_ *testing.T) {
	behavior := os.Getenv("GC_OMNIGENT_TEST_CHILD")
	if behavior == "" {
		return
	}
	if behavior == "exit" {
		os.Exit(17)
	}
	startedFile := os.Getenv("GC_OMNIGENT_TEST_STARTED_FILE")
	if startedFile == "" || os.WriteFile(startedFile, []byte("started"), 0o600) != nil {
		os.Exit(22)
	}
	if behavior == "slow" {
		stopping := make(chan os.Signal, 1)
		signal.Notify(stopping, syscall.SIGINT, syscall.SIGTERM)
		<-stopping
		os.Exit(0)
	}
	port := ""
	for i, arg := range os.Args {
		if arg == "--port" && i+1 < len(os.Args) {
			port = os.Args[i+1]
		}
	}
	if port == "" {
		os.Exit(18)
	}
	listener, err := net.Listen("tcp", "127.0.0.1:"+port)
	if err != nil {
		os.Exit(19)
	}
	crashRequested := make(chan struct{}, 1)
	server := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if behavior == "crash" && r.URL.Path == "/crash" {
			w.WriteHeader(http.StatusNoContent)
			if flusher, ok := w.(http.Flusher); ok {
				flusher.Flush()
			}
			select {
			case crashRequested <- struct{}{}:
			default:
			}
			return
		}
		if r.URL.Path != "/health" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"status":"ok"}`)
	})}
	if behavior == "crash" {
		serveDone := make(chan error, 1)
		go func() { serveDone <- server.Serve(listener) }()
		select {
		case <-crashRequested:
			os.Exit(20)
		case <-serveDone:
			os.Exit(21)
		}
	}
	if err := server.Serve(listener); err != nil && err != http.ErrServerClosed {
		os.Exit(21)
	}
	os.Exit(0)
}

func sidecarFixture(t *testing.T, behavior string) SidecarConfig {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("sidecar process fixture is Unix-specific")
	}
	root := t.TempDir()
	configDir := filepath.Join(root, "config")
	if err := os.MkdirAll(configDir, 0o700); err != nil {
		t.Fatal(err)
	}
	testBinary, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	if strings.ContainsAny(testBinary, "'\n") {
		t.Fatalf("test binary path cannot be safely embedded: %q", testBinary)
	}
	wrapper := filepath.Join(root, "omnigent")
	startedFile := filepath.Join(root, "child.started")
	body := fmt.Sprintf("#!/bin/sh\nif [ \"$1\" = \"--version\" ]; then echo 'omnigent 0.10.0.dev0 (2aba5079)'; exit 0; fi\nexport GC_OMNIGENT_TEST_CHILD='%s'\nexport GC_OMNIGENT_TEST_STARTED_FILE='%s'\nexec '%s' -test.run='^TestSidecarChildHelper$' -- \"$@\"\n", behavior, startedFile, testBinary)
	if err := os.WriteFile(wrapper, []byte(body), 0o700); err != nil {
		t.Fatal(err)
	}
	agent := filepath.Join(configDir, "agent.yaml")
	if err := os.WriteFile(agent, []byte("name: fixture-agent\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256([]byte(body))
	catalog := fmt.Sprintf(`version: 1
omnigent:
  commit: 2aba5079d4d3a2a84d8c9927884fc4b8ce0eeecc
  package_version: 0.10.0.dev0
  executable: %s
  sha256: sha256:%x
profiles:
  fixture:
    display_name: Fixture
    blurb: Deterministic offline fixture.
    harness: codex
    backend: loopback
    network: offline
    agent: agent.yaml
`, wrapper, sum)
	catalogPath := filepath.Join(configDir, "profiles.yaml")
	if err := os.WriteFile(catalogPath, []byte(catalog), 0o600); err != nil {
		t.Fatal(err)
	}
	socketFile, err := os.CreateTemp("", "og-sidecar-*.sock")
	if err != nil {
		t.Fatal(err)
	}
	socketPath := socketFile.Name()
	_ = socketFile.Close()
	_ = os.Remove(socketPath)
	t.Cleanup(func() { _ = os.Remove(socketPath) })
	return SidecarConfig{
		StateRoot: root, CatalogPath: catalogPath, SocketPath: socketPath,
		StartupTimeout: time.Second, ShutdownTimeout: 250 * time.Millisecond,
	}
}

func waitSidecarSocket(t *testing.T, socketPath string) {
	t.Helper()
	deadline := time.NewTimer(2 * time.Second)
	defer deadline.Stop()
	ticker := time.NewTicker(20 * time.Millisecond)
	defer ticker.Stop()
	for {
		conn, err := net.DialTimeout("unix", socketPath, 25*time.Millisecond)
		if err == nil {
			_ = conn.Close()
			return
		}
		select {
		case <-deadline.C:
			t.Fatalf("sidecar socket %s did not become ready", socketPath)
		case <-ticker.C:
		}
	}
}

func waitForTestFile(t *testing.T, path string) {
	t.Helper()
	deadline := time.NewTimer(2 * time.Second)
	defer deadline.Stop()
	ticker := time.NewTicker(20 * time.Millisecond)
	defer ticker.Stop()
	for {
		if _, err := os.Stat(path); err == nil {
			return
		}
		select {
		case <-deadline.C:
			t.Fatalf("test file %s did not appear", path)
		case <-ticker.C:
		}
	}
}

func envListMap(values []string) map[string]string {
	out := make(map[string]string, len(values))
	for _, value := range values {
		key, item, ok := strings.Cut(value, "=")
		if ok {
			out[key] = item
		}
	}
	return out
}

func sortedMapKeys(values map[string]string) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	slices.Sort(keys)
	return keys
}

func infoMode(info os.FileInfo) os.FileMode {
	if info == nil {
		return 0
	}
	return info.Mode().Perm()
}
