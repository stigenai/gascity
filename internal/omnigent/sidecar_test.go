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
	"sync"
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
	primaryAgent := filepath.Join(configDir, "agents", "primary.yaml")
	if err := os.WriteFile(primaryAgent, []byte("name: claude-primary\nprompt: work\n"), 0o600); err != nil {
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
	resolvedPrimaryAgent, err := filepath.EvalSymlinks(primaryAgent)
	if err != nil {
		t.Fatal(err)
	}
	args := prepared.Plan.Args
	joined := strings.Join(args, " ")
	for _, want := range []string{
		"server", "--host 127.0.0.1", "--port 43123", "--database-uri sqlite:///",
		"--conversation-database-uri sqlite:///", "--artifact-location", "--config", "--no-open", "--agent " + resolvedPrimaryAgent,
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
	hostJoined := strings.Join(prepared.Plan.HostArgs, " ")
	if hostJoined != "host --server http://127.0.0.1:43123 --non-interactive" {
		t.Fatalf("host args = %q", hostJoined)
	}
	if strings.Contains(hostJoined, "--background") {
		t.Fatalf("host must remain foreground-supervised: %v", prepared.Plan.HostArgs)
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

func TestPrepareSidecarAcceptsOnlyReadOnlyProviderStagedCatalog(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell fixture is Unix-specific")
	}
	stateRoot := t.TempDir()
	stagedRoot := t.TempDir()
	executable := filepath.Join(stateRoot, "omnigent")
	body := "#!/bin/sh\necho 'omnigent 0.10.0.dev0 (2aba5079)'\n"
	if err := os.WriteFile(executable, []byte(body), 0o700); err != nil {
		t.Fatal(err)
	}
	agentPath := filepath.Join(stagedRoot, "agent.yaml")
	if err := os.WriteFile(agentPath, []byte("name: staged\nprompt: work\n"), 0o444); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256([]byte(body))
	catalogPath := filepath.Join(stagedRoot, "profiles.yaml")
	catalog := fmt.Sprintf(`version: 1
omnigent:
  commit: 2aba5079d4d3a2a84d8c9927884fc4b8ce0eeecc
  package_version: 0.10.0.dev0
  executable: %s
  sha256: sha256:%x
profiles:
  staged:
    display_name: Staged
    blurb: Immutable staged profile.
    harness: codex
    backend: loopback
    network: offline
    agent: agent.yaml
`, executable, sum)
	if err := os.WriteFile(catalogPath, []byte(catalog), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg := SidecarConfig{StateRoot: stateRoot, CatalogPath: catalogPath, ImmutableCatalog: true}
	if _, err := PrepareSidecar(context.Background(), cfg, 43123); err == nil || !strings.Contains(err.Error(), "catalog must be read-only") {
		t.Fatalf("mutable catalog error = %v", err)
	}
	if err := os.Chmod(catalogPath, 0o444); err != nil {
		t.Fatal(err)
	}
	if _, err := PrepareSidecar(context.Background(), cfg, 43123); err != nil {
		t.Fatalf("PrepareSidecar(read-only staged catalog): %v", err)
	}
	firstDigest, err := CatalogBundleSHA256(catalogPath)
	if err != nil || !digestPattern.MatchString(firstDigest) {
		t.Fatalf("CatalogBundleSHA256 = %q, %v", firstDigest, err)
	}
	if err := os.Chmod(agentPath, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := PrepareSidecar(context.Background(), cfg, 43123); err == nil || !strings.Contains(err.Error(), "profile agent must be read-only") {
		t.Fatalf("mutable profile agent error = %v", err)
	}
	if err := os.Chmod(agentPath, 0o444); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(agentPath, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(agentPath, []byte("name: staged\nprompt: changed\n"), 0o444); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(agentPath, 0o444); err != nil {
		t.Fatal(err)
	}
	secondDigest, err := CatalogBundleSHA256(catalogPath)
	if err != nil || secondDigest == firstDigest {
		t.Fatalf("catalog bundle digest after agent change = %q, %v; want change", secondDigest, err)
	}
	linkPath := filepath.Join(stagedRoot, "catalog-link.yaml")
	if err := os.Symlink(catalogPath, linkPath); err != nil {
		t.Fatal(err)
	}
	cfg.CatalogPath = linkPath
	if _, err := PrepareSidecar(context.Background(), cfg, 43123); err == nil || !strings.Contains(err.Error(), "non-symlink") {
		t.Fatalf("symlink catalog error = %v", err)
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
		finished := false
		t.Cleanup(func() {
			if finished {
				return
			}
			cancel()
			select {
			case <-done:
			case <-time.After(3 * time.Second):
				t.Errorf("timed out cleaning up sidecar fixture")
			}
		})
		waitSidecarSocket(t, cfg.SocketPath)
		waitForTestFile(t, filepath.Join(cfg.StateRoot, "host.started"))
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
			finished = true
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

func TestServeSidecarRetainsExactConversationAndTranscriptAcrossRestart(t *testing.T) {
	cfg := sidecarFixture(t, "persistent")
	workspace := filepath.Join(cfg.StateRoot, "workspace")
	if err := os.Mkdir(workspace, 0o700); err != nil {
		t.Fatal(err)
	}
	identity := GasCityIdentity{
		SessionID: "session-persisted", Agent: "rig/worker", Rig: "rig", City: "city",
	}
	input := AttachmentOpenInput{
		ProfileID: "fixture", Workspace: workspace, Title: "durable transcript", Identity: identity,
	}

	client, stop := startSidecarFixture(t, cfg)
	descriptor, err := client.ResolveAttachment(context.Background(), input)
	if err != nil {
		t.Fatalf("create attachment: %v", err)
	}
	if descriptor.ConversationID != "conv_persisted" || !descriptor.Fresh {
		t.Fatalf("fresh descriptor = %#v", descriptor)
	}
	queued, err := client.PostMessage(context.Background(), descriptor.ConversationID, "retained transcript item")
	if err != nil || !queued {
		t.Fatalf("PostMessage queued=%v error=%v", queued, err)
	}
	assertPersistentSidecarTranscript(t, client, descriptor.ConversationID, "retained transcript item")
	stop()

	client, stop = startSidecarFixture(t, cfg)
	resume := input
	resume.ConversationID = descriptor.ConversationID
	resumed, err := client.ResolveAttachment(context.Background(), resume)
	if err != nil {
		t.Fatalf("resume attachment after complete restart: %v", err)
	}
	if resumed.ConversationID != descriptor.ConversationID || resumed.Fresh {
		t.Fatalf("resumed descriptor = %#v, want exact non-fresh conversation", resumed)
	}
	assertPersistentSidecarTranscript(t, client, descriptor.ConversationID, "retained transcript item")
	status, err := client.LocalStatus(context.Background(), descriptor.ConversationID)
	if err != nil {
		t.Fatalf("LocalStatus after restart: %v", err)
	}
	if status.Conversation == nil || status.Conversation.ID != descriptor.ConversationID || status.Conversation.Outcome != OutcomeExists {
		t.Fatalf("typed conversation status after restart = %#v", status.Conversation)
	}

	changedWorkspace := resume
	changedWorkspace.Workspace = filepath.Join(cfg.StateRoot, "other-workspace")
	if err := os.Mkdir(changedWorkspace.Workspace, 0o700); err != nil {
		t.Fatal(err)
	}
	assertSidecarAttachmentError(t, client, changedWorkspace, http.StatusBadGateway, "workspace")
	changedIdentity := resume
	changedIdentity.Identity.Agent = "rig/other-worker"
	assertSidecarAttachmentError(t, client, changedIdentity, http.StatusBadGateway, "identity label")
	missingConversation := resume
	missingConversation.ConversationID = "conv_absent"
	assertSidecarAttachmentError(t, client, missingConversation, http.StatusNotFound, "does not exist")
	assertPersistentSidecarTranscript(t, client, descriptor.ConversationID, "retained transcript item")
	if err := client.Health(context.Background()); err != nil {
		t.Fatalf("sidecar lost liveness after attachment detach/failures: %v", err)
	}
	stop()

	if err := os.Remove(filepath.Join(cfg.StateRoot, "data", "chat.db")); err != nil {
		t.Fatalf("remove retained conversation database: %v", err)
	}
	client, stop = startSidecarFixture(t, cfg)
	defer stop()
	assertSidecarAttachmentError(t, client, resume, http.StatusNotFound, "does not exist")
}

func startSidecarFixture(t *testing.T, cfg SidecarConfig) (*APIClient, func()) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- ServeSidecar(ctx, cfg) }()
	stopped := false
	stop := func() {
		t.Helper()
		if stopped {
			return
		}
		stopped = true
		cancel()
		timer := time.NewTimer(3 * time.Second)
		defer timer.Stop()
		select {
		case err := <-done:
			if err != nil {
				t.Fatalf("ServeSidecar cancellation: %v", err)
			}
		case <-timer.C:
			t.Fatal("ServeSidecar did not stop after cancellation")
		}
	}
	t.Cleanup(stop)
	waitSidecarSocket(t, cfg.SocketPath)
	client, err := NewUnixAPIClient(cfg.SocketPath)
	if err != nil {
		stop()
		t.Fatal(err)
	}
	if err := client.Health(context.Background()); err != nil {
		stop()
		t.Fatalf("sidecar health: %v", err)
	}
	return client, stop
}

func assertPersistentSidecarTranscript(t *testing.T, client *APIClient, conversationID, text string) {
	t.Helper()
	snapshot, err := client.GetSession(context.Background(), conversationID)
	if err != nil {
		t.Fatalf("GetSession(%q): %v", conversationID, err)
	}
	if snapshot.ID != conversationID || len(snapshot.Items) != 1 || len(snapshot.Items[0].Content) != 1 || snapshot.Items[0].Content[0].Text != text {
		t.Fatalf("retained session = %#v", snapshot)
	}
}

func assertSidecarAttachmentError(t *testing.T, client *APIClient, input AttachmentOpenInput, status int, contains string) {
	t.Helper()
	_, err := client.ResolveAttachment(context.Background(), input)
	var apiErr *APIError
	if !errors.As(err, &apiErr) || apiErr.StatusCode != status || !strings.Contains(apiErr.Message, contains) {
		t.Fatalf("ResolveAttachment error = %T %v, want HTTP %d containing %q", err, err, status, contains)
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
		finished := false
		t.Cleanup(func() {
			if finished {
				return
			}
			cancel()
			select {
			case <-done:
			case <-time.After(3 * time.Second):
				t.Errorf("timed out cleaning up canceled-start sidecar fixture")
			}
		})
		waitForTestFile(t, filepath.Join(cfg.StateRoot, "child.started"))
		cancel()
		select {
		case err := <-done:
			finished = true
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
		finished := false
		t.Cleanup(func() {
			if finished {
				return
			}
			cancel()
			select {
			case <-done:
			case <-time.After(3 * time.Second):
				t.Errorf("timed out cleaning up stale-socket sidecar fixture")
			}
		})
		waitSidecarSocket(t, cfg.SocketPath)
		cancel()
		err = <-done
		finished = true
		if err != nil {
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
	isHost := slices.Contains(os.Args, "host")
	startedFile := os.Getenv("GC_OMNIGENT_TEST_STARTED_FILE")
	if startedFile == "" {
		os.Exit(22)
	}
	if isHost {
		startedFile = filepath.Join(filepath.Dir(startedFile), "host.started")
	}
	if os.WriteFile(startedFile, []byte("started"), 0o600) != nil {
		os.Exit(22)
	}
	if isHost {
		stopping := make(chan os.Signal, 1)
		signal.Notify(stopping, syscall.SIGINT, syscall.SIGTERM)
		<-stopping
		os.Exit(0)
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
	if behavior == "persistent" {
		if err := runPersistentSidecarChild(listener, os.Args); err != nil {
			os.Exit(23)
		}
		os.Exit(0)
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
		if r.URL.Path == "/v1/hosts" {
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"hosts":[{"host_id":"host_fixture","owner":"local","status":"online"}]}`)
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

type persistentSidecarDatabase struct {
	Session *Session `json:"session,omitempty"`
}

type persistentSidecarChild struct {
	mu       sync.Mutex
	path     string
	database persistentSidecarDatabase
}

func runPersistentSidecarChild(listener net.Listener, args []string) error {
	databasePath, err := persistentSidecarDatabasePath(args)
	if err != nil {
		return err
	}
	child := &persistentSidecarChild{path: databasePath}
	data, err := os.ReadFile(databasePath)
	if err == nil {
		if err := json.Unmarshal(data, &child.database); err != nil {
			return err
		}
	} else if !os.IsNotExist(err) {
		return err
	}
	server := &http.Server{Handler: http.HandlerFunc(child.serveHTTP)}
	if err := server.Serve(listener); err != nil && err != http.ErrServerClosed {
		return err
	}
	return nil
}

func persistentSidecarDatabasePath(args []string) (string, error) {
	for i, arg := range args {
		if arg != "--conversation-database-uri" || i+1 >= len(args) {
			continue
		}
		const prefix = "sqlite:///"
		uri := args[i+1]
		if !strings.HasPrefix(uri, prefix) {
			return "", fmt.Errorf("unsupported conversation database URI %q", uri)
		}
		path := strings.TrimPrefix(uri, prefix)
		if !filepath.IsAbs(path) {
			return "", fmt.Errorf("conversation database path %q is not absolute", path)
		}
		return path, nil
	}
	return "", errors.New("conversation database URI is required")
}

func (c *persistentSidecarChild) serveHTTP(w http.ResponseWriter, r *http.Request) {
	switch {
	case r.Method == http.MethodGet && r.URL.Path == "/health":
		writePersistentSidecarJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	case r.Method == http.MethodGet && r.URL.Path == "/v1/hosts":
		writePersistentSidecarJSON(w, http.StatusOK, map[string]any{"hosts": []map[string]string{{"host_id": "host_persisted", "owner": "local", "status": "online"}}})
	case r.Method == http.MethodGet && r.URL.Path == "/v1/agents":
		writePersistentSidecarJSON(w, http.StatusOK, map[string]any{
			"data": []Agent{{ID: "ag_fixture", Name: "fixture-agent", Harness: "codex"}}, "has_more": false,
		})
	case r.Method == http.MethodPost && r.URL.Path == "/v1/sessions":
		var input createSessionRequest
		if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
			writePersistentSidecarJSON(w, http.StatusBadRequest, map[string]any{"error": map[string]string{"code": "invalid_request", "message": "invalid request"}})
			return
		}
		c.mu.Lock()
		if c.database.Session != nil {
			c.mu.Unlock()
			writePersistentSidecarJSON(w, http.StatusConflict, map[string]any{"error": map[string]string{"code": "already_exists", "message": "fixture conversation already exists"}})
			return
		}
		c.database.Session = &Session{
			ID: "conv_persisted", AgentID: input.AgentID, AgentName: "fixture-agent", Status: "idle",
			Workspace: input.Workspace, Labels: cloneStringMap(input.Labels), Items: []SessionItem{},
		}
		session := clonePersistentSession(c.database.Session)
		err := c.saveLocked()
		c.mu.Unlock()
		if err != nil {
			writePersistentSidecarJSON(w, http.StatusInternalServerError, map[string]any{"error": map[string]string{"code": "write_failed", "message": "database write failed"}})
			return
		}
		writePersistentSidecarJSON(w, http.StatusOK, session)
	case strings.HasPrefix(r.URL.Path, "/v1/sessions/"):
		c.serveSessionHTTP(w, r)
	default:
		http.NotFound(w, r)
	}
}

func (c *persistentSidecarChild) serveSessionHTTP(w http.ResponseWriter, r *http.Request) {
	remainder := strings.TrimPrefix(r.URL.Path, "/v1/sessions/")
	conversationID, suffix, _ := strings.Cut(remainder, "/")
	c.mu.Lock()
	exists := c.database.Session != nil && c.database.Session.ID == conversationID
	c.mu.Unlock()
	if !exists {
		writePersistentSidecarJSON(w, http.StatusNotFound, map[string]any{"error": map[string]string{"code": "conversation_not_found", "message": "missing"}})
		return
	}
	if r.Method == http.MethodGet && suffix == "stream" {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		w.(http.Flusher).Flush()
		<-r.Context().Done()
		return
	}
	if r.Method == http.MethodGet && suffix == "" {
		c.mu.Lock()
		session := clonePersistentSession(c.database.Session)
		c.mu.Unlock()
		writePersistentSidecarJSON(w, http.StatusOK, session)
		return
	}
	if r.Method == http.MethodPost && suffix == "events" {
		var input sessionEventRequest
		if err := json.NewDecoder(r.Body).Decode(&input); err != nil || input.Type != "message" || len(input.Data.Content) == 0 {
			writePersistentSidecarJSON(w, http.StatusBadRequest, map[string]any{"error": map[string]string{"code": "invalid_event", "message": "invalid event"}})
			return
		}
		c.mu.Lock()
		c.database.Session.Items = append(c.database.Session.Items, SessionItem{
			ID: fmt.Sprintf("item_%d", len(c.database.Session.Items)+1), Type: "message",
			Role: input.Data.Role, Content: append([]ContentBlock(nil), input.Data.Content...),
		})
		err := c.saveLocked()
		c.mu.Unlock()
		if err != nil {
			writePersistentSidecarJSON(w, http.StatusInternalServerError, map[string]any{"error": map[string]string{"code": "write_failed", "message": "database write failed"}})
			return
		}
		writePersistentSidecarJSON(w, http.StatusOK, map[string]bool{"queued": true})
		return
	}
	http.NotFound(w, r)
}

func (c *persistentSidecarChild) saveLocked() error {
	data, err := json.Marshal(c.database)
	if err != nil {
		return err
	}
	temporary := c.path + ".tmp"
	if err := os.WriteFile(temporary, data, 0o600); err != nil {
		return err
	}
	return os.Rename(temporary, c.path)
}

func clonePersistentSession(session *Session) Session {
	clone := *session
	clone.Labels = cloneStringMap(session.Labels)
	clone.Items = append([]SessionItem(nil), session.Items...)
	for i := range clone.Items {
		clone.Items[i].Content = append([]ContentBlock(nil), session.Items[i].Content...)
	}
	return clone
}

func writePersistentSidecarJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
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
	if err := os.WriteFile(agent, []byte("name: fixture-agent\nprompt: work\n"), 0o600); err != nil {
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
		StartupTimeout: 5 * time.Second, ShutdownTimeout: 250 * time.Millisecond,
	}
}

func waitSidecarSocket(t *testing.T, socketPath string) {
	t.Helper()
	deadline := time.NewTimer(6 * time.Second)
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
	deadline := time.NewTimer(6 * time.Second)
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
