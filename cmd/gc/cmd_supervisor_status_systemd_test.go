package main

import (
	"bytes"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
)

// TestSupervisorStatusSocketAliveSkipsSystemdCheck verifies that when the
// control socket is reachable, supervisorStatusWithOptions returns 0 with the
// normal "running" message and does not invoke supervisorSystemctlActive.
func TestSupervisorStatusSocketAliveSkipsSystemdCheck(t *testing.T) {
	gcHome := shortTempDir(t, "gc-home-")
	t.Setenv("GC_HOME", gcHome)

	sockPath := filepath.Join(gcHome, "supervisor.sock")
	startTestSupervisorSocket(t, sockPath, func(cmd string) string {
		if cmd == "ping" {
			return "9999\n"
		}
		return ""
	})

	old := supervisorSystemctlActive
	supervisorSystemctlActive = func(_ string) bool {
		t.Fatal("supervisorSystemctlActive must not be called when socket is reachable")
		return false
	}
	t.Cleanup(func() { supervisorSystemctlActive = old })

	var stdout, stderr bytes.Buffer
	code := supervisorStatusWithOptions(&stdout, &stderr, false)
	if code != 0 {
		t.Fatalf("supervisorStatusWithOptions() = %d, want 0; stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
	if !strings.Contains(stdout.String(), "Supervisor is running (PID 9999)") {
		t.Fatalf("stdout = %q, want 'Supervisor is running (PID 9999)'", stdout.String())
	}
}

// TestSupervisorStatusSystemdActiveSocketDead verifies the new message emitted
// when the unix socket is unreachable but systemd reports the service active.
func TestSupervisorStatusSystemdActiveSocketDead(t *testing.T) {
	gcHome := shortTempDir(t, "gc-home-")
	t.Setenv("GC_HOME", gcHome)

	oldGOOS := supervisorRuntimeGOOS
	supervisorRuntimeGOOS = "linux"
	t.Cleanup(func() { supervisorRuntimeGOOS = oldGOOS })

	old := supervisorSystemctlActive
	supervisorSystemctlActive = func(_ string) bool { return true }
	t.Cleanup(func() { supervisorSystemctlActive = old })

	var stdout, stderr bytes.Buffer
	code := supervisorStatusWithOptions(&stdout, &stderr, false)
	if code != 1 {
		t.Fatalf("supervisorStatusWithOptions() = %d, want 1", code)
	}
	got := stdout.String()
	const wantPrefix = "Supervisor running (systemd) but socket unreachable at "
	if !strings.HasPrefix(got, wantPrefix) {
		t.Fatalf("stdout = %q, want prefix %q", got, wantPrefix)
	}
	if !strings.Contains(got, gcHome) {
		t.Fatalf("stdout = %q, want it to contain socket path under %q", got, gcHome)
	}
}

// TestSupervisorStatusSystemdInactiveSocketDead verifies that the original
// "not running" message is preserved when both socket and systemd report the
// supervisor as inactive.
func TestSupervisorStatusSystemdInactiveSocketDead(t *testing.T) {
	gcHome := shortTempDir(t, "gc-home-")
	t.Setenv("GC_HOME", gcHome)

	oldGOOS := supervisorRuntimeGOOS
	supervisorRuntimeGOOS = "linux"
	t.Cleanup(func() { supervisorRuntimeGOOS = oldGOOS })

	old := supervisorSystemctlActive
	supervisorSystemctlActive = func(_ string) bool { return false }
	t.Cleanup(func() { supervisorSystemctlActive = old })

	var stdout, stderr bytes.Buffer
	code := supervisorStatusWithOptions(&stdout, &stderr, false)
	if code != 1 {
		t.Fatalf("supervisorStatusWithOptions() = %d, want 1", code)
	}
	if got := strings.TrimSpace(stdout.String()); got != "Supervisor is not running" {
		t.Fatalf("stdout = %q, want 'Supervisor is not running'", got)
	}
}

// TestSupervisorStatusNonLinuxSkipsSystemdCheck verifies that on non-linux
// platforms the systemd check is skipped and the existing "not running"
// message is returned unchanged.
func TestSupervisorStatusNonLinuxSkipsSystemdCheck(t *testing.T) {
	gcHome := shortTempDir(t, "gc-home-")
	t.Setenv("GC_HOME", gcHome)

	oldGOOS := supervisorRuntimeGOOS
	supervisorRuntimeGOOS = "darwin"
	t.Cleanup(func() { supervisorRuntimeGOOS = oldGOOS })

	old := supervisorSystemctlActive
	supervisorSystemctlActive = func(_ string) bool {
		t.Fatal("supervisorSystemctlActive must not be called on non-linux")
		return false
	}
	t.Cleanup(func() { supervisorSystemctlActive = old })

	var stdout, stderr bytes.Buffer
	code := supervisorStatusWithOptions(&stdout, &stderr, false)
	if code != 1 {
		t.Fatalf("supervisorStatusWithOptions() = %d, want 1", code)
	}
	if got := strings.TrimSpace(stdout.String()); got != "Supervisor is not running" {
		t.Fatalf("stdout = %q, want 'Supervisor is not running'", got)
	}
}

// TestSupervisorStatusJSONSystemdFields verifies that the JSON output always
// includes the additive systemd_active and systemd_service fields while
// preserving running=false/pid=0 semantics when no socket is reachable.
func TestSupervisorStatusJSONSystemdFields(t *testing.T) {
	for _, tc := range []struct {
		name   string
		active bool
	}{
		{"systemd_active", true},
		{"systemd_inactive", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			gcHome := shortTempDir(t, "gc-home-")
			t.Setenv("GC_HOME", gcHome)

			oldGOOS := supervisorRuntimeGOOS
			supervisorRuntimeGOOS = "linux"
			t.Cleanup(func() { supervisorRuntimeGOOS = oldGOOS })

			old := supervisorSystemctlActive
			supervisorSystemctlActive = func(_ string) bool { return tc.active }
			t.Cleanup(func() { supervisorSystemctlActive = old })

			var stdout, stderr bytes.Buffer
			supervisorStatusWithOptions(&stdout, &stderr, true)

			var payload struct {
				Running        bool   `json:"running"`
				PID            int    `json:"pid"`
				SystemdActive  bool   `json:"systemd_active"`
				SystemdService string `json:"systemd_service"`
			}
			if err := json.Unmarshal(stdout.Bytes(), &payload); err != nil {
				t.Fatalf("stdout is not valid JSON: %v\n%s", err, stdout.String())
			}
			if payload.Running {
				t.Fatalf("running = true, want false when no socket present")
			}
			if payload.PID != 0 {
				t.Fatalf("pid = %d, want 0", payload.PID)
			}
			if payload.SystemdActive != tc.active {
				t.Fatalf("systemd_active = %v, want %v", payload.SystemdActive, tc.active)
			}
			if payload.SystemdService == "" {
				t.Fatalf("systemd_service is empty, want a non-empty service name")
			}
		})
	}
}
