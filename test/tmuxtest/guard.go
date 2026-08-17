// Package tmuxtest provides helpers for integration tests that need real tmux.
//
// Guard manages tmux session lifecycle for tests: it generates unique city
// names with a "gctest-" prefix, tracks created sessions, and guarantees
// cleanup even on test failures. Three layers prevent orphan sessions:
//
//  1. Pre-sweep (TestMain): kill all gctest-* socket servers from prior crashes.
//  2. Per-test (t.Cleanup): kill sessions created by this guard.
//  3. Post-sweep (TestMain defer): final sweep after all tests complete.
//
// All operations use isolated tmux socket roots and named gctest-* sockets so
// tests never interfere with the user's running tmux server.
package tmuxtest

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

const (
	tmuxGuardCommandTimeout  = 2 * time.Second
	tmuxServerCaptureTimeout = 20 * time.Second
	tmuxServerCaptureRetry   = 20 * time.Millisecond
)

const tmuxSiblingSocketStaleAfter = 24 * time.Hour

const (
	tmuxEnv     = "TMUX"
	tmuxPaneEnv = "TMUX_PANE"
	tmuxTmpEnv  = "TMUX_TMPDIR"
)

// ConfigureProcessEnv points all tmux commands in the current process tree at
// socketRoot and removes inherited client bindings from an outer tmux session.
func ConfigureProcessEnv(socketRoot string) error {
	socketRoot = strings.TrimSpace(socketRoot)
	if socketRoot == "" {
		return fmt.Errorf("tmux socket root is empty")
	}
	if err := os.MkdirAll(socketRoot, 0o700); err != nil {
		return fmt.Errorf("creating tmux socket root %q: %w", socketRoot, err)
	}
	if err := os.Unsetenv(tmuxEnv); err != nil {
		return fmt.Errorf("unsetting %s: %w", tmuxEnv, err)
	}
	if err := os.Unsetenv(tmuxPaneEnv); err != nil {
		return fmt.Errorf("unsetting %s: %w", tmuxPaneEnv, err)
	}
	if err := os.Setenv(tmuxTmpEnv, socketRoot); err != nil {
		return fmt.Errorf("setting %s: %w", tmuxTmpEnv, err)
	}
	return nil
}

// RequireTmux skips the test if tmux is not installed.
func RequireTmux(t testing.TB) {
	t.Helper()
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux not installed")
	}
}

// Guard manages tmux session lifecycle for a single test. It generates a
// unique city name with the "gctest-" prefix and guarantees cleanup of all
// sessions matching that city via t.Cleanup.
type Guard struct {
	t          testing.TB
	cityName   string // "gctest-<8hex>"
	socketName string // tmux socket for isolation (defaults to cityName)
	mu         sync.Mutex
	servers    map[int]testServerProcess
}

// NewGuard creates a guard with a unique city name. Registers t.Cleanup
// to kill all sessions created under this guard's city name.
func NewGuard(t testing.TB) *Guard {
	return NewGuardWithSocket(t, "")
}

// NewGuardWithSocket creates a guard using the specified tmux socket.
func NewGuardWithSocket(t testing.TB, socketName string) *Guard {
	t.Helper()
	RequireTmux(t)

	b := make([]byte, 4)
	if _, err := rand.Read(b); err != nil {
		t.Fatalf("tmuxtest: generating random city name: %v", err)
	}
	cityName := fmt.Sprintf("gctest-%x", b)
	if socketName == "" {
		socketName = cityName
	}

	g := &Guard{t: t, cityName: cityName, socketName: socketName}
	t.Cleanup(func() {
		g.killGuardSessions()
	})
	return g
}

// CityName returns the unique city name (e.g., "gctest-<8hex>").
func (g *Guard) CityName() string {
	return g.cityName
}

// SocketName returns the tmux socket name used by this guard.
func (g *Guard) SocketName() string {
	return g.socketName
}

