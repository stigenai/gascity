//go:build integration

package herdr

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/runtime"
)

var remoteViewerLiveSession atomic.Uint64

// TestLiveHerdrSSHViewerFullTerminal exercises the additional Herdr hop against
// an operator-provisioned disposable SSH shell capsule.
func TestLiveHerdrSSHViewerFullTerminal(t *testing.T) {
	runLiveHerdrRemoteViewerFullTerminal(t, "ssh")
}

// TestLiveHerdrKubernetesViewerFullTerminal exercises the additional Herdr hop
// against an operator-provisioned disposable Kubernetes shell capsule.
func TestLiveHerdrKubernetesViewerFullTerminal(t *testing.T) {
	runLiveHerdrRemoteViewerFullTerminal(t, "k8s")
}

// runLiveHerdrRemoteViewerFullTerminal drives one disposable remote runtime.
// The test owns its local Herdr server and viewer panes; the explicitly
// disposable remote worker remains live unless replacement is separately
// enabled.
func runLiveHerdrRemoteViewerFullTerminal(t *testing.T, expectedRuntime string) {
	t.Helper()
	runtimeName := strings.TrimSpace(os.Getenv("GC_HERDR_VIEWER_TEST_RUNTIME"))
	cityRoot := strings.TrimSpace(os.Getenv("GC_HERDR_VIEWER_TEST_CITY"))
	target := strings.TrimSpace(os.Getenv("GC_HERDR_VIEWER_TEST_SESSION"))
	gcBin := strings.TrimSpace(os.Getenv("GC_HERDR_VIEWER_TEST_GC"))
	if runtimeName == "" || cityRoot == "" || target == "" || gcBin == "" {
		t.Skip("requires GC_HERDR_VIEWER_TEST_RUNTIME, GC_HERDR_VIEWER_TEST_CITY, GC_HERDR_VIEWER_TEST_SESSION, and GC_HERDR_VIEWER_TEST_GC")
	}
	if runtimeName != "ssh" && runtimeName != "k8s" {
		t.Fatalf("GC_HERDR_VIEWER_TEST_RUNTIME = %q, want ssh or k8s", runtimeName)
	}
	if runtimeName != expectedRuntime {
		t.Skipf("GC_HERDR_VIEWER_TEST_RUNTIME=%s configures the other remote viewer fixture", runtimeName)
	}
	if os.Getenv("GC_HERDR_VIEWER_TEST_SHELL") != "1" {
		t.Skip("requires GC_HERDR_VIEWER_TEST_SHELL=1 confirming an isolated disposable shell capsule")
	}
	if _, err := exec.LookPath("herdr"); err != nil {
		t.Skip("herdr is required for the live viewer composition test")
	}
	if info, err := os.Stat(gcBin); err != nil || info.IsDir() || info.Mode()&0o111 == 0 {
		t.Fatalf("GC_HERDR_VIEWER_TEST_GC must name an executable file: %v", err)
	}
	if info, err := os.Stat(cityRoot); err != nil || !info.IsDir() {
		t.Fatalf("GC_HERDR_VIEWER_TEST_CITY must name a city directory: %v", err)
	}

	binDir := t.TempDir()
	stateDir := t.TempDir()
	if err := os.Symlink(gcBin, filepath.Join(binDir, "gc")); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	herdrSession := fmt.Sprintf("gctest-remote-view-%d-%d", os.Getpid(), remoteViewerLiveSession.Add(1))
	viewers := NewViewerProjection(herdrSession, stateDir, cityRoot)
	t.Cleanup(func() {
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cleanupCancel()
		_ = viewers.Close(cleanupCtx, target)
		_ = viewers.c.stopServer()
	})

	binding, err := viewers.Open(context.Background(), ViewerSpec{
		Session: target,
		Label:   runtimeName + " disposable viewer",
	})
	if err != nil {
		t.Fatalf("Open initial viewer: %v", err)
	}
	waitForRemoteViewerReady(t, viewers, target)

	prefix := fmt.Sprintf("GC_HERDR_%s_%d", strings.ToUpper(runtimeName), time.Now().UnixNano())
	sendViewerShellCommand(t, viewers, target,
		fmt.Sprintf("printf '%s-one\\n'; printf '%s-two-世界🙂\\n'", prefix, prefix))
	waitForRemoteViewerOutput(t, viewers, target, prefix+"-two-世界🙂")

	for i := range 8 {
		marker := fmt.Sprintf("%s-rapid-%02d", prefix, i)
		sendViewerShellCommand(t, viewers, target, "printf '"+marker+"\\n'")
	}
	waitForRemoteViewerOutput(t, viewers, target, fmt.Sprintf("%s-rapid-%02d", prefix, 7))

	interruptReady := prefix + "-interrupt-ready"
	sendViewerShellCommand(t, viewers, target, "printf '"+interruptReady+"\\n'; sleep 30")
	waitForRemoteViewerOutput(t, viewers, target, interruptReady)
	if err := viewers.SendKeys(context.Background(), target, "ctrl+c"); err != nil {
		t.Fatalf("Ctrl-C: %v", err)
	}
	sendViewerShellCommand(t, viewers, target, "printf '"+prefix+"-after-interrupt\\n'")
	waitForRemoteViewerOutput(t, viewers, target, prefix+"-after-interrupt")

	if _, err := viewers.c.run(context.Background(), "pane", "split", binding.PaneID, "--direction", "right", "--ratio", "0.5", "--no-focus"); err != nil {
		t.Fatalf("create resize edge: %v", err)
	}
	for i := range 12 {
		direction := "left"
		if i%2 == 0 {
			direction = "right"
		}
		if err := viewers.Resize(context.Background(), target, ViewerResize{Direction: direction, Amount: 0.01}); err != nil {
			t.Fatalf("resize %d: %v", i, err)
		}
	}
	sendViewerShellCommand(t, viewers, target, "printf '"+prefix+"-after-resize\\n'")
	waitForRemoteViewerOutput(t, viewers, target, prefix+"-after-resize")

	// Detach the authoritative remote tmux client. This ends only gc session
	// attach inside the viewer pane; Open reconnects the same remote session.
	if err := viewers.SendKeys(context.Background(), target, "ctrl+b", "d"); err != nil {
		t.Fatalf("remote tmux detach: %v", err)
	}
	waitForViewerAttachmentExit(t, viewers, target)
	if _, err := viewers.Open(context.Background(), ViewerSpec{Session: target, Label: runtimeName + " disposable viewer"}); err != nil {
		t.Fatalf("Open after remote detach: %v", err)
	}
	waitForRemoteViewerReady(t, viewers, target)
	sendViewerShellCommand(t, viewers, target, "printf '"+prefix+"-after-detach\\n'")
	waitForRemoteViewerOutput(t, viewers, target, prefix+"-after-detach")

	// Losing the whole local Herdr server must still leave the remote worker
	// alive. The durable binding is probed and replaced on the next Open.
	if err := viewers.c.stopServer(); err != nil {
		t.Fatalf("stop local Herdr server: %v", err)
	}
	if _, err := viewers.Open(context.Background(), ViewerSpec{Session: target, Label: runtimeName + " disposable viewer"}); err != nil {
		t.Fatalf("Open after Herdr restart: %v", err)
	}
	waitForRemoteViewerReady(t, viewers, target)
	sendViewerShellCommand(t, viewers, target, "printf '"+prefix+"-after-herdr-restart\\n'")
	waitForRemoteViewerOutput(t, viewers, target, prefix+"-after-herdr-restart")

	if os.Getenv("GC_HERDR_VIEWER_TEST_ALLOW_REPLACE") == "1" {
		cmd := exec.Command(gcBin, "session", "reset", target)
		cmd.Dir = cityRoot
		cmd.Env = append(os.Environ(), "GC_CITY="+cityRoot)
		cmd.Stdout = io.Discard
		cmd.Stderr = io.Discard
		if err := cmd.Run(); err != nil {
			t.Fatalf("replace disposable %s runtime: %v", runtimeName, err)
		}
		waitForViewerAttachmentExit(t, viewers, target)
		if _, err := viewers.Open(context.Background(), ViewerSpec{Session: target, Label: runtimeName + " disposable viewer"}); err != nil {
			t.Fatalf("Open after %s replacement: %v", runtimeName, err)
		}
		waitForRemoteViewerReady(t, viewers, target)
		sendViewerShellCommand(t, viewers, target, "printf '"+prefix+"-after-replacement\\n'")
		waitForRemoteViewerOutput(t, viewers, target, prefix+"-after-replacement")
	}
}

