//go:build integration

package k8s

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	goruntime "runtime"
	"strings"
	"testing"
	"time"

	gcruntime "github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/shellquote"
)

// TestLiveCapsuleAttachDetachReconnect exercises the real kubectl exec -it
// path against a pre-provisioned disposable capsule fixture. The fixture must
// live in an explicitly isolated namespace and keep one outer tmux session
// named "main" running. The test owns only viewer processes: every command is
// context-bounded and the worker session intentionally survives detach/loss.
func TestLiveCapsuleAttachDetachReconnect(t *testing.T) {
	session := strings.TrimSpace(os.Getenv("GC_K8S_ATTACH_TEST_SESSION"))
	namespace := strings.TrimSpace(os.Getenv("GC_K8S_NAMESPACE"))
	if session == "" || namespace == "" {
		t.Skip("requires GC_K8S_ATTACH_TEST_SESSION and GC_K8S_NAMESPACE for a disposable live capsule")
	}
	if !strings.HasPrefix(namespace, "gc-attach-test-") {
		t.Fatalf("GC_K8S_NAMESPACE %q must use isolated gc-attach-test-* namespace", namespace)
	}
	if _, err := exec.LookPath("script"); err != nil {
		t.Skip("requires script(1) to allocate the kubectl interactive pseudo-terminal")
	}

	provider, err := NewProvider()
	if err != nil {
		t.Fatalf("NewProvider: %v", err)
	}
	if running, err := provider.IsRunningChecked(session); err != nil || !running {
		t.Fatalf("fixture session must be running before attach: running=%t err=%v", running, err)
	}
	if os.Getenv("GC_K8S_ATTACH_TEST_SHELL") != "1" {
		t.Skip("requires GC_K8S_ATTACH_TEST_SHELL=1 confirming the disposable capsule runs a shell test harness")
	}
	marker := "GC_ATTACH_多行🙂"
	if err := provider.Nudge(session, gcruntime.TextContent("printf '"+marker+"-one\\n'\nprintf '"+marker+"-two\\n'")); err != nil {
		t.Fatalf("multiline Unicode input: %v", err)
	}
	waitForLivePaneText(t, provider, session, marker+"-two")
	interruptReady := marker + "-interrupt-ready"
	if err := provider.Nudge(session, gcruntime.TextContent("printf '"+interruptReady+"\\n'; sleep 30")); err != nil {
		t.Fatalf("start interrupt target: %v", err)
	}
	waitForLivePaneText(t, provider, session, interruptReady)
	if err := provider.Interrupt(session); err != nil {
		t.Fatalf("Ctrl-C: %v", err)
	}
	if err := provider.Nudge(session, gcruntime.TextContent("printf '"+marker+"-after-interrupt\\n'")); err != nil {
		t.Fatalf("input after Ctrl-C: %v", err)
	}
	waitForLivePaneText(t, provider, session, marker+"-after-interrupt")
	beforeAttach, err := provider.Peek(session, 200)
	if err != nil {
		t.Fatalf("Peek before attach: %v", err)
	}
	markerCount := strings.Count(beforeAttach, marker+"-after-interrupt")

	var output bytes.Buffer
	provider.attachStdout = &output
	provider.attachStderr = &output
	provider.runAttachCommand = liveScriptAttachRunner(20 * time.Second)
	provider.attachStdin = bytes.NewReader([]byte{'\x02', 'd'}) // tmux prefix + detach
	if err := provider.Attach(session); err != nil {
		t.Fatalf("first attach/detach: %v\n%s", err, output.String())
	}
	if running, err := provider.IsRunningChecked(session); err != nil || !running {
		t.Fatalf("detach changed worker lifecycle: running=%t err=%v", running, err)
	}

	// Abrupt local viewer loss must not stop the outer tmux session.
	provider.attachStdin = bytes.NewReader(nil)
	provider.runAttachCommand = liveScriptAttachRunner(750 * time.Millisecond)
	if err := provider.Attach(session); err == nil {
		t.Fatal("connection-loss attach unexpectedly returned nil")
	}
	if running, err := provider.IsRunningChecked(session); err != nil || !running {
		t.Fatalf("connection loss changed worker lifecycle: running=%t err=%v", running, err)
	}

	provider.attachStdin = bytes.NewReader([]byte{'\x02', 'd'})
	provider.runAttachCommand = liveScriptAttachRunner(20 * time.Second)
	if err := provider.Attach(session); err != nil {
		t.Fatalf("reattach/detach: %v\n%s", err, output.String())
	}
	pane, err := provider.Peek(session, 200)
	if err != nil {
		t.Fatalf("Peek after reconnect: %v", err)
	}
	if got := strings.Count(pane, marker+"-after-interrupt"); got != markerCount {
		t.Fatalf("reconnect duplicated terminal input; marker count=%d, want %d\n%s", got, markerCount, pane)
	}
}

func waitForLivePaneText(t *testing.T, provider *Provider, session, want string) {
	t.Helper()
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	timeout := time.NewTimer(10 * time.Second)
	defer timeout.Stop()
	for {
		pane, err := provider.Peek(session, 200)
		if err == nil && strings.Contains(pane, want) {
			return
		}
		select {
		case <-ticker.C:
		case <-timeout.C:
			pane, _ := provider.Peek(session, 200)
			t.Fatalf("timed out waiting for %q in pane\n%s", want, pane)
		}
	}
}

func liveScriptAttachRunner(timeout time.Duration) attachCommandRunner {
	return func(_ context.Context, args []string, stdin io.Reader, stdout, stderr io.Writer) error {
		ctx, cancel := context.WithTimeout(context.Background(), timeout)
		defer cancel()
		command := "stty rows 41 cols 119; exec " + shellquote.Join(append([]string{"kubectl"}, args...))
		var cmd *exec.Cmd
		if goruntime.GOOS == "darwin" {
			cmd = exec.CommandContext(ctx, "script", "-q", "/dev/null", "sh", "-c", command)
		} else {
			cmd = exec.CommandContext(ctx, "script", "-q", "-c", command, "/dev/null")
		}
		cmd.Stdin = stdin
		cmd.Stdout = stdout
		cmd.Stderr = stderr
		if err := cmd.Run(); err != nil {
			return fmt.Errorf("interactive viewer: %w", err)
		}
		return nil
	}
}
