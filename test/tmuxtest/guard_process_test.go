//go:build integration && !windows

package tmuxtest

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/shellquote"
)

const (
	tmuxCleanupMonitorHelperEnv  = "GC_TMUX_CLEANUP_MONITOR_HELPER"
	tmuxCleanupMonitorPIDFileEnv = "GC_TMUX_CLEANUP_MONITOR_PID_FILE"
)

func testServerOption(socketName, option string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), tmuxGuardCommandTimeout)
	defer cancel()
	out, err := exec.CommandContext(ctx, "tmux", tmuxArgs(socketName, "show-options", "-sv", option)...).CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("reading isolated tmux server option %q: %w: %s", option, err, strings.TrimSpace(string(out)))
	}
	return strings.TrimSpace(string(out)), nil
}

func tmuxServerPID(socketName, socketPath string) (int, error) {
	pid, _, err := tmuxServerIdentity(socketName, socketPath)
	return pid, err
}

func TestTmuxCleanupMonitorHelper(t *testing.T) {
	if os.Getenv(tmuxCleanupMonitorHelperEnv) != "1" {
		return
	}
	pidFile := os.Getenv(tmuxCleanupMonitorPIDFileEnv)
	if pidFile == "" {
		os.Exit(2)
	}
	if err := os.WriteFile(pidFile, []byte(strconv.Itoa(os.Getpid())), 0o600); err != nil {
		os.Exit(2)
	}
	reader, writer, err := os.Pipe()
	if err != nil {
		os.Exit(2)
	}
	defer reader.Close()
	defer writer.Close()
	var oneByte [1]byte
	_, _ = reader.Read(oneByte[:])
}

func TestGuardCapturedServerCleanupReapsServerProcessGroup(t *testing.T) {
	RequireTmux(t)
	// Keep the tmux socket below Darwin's 104-byte AF_UNIX limit. The socket
	// name is process-unique and cleanup below always targets it explicitly.
	t.Setenv(tmuxTmpEnv, "/tmp")
	socket := fmt.Sprintf("gctest-cleanup-%d", os.Getpid())

	start := exec.Command("tmux", "-f", "/dev/null", "-L", socket, "new-session", "-d", "-s", "cleanup", "sleep", "300")
	if out, err := start.CombinedOutput(); err != nil {
		t.Fatalf("starting isolated tmux server: %v\n%s", err, out)
	}
	t.Cleanup(func() {
		_ = killTestSocketServerWithHandles(socket, nil)
	})

	g := &Guard{t: t, cityName: socket, socketName: socket}
	if err := g.CaptureServer(); err != nil {
		t.Fatalf("CaptureServer: %v", err)
	}
	if out, err := testServerOption(socket, "exit-empty"); err != nil || out != "on" {
		t.Fatalf("isolated server exit-empty = %q, err %v; want on", out, err)
	}
	serverPID, err := tmuxServerPID(socket, "")
	if err != nil {
		t.Fatalf("tmuxServerPID: %v", err)
	}
	t.Cleanup(func() {
		_ = syscall.Kill(-serverPID, syscall.SIGKILL)
	})

	pidFile := filepath.Join(t.TempDir(), "monitor.pid")
	monitorCommand := strings.Join([]string{
		tmuxCleanupMonitorHelperEnv + "=1",
		tmuxCleanupMonitorPIDFileEnv + "=" + shellquote.Quote(pidFile),
		"exec",
		shellquote.Quote(os.Args[0]),
		"-test.run=^TestTmuxCleanupMonitorHelper$",
	}, " ")
	launch := exec.Command("tmux", "-L", socket, "run-shell", "-b", monitorCommand)
	if out, err := launch.CombinedOutput(); err != nil {
		t.Fatalf("starting simulated tmux monitor in server process group %d: %v\n%s", serverPID, err, out)
	}
	monitorPID, err := waitForTestPIDFile(pidFile, 3*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = syscall.Kill(monitorPID, syscall.SIGKILL)
	})
	if pgid, err := syscall.Getpgid(monitorPID); err != nil {
		t.Fatalf("Getpgid(%d): %v", monitorPID, err)
	} else if pgid != serverPID {
		t.Fatalf("simulated monitor process group = %d, want tmux server %d", pgid, serverPID)
	}
	// Reproduce the escaped failure: TempDir cleanup removed TMUX_TMPDIR and
	// therefore unlinked the socket before tmux kill-server could reach it.
	// The PID/process-group handle captured at spawn must remain sufficient.
	socketPath := filepath.Join("/tmp", "tmux-"+strconv.Itoa(os.Getuid()), socket)
	if err := os.Remove(socketPath); err != nil {
		t.Fatalf("removing isolated tmux socket before cleanup: %v", err)
	}
	g.killGuardSessions()
	if err := waitForTestProcessExit(monitorPID, 3*time.Second); err != nil {
		t.Fatal(err)
	}
}