// CaptureServer records the isolated tmux server's PID and process group.
// Call it immediately after the command that first creates the server. The
// recorded handle lets cleanup terminate the server and configuration-hook
// helpers even if the socket directory disappears before t.Cleanup runs.
func (g *Guard) CaptureServer() error {
	// NewGuard registers a fallback cleanup before callers create their
	// temporary socket root. Registering again here is intentional: Cleanup is
	// LIFO, so this capture-time cleanup runs before those later TempDir
	// cleanups can remove the socket needed by the exact tmux kill.
	g.t.Cleanup(func() {
		g.killGuardSessions()
	})

	ctx, cancel := context.WithTimeout(context.Background(), tmuxServerCaptureTimeout)
	defer cancel()
	retry := time.NewTicker(tmuxServerCaptureRetry)
	defer retry.Stop()
	handle, err := captureTestServerProcessUntil(ctx, retry.C, func() (testServerProcess, error) {
		return captureTestServerProcess(g.socketName, "")
	})
	if err != nil {
		return err
	}
	g.mu.Lock()
	if g.servers == nil {
		g.servers = make(map[int]testServerProcess)
	}
	g.servers[handle.pid] = handle
	g.mu.Unlock()

	ctx, cancel = context.WithTimeout(context.Background(), tmuxGuardCommandTimeout)
	defer cancel()
	args := tmuxArgs(g.socketName, "set-option", "-s", "exit-empty", "on")
	if out, err := exec.CommandContext(ctx, "tmux", args...).CombinedOutput(); err != nil {
		return fmt.Errorf("configuring isolated tmux server %d to exit when empty: %w: %s", handle.pid, err, strings.TrimSpace(string(out)))
	}
	return nil
}

func captureTestServerProcessUntil(
	ctx context.Context,
	retry <-chan time.Time,
	capture func() (testServerProcess, error),
) (testServerProcess, error) {
	var lastErr error
	for {
		handle, err := capture()
		if err == nil {
			return handle, nil
		}
		lastErr = err
		select {
		case <-ctx.Done():
			return testServerProcess{}, fmt.Errorf(
				"waiting for isolated tmux server readiness: %w (last identity error: %w)",
				ctx.Err(), lastErr,
			)
		case <-retry:
		}
	}
}

// SessionName returns the expected tmux session name for an agent.
// Default session naming is just the sanitized agent name because per-city
// tmux socket isolation makes a city prefix unnecessary.
func (g *Guard) SessionName(agentName string) string {
	return strings.ReplaceAll(agentName, "/", "--")
}

// HasSession checks if a specific tmux session exists.
func (g *Guard) HasSession(name string) bool {
	g.t.Helper()
	args := tmuxArgs(g.socketName, "has-session", "-t", name)
	out, err := exec.Command("tmux", args...).CombinedOutput()
	if err != nil {
		// tmux has-session exits 1 when session doesn't exist
		// and also when no server is running. Both mean "not found".
		_ = out
		return false
	}
	return true
}

// killGuardSessions kills all tmux sessions matching this guard's city
// socket. One city maps to one socket, so all sessions on that socket
// belong to this guard.
func (g *Guard) killGuardSessions() {
	g.t.Helper()
	g.mu.Lock()
	handles := make([]testServerProcess, 0, len(g.servers))
	for _, handle := range g.servers {
		handles = append(handles, handle)
	}
	g.mu.Unlock()
	if err := killTestSocketServerWithHandles(g.socketName, handles); err != nil {
		g.t.Logf("tmuxtest: cleaning socket %q: %v", g.socketName, err)
	}
}

// KillAllTestSessions kills tmux sessions for all orphaned gctest-* sockets.
// Call from TestMain before and after test runs to clean up orphans.
func KillAllTestSessions(t testing.TB) {
	t.Helper()
	var cleaned int
	for _, socketPath := range listTestSocketPaths() {
		if err := killTestSocketPath(socketPath); err == nil {
			cleaned++
		} else {
			t.Logf("tmuxtest: cleaning socket path %q: %v", socketPath, err)
		}
	}
	if cleaned > 0 {
		t.Logf("tmuxtest: cleaned up %d orphaned test socket(s)", cleaned)
	}
}

// tmuxArgs prepends -L socketName to the given tmux arguments when socketName
// is non-empty.
func tmuxArgs(socketName string, args ...string) []string {
	if socketName == "" {
		return args
	}
	return append([]string{"-L", socketName}, args...)
}

