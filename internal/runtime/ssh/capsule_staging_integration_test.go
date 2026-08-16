//go:build integration

package ssh

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func TestRemoteAtomicStageScriptFilesystemContract(t *testing.T) {
	root := filepath.Join(t.TempDir(), "remote catalog with spaces 多")
	if err := os.Mkdir(root, 0o700); err != nil {
		t.Fatal(err)
	}
	relative := "agents/profile ü.yaml"
	first := []byte("name: first\nprompt: work\n")
	if code, output := runRemoteStageScript(t, root, relative, stageTestDigest(first), "0640", first); code != 0 {
		t.Fatalf("first stage exit=%d output=%s", code, output)
	}
	target := filepath.Join(root, filepath.FromSlash(relative))
	assertStageTestFile(t, target, first, 0o640)
	firstInfo := stageTestInfo(t, target)

	if code, output := runRemoteStageScript(t, root, relative, stageTestDigest(first), "0640", first); code != 0 {
		t.Fatalf("idempotent stage exit=%d output=%s", code, output)
	}
	if got := stageTestInfo(t, target); !os.SameFile(firstInfo, got) {
		t.Fatal("idempotent stage replaced the existing file")
	}

	second := []byte("name: second\nprompt: changed\n")
	if code, output := runRemoteStageScript(t, root, relative, stageTestDigest(second), "0644", second); code != 0 {
		t.Fatalf("replacement stage exit=%d output=%s", code, output)
	}
	assertStageTestFile(t, target, second, 0o644)
	if got := stageTestInfo(t, target); os.SameFile(firstInfo, got) {
		t.Fatal("atomic replacement retained the old file identity")
	}
	assertNoStageTemps(t, filepath.Dir(target))

	t.Run("partial transfer", func(t *testing.T) {
		rel := "partial.yaml"
		if code, _ := runRemoteStageScript(t, root, rel, stageTestDigest([]byte("complete")), "0644", []byte("part")); code != 74 {
			t.Fatalf("partial stage exit=%d, want 74", code)
		}
		if _, err := os.Lstat(filepath.Join(root, rel)); !os.IsNotExist(err) {
			t.Fatalf("partial transfer published target: %v", err)
		}
		assertNoStageTemps(t, root)
	})

	t.Run("symlink parent escape", func(t *testing.T) {
		outside := t.TempDir()
		link := filepath.Join(root, "escape")
		if err := os.Symlink(outside, link); err != nil {
			t.Fatal(err)
		}
		payload := []byte("contained")
		if code, _ := runRemoteStageScript(t, root, "escape/file.yaml", stageTestDigest(payload), "0644", payload); code != 73 {
			t.Fatalf("symlink escape exit=%d, want 73", code)
		}
		if _, err := os.Lstat(filepath.Join(outside, "file.yaml")); !os.IsNotExist(err) {
			t.Fatalf("symlink escape wrote outside root: %v", err)
		}
	})

	t.Run("symlink root escape", func(t *testing.T) {
		outside := t.TempDir()
		link := filepath.Join(t.TempDir(), "catalog-link")
		if err := os.Symlink(outside, link); err != nil {
			t.Fatal(err)
		}
		payload := []byte("contained")
		if code, _ := runRemoteStageScript(t, link, "file.yaml", stageTestDigest(payload), "0644", payload); code != 73 {
			t.Fatalf("symlink root exit=%d, want 73", code)
		}
	})
}

func runRemoteStageScript(t *testing.T, root, relative, digest, mode string, stdin []byte) (int, string) {
	t.Helper()
	cmd := exec.CommandContext(context.Background(), "sh", "-c", remoteAtomicStageScript, "gc-capsule-stage-v1", root, relative, digest, mode)
	cmd.Stdin = bytes.NewReader(stdin)
	var output bytes.Buffer
	cmd.Stdout = &output
	cmd.Stderr = &output
	err := cmd.Run()
	if err == nil {
		return 0, output.String()
	}
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) {
		t.Fatalf("run stage script: %v", err)
	}
	return exitErr.ExitCode(), output.String()
}

func stageTestDigest(data []byte) string {
	digest := sha256.Sum256(data)
	return "sha256:" + hex.EncodeToString(digest[:])
}

func assertStageTestFile(t *testing.T, path string, want []byte, mode os.FileMode) {
	t.Helper()
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("staged bytes = %q, want %q", got, want)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != mode {
		t.Fatalf("staged mode = %o, want %o", info.Mode().Perm(), mode)
	}
}

func stageTestInfo(t *testing.T, path string) os.FileInfo {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	return info
}

func assertNoStageTemps(t *testing.T, directory string) {
	t.Helper()
	matches, err := filepath.Glob(filepath.Join(directory, "*.gc-stage.*"))
	if err != nil {
		t.Fatal(err)
	}
	if len(matches) != 0 {
		t.Fatalf("staging left temporary files: %v", matches)
	}
}
