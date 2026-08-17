//go:build integration

package ssh

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	stdruntime "runtime"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/shellquote"
)

// TestLiveSSHCapsuleAttachDetachReconnect exercises a pre-provisioned,
// disposable SSH host. The test owns only viewer processes and leaves the
// remote worker alive; the fixture owner is responsible for final teardown.
func TestLiveSSHCapsuleAttachDetachReconnect(t *testing.T) {
	target := strings.TrimSpace(os.Getenv("GC_SSH_ATTACH_TEST_ENDPOINT"))
	session := strings.TrimSpace(os.Getenv("GC_SSH_ATTACH_TEST_SESSION"))
	if target == "" || session == "" {
		t.Skip("requires GC_SSH_ATTACH_TEST_ENDPOINT and GC_SSH_ATTACH_TEST_SESSION")
	}
	if os.Getenv("GC_SSH_ATTACH_TEST_SHELL") != "1" {
		t.Skip("requires GC_SSH_ATTACH_TEST_SHELL=1 confirming a disposable shell capsule")
	}
	if _, err := exec.LookPath("script"); err != nil {
		t.Skip("script(1) is required for a real pseudo-terminal")
	}
	ep, err := ParseEndpoint(target)
	if err != nil {
		t.Fatal(err)
	}
	p := NewProvider(ep)
	if !p.IsRunning(session) {
		t.Fatal("disposable SSH capsule must be running before attach")
	}

	marker := "GC_SSH_ATTACH_多行🙂"
	if err := p.Nudge(session, runtime.TextContent("printf '"+marker+"-one\\n'\nprintf '"+marker+"-two\\n'")); err != nil {
		t.Fatalf("multiline Unicode input: %v", err)
	}
	waitForSSHPaneText(t, p, session, marker+"-two")
	interruptReady := marker + "-interrupt-ready"
	if err := p.Nudge(session, runtime.TextContent("printf '"+interruptReady+"\\n'; sleep 30")); err != nil {
		t.Fatalf("start interrupt target: %v", err)
	}
	waitForSSHPaneText(t, p, session, interruptReady)
	if err := p.Interrupt(session); err != nil {
		t.Fatalf("Ctrl-C: %v", err)
	}
	if err := p.Nudge(session, runtime.TextContent("printf '"+marker+"-after-interrupt\\n'")); err != nil {
		t.Fatalf("input after Ctrl-C: %v", err)
	}
	waitForSSHPaneText(t, p, session, marker+"-after-interrupt")
	before, err := p.Peek(session, 200)
	if err != nil {
		t.Fatal(err)
	}
	markerCount := strings.Count(before, marker+"-after-interrupt")

	var output bytes.Buffer
	p.attachStdout = &output
	p.attachStderr = &output
	p.attachStdin = bytes.NewReader([]byte{'\x02', 'd'})
	p.runAttachCommand = liveSSHScriptAttachRunner(20 * time.Second)
	if err := p.Attach(session); err != nil {
		t.Fatalf("first attach/detach: %v\n%s", err, output.String())
	}
	if !p.IsRunning(session) {
		t.Fatal("terminal detach stopped autonomous SSH capsule")
	}
	if dimensions, code, err := p.Exec(context.Background(), session, []string{"tmux", "display-message", "-p", "-t", session, "#{pane_width}x#{pane_height}"}); err != nil || code != 0 || strings.TrimSpace(string(dimensions)) != "119x41" {
		t.Fatalf("PTY resize did not reach remote tmux: dimensions=%q code=%d err=%v", dimensions, code, err)
	}

	p.attachStdin = bytes.NewReader(nil)
	p.runAttachCommand = liveSSHScriptAttachRunner(750 * time.Millisecond)
	if err := p.Attach(session); err == nil {
		t.Fatal("abrupt viewer loss unexpectedly returned nil")
	}
	if !p.IsRunning(session) {
		t.Fatal("viewer transport loss stopped autonomous SSH capsule")
	}

	p.attachStdin = bytes.NewReader([]byte{'\x02', 'd'})
	p.runAttachCommand = liveSSHScriptAttachRunner(20 * time.Second)
	if err := p.Attach(session); err != nil {
		t.Fatalf("reattach/detach: %v\n%s", err, output.String())
	}
	after, err := p.Peek(session, 200)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Count(after, marker+"-after-interrupt"); got != markerCount {
		t.Fatalf("reconnect duplicated terminal input; marker count=%d, want %d\n%s", got, markerCount, after)
	}
}

func waitForSSHPaneText(t *testing.T, p *Provider, session, want string) {
	t.Helper()
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	timeout := time.NewTimer(10 * time.Second)
	defer timeout.Stop()
	for {
		pane, err := p.Peek(session, 200)
		if err == nil && strings.Contains(pane, want) {
			return
		}
		select {
		case <-ticker.C:
		case <-timeout.C:
			pane, _ := p.Peek(session, 200)
			t.Fatalf("timed out waiting for %q in SSH pane\n%s", want, pane)
		}
	}
}

func liveSSHScriptAttachRunner(timeout time.Duration) sshAttachCommandRunner {
	return func(_ context.Context, args []string, stdin io.Reader, stdout, stderr io.Writer) error {
		ctx, cancel := context.WithTimeout(context.Background(), timeout)
		defer cancel()
		command := "stty rows 41 cols 119; exec ssh " + shellquote.Join(args)
		var cmd *exec.Cmd
		if stdruntime.GOOS == "darwin" {
			cmd = exec.CommandContext(ctx, "script", "-q", "/dev/null", "sh", "-c", command)
		} else {
			cmd = exec.CommandContext(ctx, "script", "-q", "-c", command, "/dev/null")
		}
		cmd.Stdin = stdin
		cmd.Stdout = stdout
		cmd.Stderr = stderr
		if err := cmd.Run(); err != nil {
			return fmt.Errorf("interactive SSH viewer: %w", err)
		}
		return nil
	}
}
