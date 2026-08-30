package inputframe

import (
	"strings"
	"testing"
)

func TestControllerFrameRoundTripPreservesOneMultilineMessage(t *testing.T) {
	message := "first line\n\nsecond $(literal); 'quoted' \u4e16\u754c"
	encoded := Encode(message)
	if strings.Contains(encoded, "\n") || strings.Contains(encoded, message) {
		t.Fatalf("encoded frame exposed delimiter or plaintext: %q", encoded)
	}
	decoded, err := Decode(encoded)
	if err != nil {
		t.Fatal(err)
	}
	if decoded != message {
		t.Fatalf("decoded = %q, want %q", decoded, message)
	}
}

func TestControllerFrameRejectsUnframedAndMalformedInput(t *testing.T) {
	for _, input := range []string{"plain text", "gc-omnigent-input-v1:not*base64"} {
		if _, err := Decode(input); err == nil {
			t.Fatalf("Decode(%q) succeeded", input)
		}
	}
}

func TestEncodeForProviderIsExact(t *testing.T) {
	message := "hello\nworld"
	if got := EncodeForProvider(ControllerProvider, message); got == message {
		t.Fatal("controller provider was not framed")
	}
	for _, provider := range []string{"omnigent", "codex", "", "omnigent-city-extra"} {
		if got := EncodeForProvider(provider, message); got != message {
			t.Fatalf("provider %q changed message to %q", provider, got)
		}
	}
}

func TestMaxEncodedLenMatchesBoundary(t *testing.T) {
	for _, size := range []int{0, 1, 2, 3, 1024} {
		if got, want := len(Encode(strings.Repeat("x", size))), MaxEncodedLen(size); got != want {
			t.Fatalf("size %d: encoded length = %d, want %d", size, got, want)
		}
	}
}
