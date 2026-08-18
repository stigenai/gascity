//go:build integration

package ssh

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"

	"github.com/gastownhall/gascity/internal/runtime"
)

// localCommandRunner executes the remote argv directly. It exercises the exact
// POSIX-shell state protocol without requiring an SSH daemon.
type localCommandRunner struct{}

func (localCommandRunner) run(ctx context.Context, _ Endpoint, argv []string, stdin []byte) ([]byte, int, error) {
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.Stdin = bytes.NewReader(stdin)
	var output bytes.Buffer
	cmd.Stdout = &output
	cmd.Stderr = &output
	err := cmd.Run()
	if err == nil {
		return output.Bytes(), 0, nil
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		return output.Bytes(), exitErr.ExitCode(), nil
	}
	return output.Bytes(), -1, err
}

type lostStateResponseRunner struct {
	delegate runner
	script   string
	mu       sync.Mutex
	lost     bool
}

func (r *lostStateResponseRunner) run(ctx context.Context, ep Endpoint, argv []string, stdin []byte) ([]byte, int, error) {
	out, code, err := r.delegate.run(ctx, ep, argv, stdin)
	if err != nil || code != 0 || len(argv) < 3 || argv[0] != "sh" || argv[2] != r.script {
		return out, code, err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.lost {
		return out, code, nil
	}
	r.lost = true
	return nil, -1, context.DeadlineExceeded
}

type capsuleLifecycleHostRunner struct {
	mu       sync.Mutex
	sessions map[string]string
	stateUID map[string]string
}

func newCapsuleLifecycleHostRunner() *capsuleLifecycleHostRunner {
	return &capsuleLifecycleHostRunner{sessions: make(map[string]string), stateUID: make(map[string]string)}
}

func (r *capsuleLifecycleHostRunner) run(ctx context.Context, ep Endpoint, argv []string, stdin []byte) ([]byte, int, error) {
	if len(argv) >= 3 && argv[0] == "sh" && argv[1] == "-c" {
		switch argv[2] {
		case remoteEnsureCapsuleStateScript, remoteOpenCapsuleStateScript, remoteListCapsuleStateScript,
			remotePurgeCapsuleStateScript, remoteEnsureOwnedDirScript, remoteAtomicStageScript, remoteStageVerifyScript:
			return (localCommandRunner{}).run(ctx, ep, argv, stdin)
		}
	}
	if len(argv) >= 2 && argv[0] == "tmux" {
		r.mu.Lock()
		defer r.mu.Unlock()
		switch argv[1] {
		case "has-session":
			name := argv[len(argv)-1]
			if _, ok := r.sessions[name]; ok {
				return nil, 0, nil
			}
			return nil, 1, nil
		case "new-session":
			name := valueAfter(argv, "-s")
			if _, exists := r.sessions[name]; exists {
				return nil, 1, nil
			}
			r.sessions[name] = argv[len(argv)-1]
			return nil, 0, nil
		case "set-option":
			name := valueAfter(argv, "-t")
			if len(argv) >= 2 && argv[len(argv)-2] == capsuleStateTMUXOption {
				r.stateUID[name] = argv[len(argv)-1]
			}
			return nil, 0, nil
		case "list-sessions":
			if len(r.sessions) == 0 {
				return nil, 1, nil
			}
			names := make([]string, 0, len(r.sessions))
			for name := range r.sessions {
				names = append(names, name)
			}
			sort.Strings(names)
			var lines []string
			for _, name := range names {
				lines = append(lines, name+"\t"+r.stateUID[name])
			}
			return []byte(strings.Join(lines, "\n")), 0, nil
		case "kill-session":
			name := argv[len(argv)-1]
			delete(r.sessions, name)
			delete(r.stateUID, name)
			return nil, 0, nil
		case "respawn-pane":
			name := valueAfter(argv, "-t")
			r.sessions[name] = argv[len(argv)-1]
			return nil, 0, nil
		}
	}
	return successfulCapsulePreflightResponse(argv)
}

func (r *capsuleLifecycleHostRunner) loseTmuxServer() {
	r.mu.Lock()
	defer r.mu.Unlock()
	clear(r.sessions)
	clear(r.stateUID)
}

func (r *capsuleLifecycleHostRunner) command(name string) string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.sessions[name]
}

