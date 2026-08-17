//go:build integration

package ssh

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	stdruntime "runtime"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/runtime"
)

func TestSSHCapsuleCredentialScriptsIsolateClaudeAndCodexProfiles(t *testing.T) {
	root := filepath.Join(t.TempDir(), "profile roots with spaces ' quoted")
	privateRoot := filepath.Join(root, "private")
	runRoot := filepath.Join(root, "run")
	for _, path := range []string{privateRoot, runRoot} {
		if err := os.MkdirAll(path, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	primaryHome := privateTestDir(t, privateRoot, "claude-primary")
	secondaryHome := privateTestDir(t, privateRoot, "claude-secondary")
	codexHome := privateTestDir(t, privateRoot, "codex")
	primaryToken := privateTestFile(t, privateRoot, "primary.token", "primary-token-sentinel")
	secondaryToken := privateTestFile(t, privateRoot, "secondary.token", "secondary-token-sentinel")

	platform := "Linux"
	if stdruntime.GOOS == "darwin" {
		platform = "Darwin"
	}
	for _, path := range []string{primaryHome, secondaryHome, codexHome, primaryToken, secondaryToken} {
		out, code, err := (localCommandRunner{}).run(context.Background(), Endpoint{}, []string{
			"sh", "-c", remoteCapsuleCredentialCheckScript, "gc-capsule-secret-check-v1", platform, path,
		}, nil)
		if err != nil || code != 0 || len(out) != 0 {
			t.Fatalf("private source check %q = output %q, code %d, error %v", filepath.Base(path), out, code, err)
		}
	}

	claudePrimaryMount := filepath.Join(runRoot, "credentials", "claude-primary")
	claudeSecondaryMount := filepath.Join(runRoot, "credentials", "claude-secondary")
	if err := os.MkdirAll(filepath.Dir(claudePrimaryMount), 0o700); err != nil {
		t.Fatal(err)
	}
	claudeLauncher := writeCredentialLauncher(t, root, "claude-launcher", []runtime.SecretReference{
		{ID: "claude-primary-home", MountPath: claudePrimaryMount, SSH: &runtime.SSHSecretPathReference{Path: primaryHome}},
		{ID: "claude-secondary-home", MountPath: claudeSecondaryMount, SSH: &runtime.SSHSecretPathReference{Path: secondaryHome}},
		{ID: "primary-token", Environment: "CLAUDE_PRIMARY_TOKEN", SSH: &runtime.SSHSecretPathReference{Path: primaryToken}},
		{ID: "secondary-token", Environment: "CLAUDE_SECONDARY_TOKEN", SSH: &runtime.SSHSecretPathReference{Path: secondaryToken}},
	})
	primaryResult := filepath.Join(root, "primary-result")
	secondaryResult := filepath.Join(root, "secondary-result")
	tokenResult := filepath.Join(root, "token-result")
	child := `readlink "$1" >"$3" && readlink "$2" >"$4" && printf %s "$CLAUDE_PRIMARY_TOKEN:$CLAUDE_SECONDARY_TOKEN" >"$5"`
	cmd := exec.Command("sh", claudeLauncher, "--", "sh", "-c", child, "gc-profile-proof", //nolint:gosec // exact test fixture argv
		claudePrimaryMount, claudeSecondaryMount, primaryResult, secondaryResult, tokenResult)
	if output, err := cmd.CombinedOutput(); err != nil || len(output) != 0 {
		t.Fatalf("Claude launcher = output %q, error %v", output, err)
	}
	assertFileText(t, primaryResult, primaryHome+"\n")
	assertFileText(t, secondaryResult, secondaryHome+"\n")
	assertFileText(t, tokenResult, "primary-token-sentinel:secondary-token-sentinel")

	codexResult := filepath.Join(root, "codex-result")
	codexLauncher := writeCredentialLauncher(t, root, "codex-launcher", []runtime.SecretReference{{
		ID: "codex-home", Environment: "CODEX_HOME", SSH: &runtime.SSHSecretPathReference{Path: codexHome},
	}})
	cmd = exec.Command("sh", codexLauncher, "--", "sh", "-c", `printf %s "$CODEX_HOME" >"$1"`, "gc-profile-proof", codexResult) //nolint:gosec // exact test fixture argv
	if output, err := cmd.CombinedOutput(); err != nil || len(output) != 0 {
		t.Fatalf("Codex launcher = output %q, error %v", output, err)
	}
	assertFileText(t, codexResult, codexHome)

	if err := os.Chmod(primaryToken, 0o644); err != nil {
		t.Fatal(err)
	}
	out, code, err := (localCommandRunner{}).run(context.Background(), Endpoint{}, []string{
		"sh", "-c", remoteCapsuleCredentialCheckScript, "gc-capsule-secret-check-v1", platform, primaryToken,
	}, nil)
	if err != nil || code != 75 || len(out) != 0 {
		t.Fatalf("world-readable source check = output %q, code %d, error %v", out, code, err)
	}
}

func TestSSHCapsuleCredentialLauncherFailureIsGeneric(t *testing.T) {
	root := t.TempDir()
	source := privateTestDir(t, root, "source-secret-home")
	destination := filepath.Join(root, "occupied")
	if err := os.WriteFile(destination, []byte("credential-value-must-not-print"), 0o600); err != nil {
		t.Fatal(err)
	}
	launcher := writeCredentialLauncher(t, root, "launcher", []runtime.SecretReference{{
		ID: "profile-home", MountPath: destination, SSH: &runtime.SSHSecretPathReference{Path: source},
	}})
	cmd := exec.Command("sh", launcher, "--", "true") //nolint:gosec // exact test fixture argv
	output, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatal("credential launcher accepted occupied destination")
	}
	text := string(output)
	if !strings.Contains(text, "credential projection failed") {
		t.Fatalf("generic failure output = %q", text)
	}
	for _, forbidden := range []string{source, destination, "credential-value-must-not-print"} {
		if strings.Contains(text, forbidden) {
			t.Fatalf("credential failure output disclosed %q: %q", forbidden, text)
		}
	}
}

func privateTestDir(t *testing.T, root, name string) string {
	t.Helper()
	path := filepath.Join(root, name)
	if err := os.Mkdir(path, 0o700); err != nil {
		t.Fatal(err)
	}
	return path
}

func privateTestFile(t *testing.T, root, name, content string) string {
	t.Helper()
	path := filepath.Join(root, name)
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func writeCredentialLauncher(t *testing.T, root, name string, refs []runtime.SecretReference) string {
	t.Helper()
	path := filepath.Join(root, name)
	if err := os.WriteFile(path, []byte(renderCapsuleCredentialLauncher(refs)), 0o700); err != nil {
		t.Fatal(err)
	}
	return path
}

func assertFileText(t *testing.T, path, want string) {
	t.Helper()
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != want {
		t.Fatalf("%s = %q, want %q", filepath.Base(path), got, want)
	}
}
