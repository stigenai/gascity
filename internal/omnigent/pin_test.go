package omnigent

import (
	"context"
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestVerifyExecutableAtRequiresDigestVersionAndCommit(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell fixture is Unix-specific")
	}
	path := filepath.Join(t.TempDir(), "omnigent")
	body := "#!/bin/sh\nprintf '%s\\n' 'omnigent 0.10.0.dev0 (2aba5079, built 2026-08-15T12:16:06Z)'\n"
	if err := os.WriteFile(path, []byte(body), 0o700); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256([]byte(body))
	pin := Pin{
		Commit:         "2aba5079d4d3a2a84d8c9927884fc4b8ce0eeecc",
		PackageVersion: "0.10.0.dev0",
		Executable:     path,
		SHA256:         fmt.Sprintf("sha256:%x", sum),
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	verified, err := VerifyExecutableAt(ctx, pin, path)
	if err != nil {
		t.Fatalf("VerifyExecutableAt: %v", err)
	}
	wantPath, err := filepath.EvalSymlinks(path)
	if err != nil {
		t.Fatal(err)
	}
	if verified.Path != wantPath || verified.SHA256 != pin.SHA256 {
		t.Fatalf("verified = %#v, want path/digest from pin", verified)
	}
	if !strings.Contains(verified.VersionOutput, pin.PackageVersion) {
		t.Fatalf("VersionOutput = %q, want package version", verified.VersionOutput)
	}
}

func TestVerifyExecutableAtRejectsMismatchAndUnsafeFile(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell fixture is Unix-specific")
	}
	tests := []struct {
		name       string
		body       string
		mode       os.FileMode
		pinDigest  string
		pinVersion string
		pinCommit  string
		want       string
	}{
		{
			name:       "digest",
			body:       "#!/bin/sh\necho 'omnigent 0.10.0.dev0 (2aba5079)'\n",
			mode:       0o700,
			pinDigest:  "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
			pinVersion: "0.10.0.dev0",
			pinCommit:  "2aba5079d4d3a2a84d8c9927884fc4b8ce0eeecc",
			want:       "digest mismatch",
		},
		{
			name:       "version",
			body:       "#!/bin/sh\necho 'omnigent 0.9.0 (2aba5079)'\n",
			mode:       0o700,
			pinVersion: "0.10.0.dev0",
			pinCommit:  "2aba5079d4d3a2a84d8c9927884fc4b8ce0eeecc",
			want:       "version mismatch",
		},
		{
			name:       "commit",
			body:       "#!/bin/sh\necho 'omnigent 0.10.0.dev0 (deadbeef)'\n",
			mode:       0o700,
			pinVersion: "0.10.0.dev0",
			pinCommit:  "2aba5079d4d3a2a84d8c9927884fc4b8ce0eeecc",
			want:       "commit mismatch",
		},
		{
			name:       "not executable",
			body:       "omnigent 0.10.0.dev0 (2aba5079)\n",
			mode:       0o600,
			pinVersion: "0.10.0.dev0",
			pinCommit:  "2aba5079d4d3a2a84d8c9927884fc4b8ce0eeecc",
			want:       "not executable",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "omnigent")
			if err := os.WriteFile(path, []byte(tt.body), tt.mode); err != nil {
				t.Fatal(err)
			}
			digest := tt.pinDigest
			if digest == "" {
				sum := sha256.Sum256([]byte(tt.body))
				digest = fmt.Sprintf("sha256:%x", sum)
			}
			_, err := VerifyExecutableAt(context.Background(), Pin{
				Commit:         tt.pinCommit,
				PackageVersion: tt.pinVersion,
				Executable:     path,
				SHA256:         digest,
			}, path)
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("VerifyExecutableAt error = %v, want substring %q", err, tt.want)
			}
		})
	}
}

func TestVerifyExecutableAtRejectsUnverifiableSourceBuild(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell fixture is Unix-specific")
	}
	path := filepath.Join(t.TempDir(), "omnigent")
	body := "#!/bin/sh\necho 'omnigent 0.10.0.dev0'\n"
	if err := os.WriteFile(path, []byte(body), 0o700); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256([]byte(body))
	_, err := VerifyExecutableAt(context.Background(), Pin{
		Commit:         "2aba5079d4d3a2a84d8c9927884fc4b8ce0eeecc",
		PackageVersion: "0.10.0.dev0",
		Executable:     path,
		SHA256:         fmt.Sprintf("sha256:%x", sum),
	}, path)
	if err == nil || !strings.Contains(err.Error(), "does not report a build commit") {
		t.Fatalf("VerifyExecutableAt error = %v, want unverifiable build commit", err)
	}
}