func valueAfter(argv []string, flag string) string {
	for i := 0; i+1 < len(argv); i++ {
		if argv[i] == flag {
			return argv[i+1]
		}
	}
	return ""
}

func TestSSHCapsuleStatePersistsAcrossProviderRestartAndPurgesExplicitly(t *testing.T) {
	t.Parallel()
	root := filepath.Join(t.TempDir(), "state root with spaces 多")
	if err := os.Mkdir(root, 0o700); err != nil {
		t.Fatal(err)
	}
	provider := providerWithLocalCapsuleStateRoot(t, root)
	key, err := runtime.NewCapsuleKey("ssh://box/city", "session 多")
	if err != nil {
		t.Fatal(err)
	}
	ref, created, err := provider.EnsureCapsuleState(context.Background(), key)
	if err != nil || !created {
		t.Fatalf("EnsureCapsuleState = %#v, %t, %v; want created", ref, created, err)
	}
	if ref.Provider != string(runtime.SecretProviderSSH) || ref.ResourceID != key.ResourceStem() || ref.ResourceUID == "" || ref.MountPath != root {
		t.Fatalf("state reference = %#v", ref)
	}
	marker := filepath.Join(root, ref.ResourceID, "data", "conversation.sqlite")
	if err := os.MkdirAll(filepath.Dir(marker), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(marker, []byte("conversation-exact"), 0o600); err != nil {
		t.Fatal(err)
	}

	// A new Provider models controller/SSH connection restart. The allocation
	// is discovered from remote ground truth, not process memory.
	restarted := providerWithLocalCapsuleStateRoot(t, root)
	reopened, created, err := restarted.EnsureCapsuleState(context.Background(), key)
	if err != nil || created || reopened != ref {
		t.Fatalf("restarted EnsureCapsuleState = %#v, %t, %v; want %#v", reopened, created, err, ref)
	}
	if got, ok, err := restarted.OpenCapsuleState(context.Background(), key); err != nil || !ok || got != ref {
		t.Fatalf("OpenCapsuleState = %#v, %t, %v; want %#v", got, ok, err, ref)
	}
	if data, err := os.ReadFile(marker); err != nil || string(data) != "conversation-exact" {
		t.Fatalf("persistent conversation marker = %q, %v", data, err)
	}

	if err := restarted.PurgeCapsuleState(context.Background(), key); err != nil {
		t.Fatalf("PurgeCapsuleState: %v", err)
	}
	if _, ok, err := restarted.OpenCapsuleState(context.Background(), key); err != nil || ok {
		t.Fatalf("OpenCapsuleState after purge = ok=%t err=%v", ok, err)
	}
	if err := restarted.PurgeCapsuleState(context.Background(), key); err != nil {
		t.Fatalf("idempotent PurgeCapsuleState: %v", err)
	}
}

func TestSSHCapsuleConversationStateSurvivesDisconnectTmuxLossAndControllerRestart(t *testing.T) {
	t.Parallel()
	base := t.TempDir()
	stateRoot := filepath.Join(base, "state")
	runRoot := filepath.Join(base, "run")
	catalogRoot := filepath.Join(base, "catalog")
	workspace := filepath.Join(base, "workspace")
	for _, directory := range []string{stateRoot, runRoot, catalogRoot, workspace} {
		if err := os.Mkdir(directory, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	host := newCapsuleLifecycleHostRunner()
	newController := func() *Provider {
		return &Provider{
			conn: &Conn{ep: Endpoint{Host: "disposable-host"}, run: host}, capsuleStateRoot: stateRoot,
			workDirs: make(map[string]string),
		}
	}
	firstController := newController()
	key, _ := runtime.NewCapsuleKey("ssh/disposable/city", "session-resume")
	ref, _, err := firstController.EnsureCapsuleState(context.Background(), key)
	if err != nil {
		t.Fatalf("EnsureCapsuleState: %v", err)
	}
	cfg := testSSHCapsuleConfig(t)
	cfg.WorkDir = workspace
	cfg.Capsule.Key = key
	cfg.Capsule.State = ref
	cfg.Capsule.RunRoot = runRoot
	cfg.Capsule.SocketPath = filepath.Join(runRoot, "sidecar.sock")
	cfg.Capsule.CatalogMountPath = catalogRoot
	cfg.Capsule.Command = []string{
		"gc", "omnigent", "attach", "--mode", "capsule",
		"--socket", cfg.Capsule.SocketPath, "--state-root", stateRoot,
		"--catalog", filepath.Join(catalogRoot, "profiles.yaml"), "--profile", "claude-primary",
	}
	if err := firstController.Start(context.Background(), "session-resume", cfg); err != nil {
		t.Fatalf("first Start: %v", err)
	}
	physicalState, _, _, _ := firstController.capsulePhysicalPaths(cfg.Capsule)
	conversationDB := filepath.Join(physicalState, "data", "conversation.sqlite")
	if err := os.MkdirAll(filepath.Dir(conversationDB), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(conversationDB, []byte("conversation_id=conv_exact\nturn=retained"), 0o600); err != nil {
		t.Fatal(err)
	}

	// A dropped SSH connection changes neither remote tmux nor state. A fresh
	// controller instance reopens both from remote ground truth.
	restartedController := newController()
	if !restartedController.IsRunning("session-resume") {
		t.Fatal("SSH reconnect did not rediscover remote tmux session")
	}
	if reopened, ok, err := restartedController.OpenCapsuleState(context.Background(), key); err != nil || !ok || reopened != ref {
		t.Fatalf("controller restart state reopen = %#v, %t, %v", reopened, ok, err)
	}

	// Abrupt tmux loss removes only the live attachment marker. Recreating the
	// outer session must use the same physical state path and exact bytes.
	host.loseTmuxServer()
	if err := restartedController.Start(context.Background(), "session-resume", cfg); err != nil {
		t.Fatalf("Start after tmux loss: %v", err)
	}
	if command := host.command("session-resume"); !strings.Contains(command, physicalState) {
		t.Fatalf("resumed command %q does not use retained state %q", command, physicalState)
	}
	if data, err := os.ReadFile(conversationDB); err != nil || string(data) != "conversation_id=conv_exact\nturn=retained" {
		t.Fatalf("exact conversation continuity = %q, %v", data, err)
	}
	if err := restartedController.Stop("session-resume"); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if err := restartedController.PurgeCapsuleState(context.Background(), key); err != nil {
		t.Fatalf("explicit purge: %v", err)
	}
}

func TestSSHCapsuleStateConcurrentSessionsAreIsolatedAndListed(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	provider := providerWithLocalCapsuleStateRoot(t, root)
	keys := make([]runtime.CapsuleKey, 2)
	for i, id := range []string{"session-a", "session-b"} {
		key, err := runtime.NewCapsuleKey("city", id)
		if err != nil {
			t.Fatal(err)
		}
		keys[i] = key
	}
	type ensureResult struct {
		keyIndex int
		ref      runtime.CapsuleStateReference
		err      error
	}
	var wg sync.WaitGroup
	results := make(chan ensureResult, len(keys)*2)
	for i := range keys {
		for attempt := 0; attempt < 2; attempt++ {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				ref, _, err := provider.EnsureCapsuleState(context.Background(), keys[i])
				results <- ensureResult{keyIndex: i, ref: ref, err: err}
			}(i)
		}
	}
	wg.Wait()
	close(results)
	refsByKey := make([]runtime.CapsuleStateReference, len(keys))
	for result := range results {
		if result.err != nil {
			t.Fatal(result.err)
		}
		if refsByKey[result.keyIndex].ResourceUID != "" && refsByKey[result.keyIndex] != result.ref {
			t.Fatalf("concurrent ensure disagreed for key %d: %#v vs %#v", result.keyIndex, refsByKey[result.keyIndex], result.ref)
		}
		refsByKey[result.keyIndex] = result.ref
	}
	refs := append([]runtime.CapsuleStateReference(nil), refsByKey...)
	if refs[0].ResourceID == refs[1].ResourceID || refs[0].ResourceUID == refs[1].ResourceUID {
		t.Fatalf("concurrent state references are not isolated: %#v", refs)
	}
	listed, err := provider.ListCapsuleStates(context.Background())
	if err != nil {
		t.Fatalf("ListCapsuleStates: %v", err)
	}
	sort.Slice(refs, func(i, j int) bool { return refs[i].ResourceID < refs[j].ResourceID })
	if len(listed) != len(refs) || listed[0] != refs[0] || listed[1] != refs[1] {
		t.Fatalf("ListCapsuleStates = %#v, want %#v", listed, refs)
	}
}

func TestSSHCapsuleStateRecoversCommittedOperationsAfterLostSSHResponse(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	secureLocalCapsuleStateRoot(t, root)
	key, _ := runtime.NewCapsuleKey("city", "lost-response")
	ensureRunner := &lostStateResponseRunner{delegate: localCommandRunner{}, script: remoteEnsureCapsuleStateScript}
	provider := &Provider{
		conn: &Conn{ep: Endpoint{Host: "local-test"}, run: ensureRunner}, capsuleStateRoot: root,
		workDirs: make(map[string]string),
	}
	ref, created, err := provider.EnsureCapsuleState(context.Background(), key)
	if err != nil || created || ref.ResourceUID == "" {
		t.Fatalf("EnsureCapsuleState after lost committed response = %#v, %t, %v", ref, created, err)
	}
	purgeRunner := &lostStateResponseRunner{delegate: localCommandRunner{}, script: remotePurgeCapsuleStateScript}
	provider.conn.run = purgeRunner
	if err := provider.PurgeCapsuleState(context.Background(), key); err != nil {
		t.Fatalf("PurgeCapsuleState after lost committed response: %v", err)
	}
	if _, ok, err := provider.OpenCapsuleState(context.Background(), key); err != nil || ok {
		t.Fatalf("OpenCapsuleState after recovered purge = ok=%t err=%v", ok, err)
	}
}

func TestSSHCapsulePurgeRecoversVerifiedRenameTombstone(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	provider := providerWithLocalCapsuleStateRoot(t, root)
	key, _ := runtime.NewCapsuleKey("city", "purge-crash")
	ref, _, err := provider.EnsureCapsuleState(context.Background(), key)
	if err != nil {
		t.Fatal(err)
	}
	tombstone := filepath.Join(root, ".gc-purge-"+ref.ResourceID+"-999")
	if err := os.Rename(filepath.Join(root, ref.ResourceID), tombstone); err != nil {
		t.Fatal(err)
	}
	if err := provider.PurgeCapsuleState(context.Background(), key); err != nil {
		t.Fatalf("PurgeCapsuleState tombstone recovery: %v", err)
	}
	if _, err := os.Lstat(tombstone); !os.IsNotExist(err) {
		t.Fatalf("verified purge tombstone remains: %v", err)
	}
}

func TestSSHCapsuleStateRejectsTamperAndForgedIdentity(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	provider := providerWithLocalCapsuleStateRoot(t, root)
	key, _ := runtime.NewCapsuleKey("city", "session")
	ref, _, err := provider.EnsureCapsuleState(context.Background(), key)
	if err != nil {
		t.Fatal(err)
	}
	identityPath := filepath.Join(root, ref.ResourceID, capsuleStateIdentityFile)
	if err := os.WriteFile(identityPath, []byte("tampered\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := provider.OpenCapsuleState(context.Background(), key); !errors.Is(err, runtime.ErrCapsuleStateConflict) {
		t.Fatalf("OpenCapsuleState tamper error = %v, want ErrCapsuleStateConflict", err)
	}
	if err := provider.PurgeCapsuleState(context.Background(), key); !errors.Is(err, runtime.ErrCapsuleStateConflict) {
		t.Fatalf("PurgeCapsuleState tamper error = %v, want ErrCapsuleStateConflict", err)
	}
	forged := key
	forged.SessionID = "other"
	if _, _, err := provider.EnsureCapsuleState(context.Background(), forged); !errors.Is(err, runtime.ErrCapsuleStateConflict) {
		t.Fatalf("EnsureCapsuleState forged key error = %v, want ErrCapsuleStateConflict", err)
	}
}

func providerWithLocalCapsuleStateRoot(t *testing.T, root string) *Provider {
	t.Helper()
	secureLocalCapsuleStateRoot(t, root)
	return &Provider{
		conn:             &Conn{ep: Endpoint{Host: "local-test"}, run: localCommandRunner{}},
		capsuleStateRoot: root,
		workDirs:         make(map[string]string),
	}
}

func secureLocalCapsuleStateRoot(t *testing.T, root string) {
	t.Helper()
	if err := os.Chmod(root, 0o700); err != nil {
		t.Fatalf("secure local capsule state root: %v", err)
	}
}
