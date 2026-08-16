//go:build !windows

package tmuxtest

import (
	"errors"
	"fmt"
	"syscall"
	"time"
)

const testServerProcessStopWait = 500 * time.Millisecond

type testServerProcess struct {
	pid  int
	pgid int
}

func captureTestServerProcess(socketName, socketPath string) (testServerProcess, error) {
	pid, err := tmuxServerPID(socketName, socketPath)
	if err != nil {
		return testServerProcess{}, err
	}
	pgid, err := syscall.Getpgid(pid)
	if err != nil {
		return testServerProcess{}, fmt.Errorf("querying isolated tmux server %d process group: %w", pid, err)
	}
	if pgid != pid {
		return testServerProcess{}, fmt.Errorf("isolated tmux server %d has unexpected process group %d", pid, pgid)
	}
	if pgid == syscall.Getpgrp() {
		return testServerProcess{}, fmt.Errorf("refusing to capture current process group %d", pgid)
	}
	return testServerProcess{pid: pid, pgid: pgid}, nil
}

func terminateTestServerProcess(handle testServerProcess) error {
	if handle.pid <= 1 || handle.pgid <= 1 || handle.pid != handle.pgid {
		return fmt.Errorf("refusing invalid isolated tmux process handle pid=%d pgid=%d", handle.pid, handle.pgid)
	}
	if handle.pgid == syscall.Getpgrp() {
		return fmt.Errorf("refusing to terminate current process group %d", handle.pgid)
	}

	// The exact socket-targeted kill runs first. Signal the captured process
	// group afterward as the guaranteed path for a wedged server and for tmux
	// configuration helpers (such as tmux-claude-monitor) that tmux reparents
	// when the server exits.
	if err := syscall.Kill(-handle.pgid, syscall.SIGTERM); err != nil && !errors.Is(err, syscall.ESRCH) {
		return fmt.Errorf("terminating isolated tmux process group %d: %w", handle.pgid, err)
	}
	if waitForTestProcessGroupExit(handle.pgid, testServerProcessStopWait) {
		return nil
	}
	if err := syscall.Kill(-handle.pgid, syscall.SIGKILL); err != nil && !errors.Is(err, syscall.ESRCH) {
		return fmt.Errorf("killing isolated tmux process group %d: %w", handle.pgid, err)
	}
	return nil
}

func waitForTestProcessGroupExit(pgid int, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	ticker := time.NewTicker(20 * time.Millisecond)
	defer ticker.Stop()
	for {
		if err := syscall.Kill(-pgid, 0); errors.Is(err, syscall.ESRCH) {
			return true
		}
		if time.Now().After(deadline) {
			return false
		}
		<-ticker.C
	}
}