func sendViewerShellCommand(t *testing.T, viewers *ViewerProjection, target, command string) {
	t.Helper()
	if err := viewers.SendText(context.Background(), target, command); err != nil {
		t.Fatalf("send viewer shell command: %v", err)
	}
	if err := viewers.SendKeys(context.Background(), target, "enter"); err != nil {
		t.Fatalf("submit viewer shell command: %v", err)
	}
}

func waitForRemoteViewerReady(t *testing.T, viewers *ViewerProjection, target string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	for {
		_, err := viewers.Read(ctx, target, 20)
		if err == nil {
			return
		}
		select {
		case <-ticker.C:
		case <-ctx.Done():
			t.Fatalf("viewer did not become ready: %v", err)
		}
	}
}

func waitForRemoteViewerOutput(t *testing.T, viewers *ViewerProjection, target, marker string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	var last string
	var lastErr error
	for {
		last, lastErr = viewers.Read(ctx, target, 300)
		if lastErr == nil && strings.Contains(last, marker) {
			return
		}
		select {
		case <-ticker.C:
		case <-ctx.Done():
			t.Fatalf("viewer output did not contain generated marker: err=%v", lastErr)
		}
	}
}

func waitForViewerAttachmentExit(t *testing.T, viewers *ViewerProjection, target string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	for {
		_, err := viewers.Read(ctx, target, 20)
		if errors.Is(err, runtime.ErrSessionNotFound) {
			return
		}
		if err != nil && !errors.Is(err, runtime.ErrRuntimeUnavailable) {
			t.Fatalf("wait for viewer attachment exit: %v", err)
		}
		select {
		case <-ticker.C:
		case <-ctx.Done():
			t.Fatalf("viewer attachment did not exit: %v", err)
		}
	}
}