func TestVerifyExecutableDiagnosticsCoverReadinessMatrixWithoutLeakingOutput(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell fixture is Unix-specific")
	}
	t.Run("missing", func(t *testing.T) {
		pin := testPinForBody("#!/bin/sh\n", "/definitely/missing/omnigent")
		_, err := VerifyExecutableAt(context.Background(), pin, pin.Executable)
		if err == nil || !strings.Contains(err.Error(), "install the exact pinned") {
			t.Fatalf("error = %v, want remediation", err)
		}
	})
	t.Run("path with spaces", func(t *testing.T) {
		root := filepath.Join(t.TempDir(), "path with spaces")
		if err := os.MkdirAll(root, 0o700); err != nil {
			t.Fatal(err)
		}
		path := filepath.Join(root, "omnigent")
		body := "#!/bin/sh\necho 'omnigent 0.10.0.dev0 (2aba5079)'\n"
		if err := os.WriteFile(path, []byte(body), 0o700); err != nil {
			t.Fatal(err)
		}
		pin := testPinForBody(body, path)
		_, err := VerifyExecutableAt(context.Background(), pin, path)
		if err == nil || !strings.Contains(err.Error(), "path without whitespace") {
			t.Fatalf("error = %v, want whitespace remediation", err)
		}
	})
	t.Run("malformed output is redacted", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "omnigent")
		body := "#!/bin/sh\necho 'malformed SECRET_VALUE_123'\n"
		if err := os.WriteFile(path, []byte(body), 0o700); err != nil {
			t.Fatal(err)
		}
		pin := testPinForBody(body, path)
		_, err := VerifyExecutableAt(context.Background(), pin, path)
		if err == nil || !strings.Contains(err.Error(), "unsupported output") || strings.Contains(err.Error(), "SECRET_VALUE_123") {
			t.Fatalf("error = %v, want redacted unsupported-output diagnostic", err)
		}
	})
	for _, version := range []string{"0.9.9", "0.11.0"} {
		t.Run("version "+version, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "omnigent")
			body := fmt.Sprintf("#!/bin/sh\necho 'omnigent %s (2aba5079)'\n", version)
			if err := os.WriteFile(path, []byte(body), 0o700); err != nil {
				t.Fatal(err)
			}
			pin := testPinForBody(body, path)
			_, err := VerifyExecutableAt(context.Background(), pin, path)
			if err == nil || !strings.Contains(err.Error(), "version mismatch") || !strings.Contains(err.Error(), "install the exact pinned") {
				t.Fatalf("error = %v, want version remediation", err)
			}
		})
	}
}

func TestExplicitPinnedUpgradeAndRollbackAreReversible(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell fixture is Unix-specific")
	}
	root := t.TempDir()
	oldPath := filepath.Join(root, "omnigent-old")
	newPath := filepath.Join(root, "omnigent-new")
	oldBody := "#!/bin/sh\necho 'omnigent 0.10.0.dev0 (2aba5079)'\n"
	newBody := "#!/bin/sh\necho 'omnigent 0.11.0 (3bbb5079)'\n"
	if err := os.WriteFile(oldPath, []byte(oldBody), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(newPath, []byte(newBody), 0o700); err != nil {
		t.Fatal(err)
	}
	oldPin := testPinForBody(oldBody, oldPath)
	newPin := Pin{
		Commit: "3bbb5079d4d3a2a84d8c9927884fc4b8ce0eeecc", PackageVersion: "0.11.0",
		Executable: newPath, SHA256: testPinForBody(newBody, newPath).SHA256,
	}
	if _, err := VerifyExecutableAt(context.Background(), oldPin, oldPath); err != nil {
		t.Fatalf("verify old pin: %v", err)
	}
	if _, err := VerifyExecutableAt(context.Background(), oldPin, newPath); err == nil || !strings.Contains(err.Error(), "digest mismatch") {
		t.Fatalf("unapproved replacement error = %v", err)
	}
	if _, err := VerifyExecutableAt(context.Background(), newPin, newPath); err != nil {
		t.Fatalf("verify explicit upgrade pin: %v", err)
	}
	if _, err := VerifyExecutableAt(context.Background(), oldPin, oldPath); err != nil {
		t.Fatalf("verify rollback pin: %v", err)
	}
}

func testPinForBody(body, executable string) Pin {
	sum := sha256.Sum256([]byte(body))
	return Pin{
		Commit:         "2aba5079d4d3a2a84d8c9927884fc4b8ce0eeecc",
		PackageVersion: "0.10.0.dev0",
		Executable:     executable,
		SHA256:         fmt.Sprintf("sha256:%x", sum),
	}
}
