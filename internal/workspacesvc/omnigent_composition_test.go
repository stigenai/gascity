package workspacesvc

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/omnigent"
	gcruntime "github.com/gastownhall/gascity/internal/runtime"
)

func TestOmnigentProxyProcessComposition(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Omnigent local service composition requires Unix sockets")
	}
	if mode := omnigentCompositionMode(os.Args); mode != "" {
		runOmnigentCompositionHelper(mode)
		return
	}

	cityPath := t.TempDir()
	stateRoot := filepath.Join(cityPath, ".gc", "services", "omnigent")
	configDir := filepath.Join(stateRoot, "config")
	if err := os.MkdirAll(configDir, 0o700); err != nil {
		t.Fatal(err)
	}
	testBinary, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	if strings.ContainsAny(testBinary, "'\n") {
		t.Fatalf("test binary path cannot be safely embedded: %q", testBinary)
	}
	wrapper := filepath.Join(stateRoot, "omnigent-fixture")
	childPIDPath := filepath.Join(stateRoot, "run", "child.pid")
	wrapperBody := fmt.Sprintf("#!/bin/sh\nif [ \"$1\" = \"--version\" ]; then echo 'omnigent 0.10.0.dev0 (2aba5079)'; exit 0; fi\nprintf '%%s\\n' \"$$\" > '%s'\nexec '%s' -test.run='^TestOmnigentProxyProcessComposition$' -- child-helper \"$@\"\n", childPIDPath, testBinary)
	if err := os.WriteFile(wrapper, []byte(wrapperBody), 0o700); err != nil {
		t.Fatal(err)
	}
	agentPath := filepath.Join(configDir, "mock-agent.yaml")
	if err := os.WriteFile(agentPath, []byte("name: offline-mock\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256([]byte(wrapperBody))
	catalog := fmt.Sprintf(`version: 1
omnigent:
  commit: 2aba5079d4d3a2a84d8c9927884fc4b8ce0eeecc
  package_version: 0.10.0.dev0
  executable: %s
  sha256: sha256:%x
profiles:
  offline:
    display_name: Offline
    blurb: Deterministic local composition fixture.
    harness: codex
    backend: loopback
    network: offline
    agent: mock-agent.yaml
`, wrapper, digest)
	if err := os.WriteFile(filepath.Join(configDir, "profiles.yaml"), []byte(catalog), 0o600); err != nil {
		t.Fatal(err)
	}

	rt := &testRuntime{
		cityPath: cityPath,
		cityName: "omnigent-composition",
		cfg: &config.City{Services: []config.Service{{
			Name: "omnigent",
			Kind: "proxy_process",
			Process: config.ServiceProcessConfig{
				Command:    []string{testBinary, "-test.run=^TestOmnigentProxyProcessComposition$", "--", "service-helper"},
				HealthPath: "/health",
			},
		}}},
		sp:    gcruntime.NewFake(),
		store: beads.NewMemStore(),
	}
	mgr := NewManager(rt)
	closed := false
	t.Cleanup(func() {
		if !closed {
			if err := mgr.Close(); err != nil {
				t.Errorf("Close: %v", err)
			}
		}
	})
	if err := mgr.Reload(); err != nil {
		t.Fatalf("Reload: %v", err)
	}
	assertOmnigentCompositionReady(t, mgr, stateRoot)
	firstChildPID := readOmnigentCompositionChildPID(t, childPIDPath)

	if err := mgr.Restart("omnigent"); err != nil {
		t.Fatalf("Restart: %v", err)
	}
	assertOmnigentCompositionReady(t, mgr, stateRoot)
	assertOmnigentCompositionProcessExited(t, firstChildPID)
	secondChildPID := readOmnigentCompositionChildPID(t, childPIDPath)
	if secondChildPID == firstChildPID {
		t.Fatalf("Restart reused child pid %d", secondChildPID)
	}
	if err := mgr.Close(); err != nil {
		t.Errorf("Close: %v", err)
	}
	closed = true
	assertOmnigentCompositionProcessExited(t, secondChildPID)
}

func readOmnigentCompositionChildPID(t *testing.T, path string) int {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read Omnigent child pid: %v", err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil || pid <= 0 {
		t.Fatalf("parse Omnigent child pid %q: %v", data, err)
	}
	return pid
}

func assertOmnigentCompositionProcessExited(t *testing.T, pid int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if err := syscall.Kill(pid, 0); errors.Is(err, syscall.ESRCH) {
			return
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatalf("Omnigent child process %d survived supervised shutdown", pid)
}

func assertOmnigentCompositionReady(t *testing.T, mgr *Manager, stateRoot string) {
	t.Helper()
	status, ok := mgr.Get("omnigent")
	if !ok {
		t.Fatal("Omnigent service status missing")
	}
	if status.LocalState != "ready" || status.StateRoot != filepath.ToSlash(filepath.Join(".gc", "services", "omnigent")) {
		t.Fatalf("status = %+v, want ready city-scoped service", status)
	}
	for _, name := range []string{"config", "data", "run", "secrets"} {
		info, err := os.Stat(filepath.Join(stateRoot, name))
		if err != nil || !info.IsDir() || info.Mode().Perm() != 0o700 {
			t.Fatalf("city-scoped %s directory: mode=%v err=%v", name, infoModeForOmnigentComposition(info), err)
		}
	}
	req := httptest.NewRequest(http.MethodGet, "/svc/omnigent/gascity/v1/profiles", nil)
	rec := httptest.NewRecorder()
	if !mgr.ServeHTTP(rec, req) {
		t.Fatal("Omnigent service request was not routed")
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("profile status = %d, body=%q", rec.Code, rec.Body.String())
	}
	var profiles []omnigent.PublicProfile
	if err := json.NewDecoder(rec.Body).Decode(&profiles); err != nil {
		t.Fatalf("decode profiles: %v", err)
	}
	if len(profiles) != 1 || profiles[0].ID != "offline" || profiles[0].Availability != "available" {
		t.Fatalf("profiles = %#v", profiles)
	}
}

func infoModeForOmnigentComposition(info os.FileInfo) os.FileMode {
	if info == nil {
		return 0
	}
	return info.Mode().Perm()
}

func omnigentCompositionMode(args []string) string {
	for i, arg := range args {
		if arg == "--" && i+1 < len(args) {
			return args[i+1]
		}
	}
	return ""
}

func runOmnigentCompositionHelper(mode string) {
	switch mode {
	case "service-helper":
		ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
		err := omnigent.ServeSidecar(ctx, omnigent.SidecarConfig{
			StateRoot:  os.Getenv("GC_SERVICE_STATE_ROOT"),
			SocketPath: os.Getenv("GC_SERVICE_SOCKET"),
			Stdout:     io.Discard,
			Stderr:     io.Discard,
		})
		stop()
		if err != nil {
			os.Exit(71)
		}
		os.Exit(0)
	case "child-helper":
		port := ""
		for i, arg := range os.Args {
			if arg == "--port" && i+1 < len(os.Args) {
				port = os.Args[i+1]
				break
			}
		}
		if port == "" {
			os.Exit(72)
		}
		listener, err := net.Listen("tcp", "127.0.0.1:"+port)
		if err != nil {
			os.Exit(73)
		}
		server := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path != "/health" {
				http.NotFound(w, r)
				return
			}
			w.WriteHeader(http.StatusNoContent)
		})}
		if err := server.Serve(listener); err != nil && err != http.ErrServerClosed {
			os.Exit(74)
		}
		os.Exit(0)
	default:
		os.Exit(75)
	}
}
