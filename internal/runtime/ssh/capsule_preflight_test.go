package ssh

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/runtime"
)

func TestProviderCapsulePreflightPassesBeforeTmuxLaunch(t *testing.T) {
	t.Parallel()
	f := &fakeRunner{respond: successfulCapsulePreflightResponse}
	p := providerWith(f)
	if err := p.Start(context.Background(), "ga-ssh-capsule", testSSHCapsuleConfig(t)); err != nil {
		t.Fatalf("Start: %v", err)
	}
	newSession := callIndex(f.calls, isTmux("new-session"))
	version := callIndex(f.calls, func(argv []string) bool {
		return slices.Equal(argv, []string{"/opt/gascity/bin/omnigent", "--version"})
	})
	if newSession < 0 || version < 0 || version >= newSession {
		t.Fatalf("preflight/launch call order = %#v", f.calls)
	}
}

func TestProviderCapsulePreflightFailureMatrixNeverStartsTmux(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		kind CapsulePreflightFailureKind
		fail func([]string) ([]byte, int, error, bool)
	}{
		{
			name: "transport failure", kind: CapsulePreflightTransport,
			fail: func(argv []string) ([]byte, int, error, bool) {
				return nil, -1, context.DeadlineExceeded, len(argv) == 2 && argv[0] == "uname"
			},
		},
		{
			name: "unsupported platform", kind: CapsulePreflightUnsupportedPlatform,
			fail: func(argv []string) ([]byte, int, error, bool) {
				return []byte("FreeBSD\n"), 0, nil, len(argv) == 2 && argv[0] == "uname"
			},
		},
		{
			name: "unsupported shell", kind: CapsulePreflightUnsupportedShell,
			fail: func(argv []string) ([]byte, int, error, bool) {
				return nil, 2, nil, isShellProbe(argv)
			},
		},
		{
			name: "missing tmux", kind: CapsulePreflightMissingBinary,
			fail: func(argv []string) ([]byte, int, error, bool) {
				return nil, 1, nil, isCommandLookup(argv, "tmux")
			},
		},
		{
			name: "missing gc", kind: CapsulePreflightMissingBinary,
			fail: func(argv []string) ([]byte, int, error, bool) {
				return nil, 1, nil, isCommandLookup(argv, "gc")
			},
		},
		{
			name: "missing omnigent", kind: CapsulePreflightMissingBinary,
			fail: func(argv []string) ([]byte, int, error, bool) {
				return nil, 1, nil, isCommandLookup(argv, "omnigent")
			},
		},
		{
			name: "tmux too old", kind: CapsulePreflightMissingBinary,
			fail: func(argv []string) ([]byte, int, error, bool) {
				return []byte("tmux 3.1c\n"), 0, nil, slices.Equal(argv, []string{"/usr/bin/tmux", "-V"})
			},
		},
		{
			name: "gc lacks capsule attachment", kind: CapsulePreflightMissingBinary,
			fail: func(argv []string) ([]byte, int, error, bool) {
				return nil, 2, nil, slices.Equal(argv, []string{"/opt/gascity/bin/gc", "omnigent", "attach", "--help"})
			},
		},
		{
			name: "pin digest mismatch", kind: CapsulePreflightPinMismatch,
			fail: func(argv []string) ([]byte, int, error, bool) {
				return []byte(strings.Repeat("0", 64) + "  /opt/gascity/bin/omnigent\n"), 0, nil, len(argv) > 0 && argv[0] == "sha256sum"
			},
		},
		{
			name: "pin version mismatch", kind: CapsulePreflightPinMismatch,
			fail: func(argv []string) ([]byte, int, error, bool) {
				return []byte("omnigent 0.9.0 (bbbbbbbb)\n"), 0, nil, slices.Equal(argv, []string{"/opt/gascity/bin/omnigent", "--version"})
			},
		},
		{
			name: "pin commit mismatch", kind: CapsulePreflightPinMismatch,
			fail: func(argv []string) ([]byte, int, error, bool) {
				return []byte("omnigent 0.10.0.dev0 (cccccccc)\n"), 0, nil, slices.Equal(argv, []string{"/opt/gascity/bin/omnigent", "--version"})
			},
		},
		{
			name: "unwritable workspace", kind: CapsulePreflightUnwritablePath,
			fail: func(argv []string) ([]byte, int, error, bool) {
				return nil, 1, nil, slices.Equal(argv, []string{"test", "-w", "/srv/gascity/work"})
			},
		},
		{
			name: "missing profile auth", kind: CapsulePreflightMissingProfileAuth,
			fail: func(argv []string) ([]byte, int, error, bool) {
				return nil, 1, nil, slices.Equal(argv, []string{"test", "-f", "/srv/gascity/secrets/claude-primary"})
			},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := &fakeRunner{respond: func(argv []string) ([]byte, int, error) {
				if out, code, err, ok := tc.fail(argv); ok {
					return out, code, err
				}
				return successfulCapsulePreflightResponse(argv)
			}}
			err := providerWith(f).Start(context.Background(), "ga-ssh-capsule", testSSHCapsuleConfig(t))
			var preflightErr *CapsulePreflightError
			if !errors.As(err, &preflightErr) || preflightErr.Kind != tc.kind {
				t.Fatalf("Start error = %v, want preflight kind %q", err, tc.kind)
			}
			if tc.kind == CapsulePreflightTransport && !errors.Is(err, runtime.ErrRuntimeUnavailable) {
				t.Fatalf("transport preflight error = %v, want ErrRuntimeUnavailable", err)
			}
			if firstCall(f, isTmux("new-session")) != nil {
				t.Fatalf("failed preflight launched tmux: %#v", f.calls)
			}
			if strings.Contains(err.Error(), "/srv/gascity/secrets/claude-primary") {
				t.Fatalf("preflight error disclosed credential path: %v", err)
			}
		})
	}
}

