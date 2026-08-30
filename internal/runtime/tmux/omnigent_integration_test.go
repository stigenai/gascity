//go:build integration

package tmux

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/omnigent/inputframe"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/test/tmuxtest"
)

func TestOmnigentInteractiveViewUsesOnlyIsolatedTmuxSession(t *testing.T) {
	if !hasTmux() {
		t.Skip("tmux not installed")
	}
	socket := fmt.Sprintf("gctest-omnigent-%d-%d", os.Getpid(), time.Now().UnixNano())
	cfg := DefaultConfig()
	cfg.SocketName = socket
	cfg.SetupTimeout = 5 * time.Second
	cfg.NudgeReadyTimeout = 2 * time.Second
	cfg.NudgeIdleTimeout = 0
	p := NewProviderWithConfig(cfg)
	const session = "omnigent-worker"
	workdir := t.TempDir()
	guard := tmuxtest.NewGuardWithSocket(t, socket)
	script := filepath.Join(workdir, "omnigent-attachment-fixture")
	body := "#!/bin/sh\ntrap 'printf \\\"OMNI_INTERRUPTED\\\\n\\\"' INT\nprintf 'OMNI_READY\\n'\nwhile IFS= read -r line; do printf '%s\\n' \"$line\" >> received-input; printf 'OMNI_ECHO:%s\\n' \"$line\"; done\n"
	if err := os.WriteFile(script, []byte(body), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = p.Stop(session)
	})
	if err := p.Start(context.Background(), session, runtime.Config{
		Command: script, WorkDir: workdir, ProviderName: inputframe.ControllerProvider,
		Env: map[string]string{"GC_PROVIDER": inputframe.ControllerProvider},
	}); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if err := guard.CaptureServer(); err != nil {
		t.Fatalf("capturing isolated tmux server: %v", err)
	}
	waitTmuxOmnigentOutput(t, p, session, "OMNI_READY")
	first := "literal $(no-shell); 'quotes' 世界"
	if err := p.Nudge(session, runtime.TextContent(first)); err != nil {
		t.Fatalf("Nudge: %v", err)
	}
	if err := p.Nudge(session, runtime.TextContent("second line\nthird line")); err != nil {
		t.Fatalf("multiline Nudge: %v", err)
	}
	waitTmuxOmnigentOutput(t, p, session, "OMNI_ECHO:"+inputframe.Encode("second line\nthird line"))
	deadline := time.NewTimer(5 * time.Second)
	defer deadline.Stop()
	ticker := time.NewTicker(50 * time.Millisecond)
	defer ticker.Stop()
	for {
		got, err := os.ReadFile(filepath.Join(workdir, "received-input"))
		want := strings.Join([]string{
			inputframe.Encode(first),
			inputframe.Encode("second line\nthird line"),
		}, "\n") + "\n"
		if err == nil && string(got) == want {
			break
		}
		select {
		case <-deadline.C:
			t.Fatalf("fixture input = %q, err=%v", got, err)
		case <-ticker.C:
		}
	}
	if err := p.Interrupt(session); err != nil {
		t.Fatalf("Interrupt: %v", err)
	}
	waitTmuxOmnigentOutput(t, p, session, "OMNI_INTERRUPTED")
	if err := p.Stop(session); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if p.IsRunning(session) {
		t.Fatal("isolated Omnigent tmux session remains after stop")
	}
	// Every provider operation above used cfg.SocketName. This read-only probe
	// names the same test socket explicitly; the default tmux server is never a
	// target or cleanup mechanism.
	probe := exec.Command("tmux", "-L", socket, "has-session", "-t", session)
	if err := probe.Run(); err == nil {
		t.Fatal("stopped session still exists on isolated test socket")
	}
}

func waitTmuxOmnigentOutput(t *testing.T, p *Provider, session, marker string) {
	t.Helper()
	deadline := time.NewTimer(10 * time.Second)
	defer deadline.Stop()
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	for {
		output, err := p.Peek(session, 200)
		if err == nil && strings.Contains(output, marker) {
			return
		}
		select {
		case <-deadline.C:
			output, err := p.Peek(session, 200)
			t.Fatalf("tmux pane output did not contain %q: output=%q err=%v", marker, output, err)
		case <-ticker.C:
		}
	}
}
