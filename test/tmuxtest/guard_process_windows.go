//go:build windows

package tmuxtest

import (
	"fmt"
	"os"
)

type testServerProcess struct {
	pid int
}

func captureTestServerProcess(socketName, socketPath string) (testServerProcess, error) {
	pid, err := tmuxServerPID(socketName, socketPath)
	if err != nil {
		return testServerProcess{}, err
	}
	return testServerProcess{pid: pid}, nil
}

func terminateTestServerProcess(handle testServerProcess) error {
	process, err := os.FindProcess(handle.pid)
	if err != nil {
		return fmt.Errorf("finding isolated tmux server %d: %w", handle.pid, err)
	}
	if err := process.Kill(); err != nil {
		return fmt.Errorf("killing isolated tmux server %d: %w", handle.pid, err)
	}
	return nil
}