func killTestSocketServerWithHandles(socketName string, handles []testServerProcess) error {
	if handle, err := captureTestServerProcess(socketName, ""); err == nil {
		handles = append(handles, handle)
	}
	hasHandle := len(handles) > 0
	ctx, cancel := context.WithTimeout(context.Background(), tmuxGuardCommandTimeout)
	defer cancel()
	args := tmuxArgs(socketName, "kill-server")
	killErr := exec.CommandContext(ctx, "tmux", args...).Run()
	processErr := terminateCapturedTestServers(handles)
	socketErr := removeCapturedTestServerSockets(handles)
	if processErr == nil && socketErr == nil && (killErr == nil || hasHandle) {
		return nil
	}
	return errors.Join(killErr, processErr, socketErr)
}

func killTestSocketPath(socketPath string) error {
	var handles []testServerProcess
	if handle, err := captureTestServerProcess("", socketPath); err == nil {
		handles = append(handles, handle)
	}
	hasHandle := len(handles) > 0
	ctx, cancel := context.WithTimeout(context.Background(), tmuxGuardCommandTimeout)
	defer cancel()
	killErr := exec.CommandContext(ctx, "tmux", "-S", socketPath, "kill-server").Run()
	processErr := terminateCapturedTestServers(handles)
	socketErr := removeTestSocket(socketPath)
	if processErr == nil && socketErr == nil {
		return nil
	}
	if killErr == nil || hasHandle {
		killErr = nil
	}
	return errors.Join(killErr, processErr, socketErr)
}

func tmuxServerIdentity(socketName, socketPath string) (int, string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), tmuxGuardCommandTimeout)
	defer cancel()
	args := []string{"display-message", "-p", "#{pid}\t#{socket_path}"}
	switch {
	case socketPath != "":
		args = append([]string{"-S", socketPath}, args...)
	case socketName != "":
		args = tmuxArgs(socketName, args...)
	default:
		return 0, "", errors.New("refusing to inspect the default tmux server")
	}
	out, err := exec.CommandContext(ctx, "tmux", args...).CombinedOutput()
	if err != nil {
		return 0, "", fmt.Errorf("querying isolated tmux server identity: %w: %s", err, strings.TrimSpace(string(out)))
	}
	parts := strings.SplitN(strings.TrimSpace(string(out)), "\t", 2)
	if len(parts) != 2 {
		return 0, "", fmt.Errorf("parsing isolated tmux server identity %q", strings.TrimSpace(string(out)))
	}
	pid, err := strconv.Atoi(parts[0])
	if err != nil || pid <= 1 {
		return 0, "", fmt.Errorf("parsing isolated tmux server PID %q", parts[0])
	}
	reportedPath := filepath.Clean(parts[1])
	if !filepath.IsAbs(reportedPath) {
		return 0, "", fmt.Errorf("isolated tmux server %d reported non-absolute socket path %q", pid, parts[1])
	}
	if socketPath != "" && reportedPath != filepath.Clean(socketPath) {
		return 0, "", fmt.Errorf("isolated tmux server %d reported socket path %q, want %q", pid, reportedPath, filepath.Clean(socketPath))
	}
	if socketName != "" && filepath.Base(reportedPath) != socketName {
		return 0, "", fmt.Errorf("isolated tmux server %d reported socket name %q, want %q", pid, filepath.Base(reportedPath), socketName)
	}
	return pid, reportedPath, nil
}

