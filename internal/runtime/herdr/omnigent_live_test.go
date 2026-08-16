package herdr

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/runtime"
)

var omnigentLiveSession uint64

func TestOmnigentRawPaneLiveViewWithIsolatedHerdrServer(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping live herdr test in short mode")
	}
	if _, err := exec.LookPath("herdr"); err != nil {
		t.Skip("herdr not installed")
	}
	session := fmt.Sprintf("gctest-omnigent-%d-%d", os.Getpid(), atomic.AddUint64(&omnigentLiveSession, 1))
	workdir := t.TempDir()
	script := filepath.Join(workdir, "omnigent-attachment-fixture")
	if err := os.WriteFile(script, []byte("#!/bin/sh\nprintf 'OMNI_READY\\n'\nwhile IFS= read -r line; do if [ ! -e received-input ]; then printf '%s' \"$line\" > received-input; fi; printf 'OMNI_ECHO:%s\\n' \"$line\"; done\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	p := New(session, t.TempDir(), t.TempDir(), 5*time.Second, 5*time.Second)
	t.Cleanup(func() {
		_ = p.Stop("city--worker")
		_ = p.TeardownServer()
	})
	if err := p.Start(context.Background(), "city--worker", runtime.Config{Command: script, WorkDir: workdir}); err != nil {
		t.Fatalf("Start: %v", err)
	}
	waitHerdrOmnigentOutput(t, p, "city--worker", "OMNI_READY")
	prompt := "multiline paste stays literal: $(no-shell) ; 'quotes' 世界"
	if err := p.Nudge("city--worker", runtime.TextContent(prompt)); err != nil {
		t.Fatalf("Nudge: %v", err)
	}
	waitHerdrOmnigentOutput(t, p, "city--worker", "OMNI_ECHO:")
	deadline := time.NewTimer(5 * time.Second)
	defer deadline.Stop()
	ticker := time.NewTicker(50 * time.Millisecond)
	defer ticker.Stop()
	for {
		got, err := os.ReadFile(filepath.Join(workdir, "received-input"))
		if err == nil && string(got) == prompt {
			break
		}
		select {
		case <-deadline.C:
			t.Fatalf("fixture input = %q, err=%v", got, err)
		case <-ticker.C:
		}
	}
	if !p.IsRunning("city--worker") {
		t.Fatal("raw Omnigent pane is not live after interactive input")
	}
}

func waitHerdrOmnigentOutput(t *testing.T, p *Provider, name, marker string) {
	t.Helper()
	deadline := time.NewTimer(10 * time.Second)
	defer deadline.Stop()
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	for {
		output, err := p.Peek(name, 100)
		if err == nil && strings.Contains(output, marker) {
			return
		}
		select {
		case <-deadline.C:
			output, err := p.Peek(name, 100)
			t.Fatalf("pane output did not contain %q: output=%q err=%v", marker, output, err)
		case <-ticker.C:
		}
	}
}
