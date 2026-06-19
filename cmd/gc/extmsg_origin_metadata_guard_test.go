package main

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestExtMsgOriginMetadataKeyIsCentralized(t *testing.T) {
	root := repoRootForExtMsgGuard(t)
	keysPath := filepath.Join(root, "internal", "beadmeta", "keys.go")
	data, err := os.ReadFile(keysPath)
	if err != nil {
		t.Fatalf("reading internal/beadmeta/keys.go: %v", err)
	}
	text := string(data)
	if !strings.Contains(text, "ExtMsgOriginMetadataKey") {
		t.Fatalf("internal/beadmeta/keys.go must declare ExtMsgOriginMetadataKey")
	}
	if !strings.Contains(text, `"gc.extmsg.origin"`) {
		t.Fatalf("internal/beadmeta/keys.go must assign ExtMsgOriginMetadataKey to %q", "gc.extmsg.origin")
	}
}

func TestCmdGCExtMsgOriginMetadataKeyHasNoStringLiterals(t *testing.T) {
	root := repoRootForExtMsgGuard(t)
	cmdDir := filepath.Join(root, "cmd", "gc")
	entries, err := os.ReadDir(cmdDir)
	if err != nil {
		t.Fatalf("reading cmd/gc: %v", err)
	}

	var hits []string
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		path := filepath.Join(cmdDir, name)
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("reading %s: %v", path, err)
		}
		if strings.Contains(string(data), `"gc.extmsg.origin"`) {
			hits = append(hits, filepath.ToSlash(filepath.Join("cmd", "gc", name)))
		}
	}

	if len(hits) > 0 {
		t.Fatalf("cmd/gc must use beadmeta.ExtMsgOriginMetadataKey instead of raw %q literals in: %s",
			"gc.extmsg.origin", strings.Join(hits, ", "))
	}
}

func repoRootForExtMsgGuard(t *testing.T) string {
	t.Helper()
	_, filename, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	return filepath.Clean(filepath.Join(filepath.Dir(filename), "..", ".."))
}
