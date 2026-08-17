//go:build integration

package ssh

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/shellquote"
	"github.com/gastownhall/gascity/test/tmuxtest"
)

func TestSSHCapsuleStopRealTmuxLeavesOtherSessionAndRetainsConversationState(t *testing.T) {
	root := t.TempDir() // Created before Guard so its LIFO cleanup runs afterward.
	guard := tmuxtest.NewGuard(t)
	runner := newIsolatedSSHTestRunner(t, root, guard.SocketName())
	p := newLocalCleanupProvider(t, root, runner)
	key, ref, statePath := prepareLocalCleanupCapsule(t, p, "session-a")
	otherKey, otherRef, otherStatePath := prepareLocalCleanupCapsule(t, p, "session-b")
	process := writeIgnoringProcess(t, root)

	startIsolatedTmuxSession(t, runner, "session-a", process, statePath)
	if err := guard.CaptureServer(); err != nil {
		t.Fatalf("capture isolated tmux server: %v", err)
	}
	// Registered after TempDir and Guard: LIFO runs this exact process-group
	// stop before either the socket guard or temporary paths disappear.
	t.Cleanup(func() {
		if err := p.stopCapsule(context.Background(), "session-a", ref); err != nil {
			t.Errorf("cleanup first capsule session: %v", err)
		}
	})
	setIsolatedTmuxOption(t, runner, "session-a", capsuleStateTMUXOption, ref.ResourceUID)
	startIsolatedTmuxSession(t, runner, "session-b", process, otherStatePath)
	t.Cleanup(func() {
		if err := p.stopCapsule(context.Background(), "session-b", otherRef); err != nil {
			t.Errorf("cleanup other capsule session: %v", err)
		}
	})
	setIsolatedTmuxOption(t, runner, "session-b", capsuleStateTMUXOption, otherRef.ResourceUID)

	if err := p.Stop("session-a"); err != nil {
		t.Fatalf("Stop(session-a): %v", err)
	}
	if guard.HasSession("session-a") {
		t.Fatal("stopped capsule tmux session still exists")
	}
	if !guard.HasSession("session-b") {
		t.Fatal("stopping one capsule killed the other tmux session or server")
	}
	if _, ok, err := p.OpenCapsuleState(context.Background(), key); err != nil || !ok {
		t.Fatalf("Stop removed durable conversation state: ok=%t err=%v", ok, err)
	}
	assertPathAbsent(t, filepath.Join(p.capsuleRunRoot, key.ResourceStem()))
	assertPathAbsent(t, filepath.Join(p.capsuleCatalogRoot, key.ResourceStem()))
	assertPathExists(t, filepath.Join(p.capsuleRunRoot, otherKey.ResourceStem()))
	assertPathExists(t, filepath.Join(p.capsuleCatalogRoot, otherKey.ResourceStem()))
	if err := p.Stop("session-b"); err != nil {
		t.Fatalf("Stop(session-b): %v", err)
	}
	if processTableContains(t, statePath) || processTableContains(t, otherStatePath) {
		t.Fatal("capsule process census did not return to baseline")
	}
}

func TestSSHCapsuleStopRealTmuxLossReapsOrphanedProcessGroup(t *testing.T) {
	root := t.TempDir() // Created before Guard so its LIFO cleanup runs afterward.
	guard := tmuxtest.NewGuard(t)
	runner := newIsolatedSSHTestRunner(t, root, guard.SocketName())
	p := newLocalCleanupProvider(t, root, runner)
	key, ref, statePath := prepareLocalCleanupCapsule(t, p, "session-orphan")
	process := writeIgnoringProcess(t, root)
	startIsolatedTmuxSession(t, runner, "session-orphan", process, statePath)
	if err := guard.CaptureServer(); err != nil {
		t.Fatalf("capture isolated tmux server: %v", err)
	}
	t.Cleanup(func() {
		if err := p.stopCapsule(context.Background(), "session-orphan", ref); err != nil {
			t.Errorf("cleanup orphan capsule session: %v", err)
		}
	})
	setIsolatedTmuxOption(t, runner, "session-orphan", capsuleStateTMUXOption, ref.ResourceUID)

	if _, code, err := runner.run(context.Background(), Endpoint{}, []string{"tmux", "kill-server"}, nil); err != nil || code != 0 {
		t.Fatalf("targeted isolated tmux server loss: code=%d err=%v", code, err)
	}
	if err := p.Stop("session-orphan"); err != nil {
		t.Fatalf("Stop after tmux loss: %v", err)
	}
	if processTableContains(t, statePath) {
		t.Fatalf("orphaned capsule process still contains state path %q", statePath)
	}
	if _, ok, err := p.OpenCapsuleState(context.Background(), key); err != nil || !ok {
		t.Fatalf("orphan cleanup removed durable state: ok=%t err=%v", ok, err)
	}
	assertPathAbsent(t, filepath.Join(p.capsuleRunRoot, key.ResourceStem()))
	assertPathAbsent(t, filepath.Join(p.capsuleCatalogRoot, key.ResourceStem()))
}