func TestGuardCapturedServerCleanupRemovesStaleSocket(t *testing.T) {
	RequireTmux(t)
	t.Setenv(tmuxTmpEnv, "/tmp")
	socket := fmt.Sprintf("gctest-stale-socket-%d", os.Getpid())
	socketPath := filepath.Join("/tmp", "tmux-"+strconv.Itoa(os.Getuid()), socket)
	startServer := func() {
		t.Helper()
		start := exec.Command("tmux", "-f", "/dev/null", "-L", socket, "new-session", "-d", "-s", "cleanup", "sleep", "300")
		if out, err := start.CombinedOutput(); err != nil {
			t.Fatalf("starting isolated tmux server: %v\n%s", err, out)
		}
	}
	startServer()
	t.Cleanup(func() {
		_ = killTestSocketServerWithHandles(socket, nil)
		_ = os.Remove(socketPath)
	})

	g := &Guard{t: t, cityName: socket, socketName: socket}
	if err := g.CaptureServer(); err != nil {
		t.Fatalf("CaptureServer: %v", err)
	}
	serverPID, err := tmuxServerPID(socket, "")
	if err != nil {
		t.Fatalf("tmuxServerPID: %v", err)
	}
	if err := syscall.Kill(-serverPID, syscall.SIGKILL); err != nil {
		t.Fatalf("killing isolated server process group: %v", err)
	}
	if !waitForTestProcessGroupExit(serverPID, 3*time.Second) {
		t.Fatalf("isolated server process group %d did not exit", serverPID)
	}
	if _, err := os.Lstat(socketPath); err != nil {
		t.Fatalf("stale socket %s missing before guard cleanup: %v", socketPath, err)
	}

	g.killGuardSessions()
	if _, err := os.Lstat(socketPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("stale socket survived guard cleanup: %v", err)
	}

	// The suite-wide pre/post sweep has no previously captured handle. It must
	// still remove a dead socket after the server process is already gone.
	startServer()
	serverPID, err = tmuxServerPID(socket, "")
	if err != nil {
		t.Fatalf("tmuxServerPID for sweep fallback: %v", err)
	}
	if err := syscall.Kill(-serverPID, syscall.SIGKILL); err != nil {
		t.Fatalf("killing isolated server before sweep fallback: %v", err)
	}
	if !waitForTestProcessGroupExit(serverPID, 3*time.Second) {
		t.Fatalf("isolated sweep-fallback process group %d did not exit", serverPID)
	}
	if err := killTestSocketPath(socketPath); err != nil {
		t.Fatalf("killTestSocketPath: %v", err)
	}
	if _, err := os.Lstat(socketPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("stale sweep socket survived cleanup: %v", err)
	}
}

func waitForTestPIDFile(path string, timeout time.Duration) (int, error) {
	deadline := time.NewTimer(timeout)
	defer deadline.Stop()
	ticker := time.NewTicker(20 * time.Millisecond)
	defer ticker.Stop()
	for {
		data, err := os.ReadFile(path)
		if err == nil {
			pid, parseErr := strconv.Atoi(strings.TrimSpace(string(data)))
			if parseErr != nil || pid <= 1 {
				return 0, fmt.Errorf("invalid monitor PID file %q", data)
			}
			return pid, nil
		}
		select {
		case <-deadline.C:
			return 0, fmt.Errorf("monitor PID file %s did not appear: %w", path, err)
		case <-ticker.C:
		}
	}
}

func waitForTestProcessExit(pid int, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	ticker := time.NewTicker(20 * time.Millisecond)
	defer ticker.Stop()
	for {
		err := syscall.Kill(pid, 0)
		if errors.Is(err, syscall.ESRCH) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("probing process %d: %w", pid, err)
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("process %s remained alive after tmux cleanup", strconv.Itoa(pid))
		}
		<-ticker.C
	}
}
