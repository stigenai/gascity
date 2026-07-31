package sessionlog

import (
	"os"
	"path/filepath"
	"testing"
)

func TestExtractPiTailUsageReturnsOneEntryPerResponse(t *testing.T) {
	path := filepath.Join(t.TempDir(), "pi.jsonl")
	lines := []string{
		`{"type":"session","id":"pi-session","cwd":"/work"}`,
		`{"type":"message","id":"user-1","message":{"role":"user","content":"hello"}}`,
		`{"type":"message","id":"assistant-1","message":{"role":"assistant","provider":"litellm","model":"ornith","usage":{"input":234,"output":72,"cacheRead":53365,"cacheWrite":4,"reasoning":25,"totalTokens":53700},"responseId":"chatcmpl-1"}}`,
		`{"type":"message","id":"assistant-1-copy","message":{"role":"assistant","provider":"litellm","model":"ornith","usage":{"input":235,"output":73,"cacheRead":53366,"cacheWrite":5,"reasoning":26,"totalTokens":53705},"responseId":"chatcmpl-1"}}`,
		`{"type":"message","id":"assistant-zero","message":{"role":"assistant","provider":"litellm","model":"ornith","usage":{"input":0,"output":0,"cacheRead":0,"cacheWrite":0,"reasoning":0},"responseId":"chatcmpl-zero"}}`,
		`not json`,
	}
	if err := os.WriteFile(path, []byte(joinTestLines(lines)), 0o600); err != nil {
		t.Fatal(err)
	}

	got, err := ExtractPiTailUsage(path)
	if err != nil {
		t.Fatalf("ExtractPiTailUsage: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("len(usage) = %d, want 1: %#v", len(got), got)
	}
	want := TailUsage{
		EntryUUID:           "assistant-1-copy",
		MessageID:           "chatcmpl-1",
		Model:               "ornith",
		InputTokens:         235,
		OutputTokens:        73,
		ReasoningTokens:     26,
		CacheReadTokens:     53366,
		CacheCreationTokens: 5,
	}
	if got[0] != want {
		t.Fatalf("usage = %#v, want %#v", got[0], want)
	}
}

func joinTestLines(lines []string) string {
	result := ""
	for _, line := range lines {
		result += line + "\n"
	}
	return result
}