func testSSHCapsuleConfig(t *testing.T) runtime.Config {
	t.Helper()
	key, err := runtime.NewCapsuleKey("ssh/box/city", "ga-ssh-capsule")
	if err != nil {
		t.Fatal(err)
	}
	catalogBytes := []byte("version: 1\nprofiles: {}\n")
	catalogSource := filepath.Join(t.TempDir(), "profiles.yaml")
	if err := os.WriteFile(catalogSource, catalogBytes, 0o600); err != nil {
		t.Fatal(err)
	}
	catalogDigest := sha256.Sum256(catalogBytes)
	return runtime.Config{
		WorkDir: "/srv/gascity/work",
		Capsule: &runtime.CapsuleLaunchConfig{
			Key: key,
			State: runtime.CapsuleStateReference{
				Key: key, Provider: "ssh", ResourceID: "/var/lib/gascity/omnigent",
				ResourceUID: "ssh-state-v1", MountPath: "/var/lib/gascity/omnigent",
			},
			Command: []string{"gc", "omnigent", "attach", "--mode", "capsule"},
			RunRoot: "/run/gascity/omnigent", SocketPath: "/run/gascity/omnigent/sidecar.sock",
			CatalogResourceID: "catalog-sha", CatalogMountPath: "/etc/gascity/omnigent",
			CatalogSHA256: "sha256:" + strings.Repeat("a", 64),
			CatalogInputs: []runtime.CapsuleInput{{
				SourcePath: catalogSource, RelativePath: "profiles.yaml",
				SHA256: "sha256:" + hex.EncodeToString(catalogDigest[:]), Mode: 0o644,
			}},
			ExecutablePin: runtime.CapsuleExecutablePin{
				Executable: "omnigent", PackageVersion: "0.10.0.dev0",
				Commit: strings.Repeat("b", 40), SHA256: "sha256:" + strings.Repeat("d", 64),
			},
			Network: runtime.CapsuleNetworkExternalModel,
		},
		SecretReferences: []runtime.SecretReference{{
			ID: "claude-primary", Environment: "CLAUDE_AUTH_TOKEN",
			SSH: &runtime.SSHSecretPathReference{Path: "/srv/gascity/secrets/claude-primary"},
		}},
	}
}

func successfulCapsulePreflightResponse(argv []string) ([]byte, int, error) {
	switch {
	case len(argv) == 2 && argv[0] == "uname" && argv[1] == "-s":
		return []byte("Linux\n"), 0, nil
	case isShellProbe(argv):
		return nil, 0, nil
	case isCommandLookup(argv, "tmux"):
		return []byte("/usr/bin/tmux\n"), 0, nil
	case isCommandLookup(argv, "gc"):
		return []byte("/opt/gascity/bin/gc\n"), 0, nil
	case isCommandLookup(argv, "omnigent"):
		return []byte("/opt/gascity/bin/omnigent\n"), 0, nil
	case slices.Equal(argv, []string{"/usr/bin/tmux", "-V"}):
		return []byte("tmux 3.4\n"), 0, nil
	case slices.Equal(argv, []string{"/opt/gascity/bin/gc", "omnigent", "attach", "--help"}):
		return nil, 0, nil
	case len(argv) > 0 && argv[0] == "sha256sum":
		return []byte(strings.Repeat("d", 64) + "  /opt/gascity/bin/omnigent\n"), 0, nil
	case slices.Equal(argv, []string{"/opt/gascity/bin/omnigent", "--version"}):
		return []byte("omnigent 0.10.0.dev0 (bbbbbbbb)\n"), 0, nil
	case len(argv) > 0 && argv[0] == "test":
		return nil, 0, nil
	case isTmux("has-session")(argv):
		return nil, 1, nil
	default:
		return nil, 0, nil
	}
}

func isCommandLookup(argv []string, binary string) bool {
	return len(argv) == 5 && argv[0] == "sh" && argv[1] == "-c" && argv[3] == "gc-preflight" && argv[4] == binary
}

func isShellProbe(argv []string) bool {
	return len(argv) == 5 && argv[0] == "sh" && argv[1] == "-c" && argv[3] == "gc-shell-probe" && argv[4] == "gc-shell-probe"
}

func callIndex(calls [][]string, predicate func([]string) bool) int {
	for i, call := range calls {
		if predicate(call) {
			return i
		}
	}
	return -1
}