type isolatedSSHTestRunner struct {
	tmuxPath string
	socket   string
	binDir   string
}

func newIsolatedSSHTestRunner(t *testing.T, root, socket string) *isolatedSSHTestRunner {
	t.Helper()
	tmuxPath, err := exec.LookPath("tmux")
	if err != nil {
		t.Skip("tmux is required")
	}
	binDir := filepath.Join(root, "bin")
	if err := os.MkdirAll(binDir, 0o700); err != nil {
		t.Fatal(err)
	}
	wrapper := "#!/bin/sh\nexec " + shellquote.Quote(tmuxPath) + " -L " + shellquote.Quote(socket) + " \"$@\"\n"
	if err := os.WriteFile(filepath.Join(binDir, "tmux"), []byte(wrapper), 0o700); err != nil {
		t.Fatal(err)
	}
	return &isolatedSSHTestRunner{tmuxPath: tmuxPath, socket: socket, binDir: binDir}
}

func (r *isolatedSSHTestRunner) run(ctx context.Context, _ Endpoint, argv []string, stdin []byte) ([]byte, int, error) {
	if len(argv) == 0 {
		return nil, -1, errors.New("empty argv")
	}
	command := argv[0]
	args := append([]string(nil), argv[1:]...)
	if command == "tmux" {
		command = r.tmuxPath
		args = append([]string{"-L", r.socket}, args...)
	}
	cmd := exec.CommandContext(ctx, command, args...)
	cmd.Env = append(os.Environ(), "PATH="+r.binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
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

func newLocalCleanupProvider(t *testing.T, root string, remoteRunner runner) *Provider {
	t.Helper()
	stateRoot := filepath.Join(root, "state")
	runRoot := filepath.Join(root, "run")
	catalogRoot := filepath.Join(root, "catalog")
	for _, path := range []string{stateRoot, runRoot, catalogRoot} {
		if err := os.MkdirAll(path, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	return &Provider{
		conn: &Conn{ep: Endpoint{Host: "local-test"}, run: remoteRunner}, capsuleStateRoot: stateRoot,
		capsuleRunRoot: runRoot, capsuleCatalogRoot: catalogRoot, workDirs: make(map[string]string),
	}
}

func prepareLocalCleanupCapsule(t *testing.T, p *Provider, session string) (runtime.CapsuleKey, runtime.CapsuleStateReference, string) {
	t.Helper()
	key, err := runtime.NewCapsuleKey("isolated-ssh-cleanup", session)
	if err != nil {
		t.Fatal(err)
	}
	ref, _, err := p.EnsureCapsuleState(context.Background(), key)
	if err != nil {
		t.Fatal(err)
	}
	for _, root := range []string{p.capsuleRunRoot, p.capsuleCatalogRoot} {
		if err := os.Mkdir(filepath.Join(root, key.ResourceStem()), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	return key, ref, filepath.Join(p.capsuleStateRoot, key.ResourceStem())
}

func writeIgnoringProcess(t *testing.T, root string) string {
	t.Helper()
	path := filepath.Join(root, "capsule-process")
	script := "#!/bin/sh\ntrap '' HUP TERM\nwhile :; do sleep 60 & wait $!; done\n"
	if err := os.WriteFile(path, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	return path
}

func startIsolatedTmuxSession(t *testing.T, runner runner, name, process, statePath string) {
	t.Helper()
	command := shellquote.Join([]string{process, "--state-root", statePath})
	if out, code, err := runner.run(context.Background(), Endpoint{}, []string{"tmux", "new-session", "-d", "-s", name, command}, nil); err != nil || code != 0 {
		t.Fatalf("start isolated tmux %q: output=%q code=%d err=%v", name, out, code, err)
	}
}

func setIsolatedTmuxOption(t *testing.T, runner runner, name, key, value string) {
	t.Helper()
	if out, code, err := runner.run(context.Background(), Endpoint{}, []string{"tmux", "set-option", "-t", name, key, value}, nil); err != nil || code != 0 {
		t.Fatalf("set tmux option: output=%q code=%d err=%v", out, code, err)
	}
}

func processTableContains(t *testing.T, needle string) bool {
	t.Helper()
	out, err := exec.Command("ps", "-eo", "command=").Output()
	if err != nil {
		t.Fatal(err)
	}
	for _, line := range strings.Split(string(out), "\n") {
		if strings.Contains(line, needle) {
			return true
		}
	}
	return false
}

func assertPathAbsent(t *testing.T, path string) {
	t.Helper()
	if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("path %q still exists or cannot be checked: %v", path, err)
	}
}

func assertPathExists(t *testing.T, path string) {
	t.Helper()
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("path %q missing: %v", path, err)
	}
}
