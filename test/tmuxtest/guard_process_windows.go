//go:build windows

package tmuxtest

import (
	"fmt"
	"os"
)

type testServerProcess struct {
	pid        int
	socketPath string
}

func captureTestServerProcess(socketName, socketPath string) (testServerProcess, error) {
	pid, capturedSocketPath, err := tmuxServerIdentity(socketName, socketPath)
	if err != nil {
		return testServerProcess{}, err
	}
	return testServerProcess{pid: pid, socketPath: capturedSocketPath}, nil
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