func terminateCapturedTestServers(handles []testServerProcess) error {
	seen := make(map[int]struct{}, len(handles))
	var errs []error
	for _, handle := range handles {
		if _, ok := seen[handle.pid]; ok {
			continue
		}
		seen[handle.pid] = struct{}{}
		if err := terminateTestServerProcess(handle); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

func removeCapturedTestServerSockets(handles []testServerProcess) error {
	seen := make(map[string]struct{}, len(handles))
	var errs []error
	for _, handle := range handles {
		socketPath := filepath.Clean(handle.socketPath)
		if handle.socketPath == "" || socketPath == "." {
			continue
		}
		if _, ok := seen[socketPath]; ok {
			continue
		}
		seen[socketPath] = struct{}{}
		if err := removeTestSocket(socketPath); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

func removeTestSocket(socketPath string) error {
	socketPath = filepath.Clean(socketPath)
	if !filepath.IsAbs(socketPath) {
		return fmt.Errorf("refusing to remove non-absolute tmux socket path %q", socketPath)
	}
	info, err := os.Lstat(socketPath)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("inspecting isolated tmux socket %q: %w", socketPath, err)
	}
	if info.Mode()&os.ModeSocket == 0 {
		return fmt.Errorf("refusing to remove non-socket tmux path %q", socketPath)
	}
	if err := os.Remove(socketPath); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("removing isolated tmux socket %q: %w", socketPath, err)
	}
	return nil
}

// listTestSocketPaths returns tmux socket paths for orphaned gctest cities.
func listTestSocketPaths() []string {
	activeRoot := strings.TrimSpace(os.Getenv(tmuxTmpEnv))
	if activeRoot != "" {
		activeRoot = filepath.Clean(activeRoot)
	}
	now := time.Now()
	uid := strconv.Itoa(os.Getuid())
	var sockets []string
	for _, root := range tmuxSocketSearchRoots() {
		entries, err := filepath.Glob(filepath.Join(root, "tmux-"+uid, "*"))
		if err != nil {
			continue
		}
		for _, socketPath := range entries {
			if root == activeRoot || testSocketPathIsStale(socketPath, now) {
				sockets = append(sockets, socketPath)
			}
		}
	}
	return sockets
}

func testSocketPathIsStale(socketPath string, now time.Time) bool {
	info, err := os.Stat(socketPath)
	if err != nil {
		return false
	}
	return now.Sub(info.ModTime()) >= tmuxSiblingSocketStaleAfter
}

func tmuxSocketSearchRoots() []string {
	roots := make([]string, 0, 8)
	seen := make(map[string]struct{})
	addRoot := func(root string) {
		root = strings.TrimSpace(root)
		if root == "" {
			return
		}
		root = filepath.Clean(root)
		if _, ok := seen[root]; ok {
			return
		}
		seen[root] = struct{}{}
		roots = append(roots, root)
	}

	activeRoot := os.Getenv(tmuxTmpEnv)
	addRoot(activeRoot)
	for _, pattern := range tmuxSocketRootPatterns(activeRoot) {
		matches, err := filepath.Glob(pattern)
		if err != nil {
			continue
		}
		for _, match := range matches {
			addRoot(match)
		}
	}
	return roots
}

func tmuxSocketRootPatterns(activeRoot string) []string {
	activeRoot = strings.TrimSpace(activeRoot)
	if activeRoot == "" || filepath.Base(activeRoot) != "tmux" {
		return nil
	}
	activeRoot = filepath.Clean(activeRoot)
	runRoot := filepath.Dir(activeRoot)
	runName := filepath.Base(runRoot)
	namespace := filepath.Dir(runRoot)
	if runName == "runtime" {
		runRoot = filepath.Dir(runRoot)
		runName = filepath.Base(runRoot)
		namespace = filepath.Dir(runRoot)
		return runtimeTmuxSocketRootPatterns(namespace, runName)
	}
	return directTmuxSocketRootPatterns(namespace, runName)
}

func directTmuxSocketRootPatterns(namespace, runName string) []string {
	switch {
	case strings.HasPrefix(runName, "gc-integration-"):
		return []string{filepath.Join(namespace, "gc-integration-*", "tmux")}
	case strings.HasPrefix(runName, "gctutorial-"):
		return []string{filepath.Join(namespace, "gctutorial-*", "tmux")}
	case strings.HasPrefix(runName, "gct"):
		return []string{filepath.Join(namespace, "gct*", "tmux")}
	default:
		return nil
	}
}

func runtimeTmuxSocketRootPatterns(namespace, runName string) []string {
	switch {
	case strings.HasPrefix(runName, "gcac-"):
		return []string{filepath.Join(namespace, "gcac-*", "runtime", "tmux")}
	case strings.HasPrefix(runName, "gcwi-"):
		return []string{filepath.Join(namespace, "gcwi-*", "runtime", "tmux")}
	case strings.HasPrefix(runName, "gc-acceptance-b-"):
		return []string{filepath.Join(namespace, "gc-acceptance-b-*", "runtime", "tmux")}
	case strings.HasPrefix(runName, "gc-acceptance-"):
		return []string{filepath.Join(namespace, "gc-acceptance-*", "runtime", "tmux")}
	default:
		return nil
	}
}
