package runtime

import "testing"

func TestBoundTerminalReadKeepsNewestBytesAndCopies(t *testing.T) {
	source := []byte("012345")
	got := BoundTerminalRead(source, 3)
	if string(got.Data) != "345" || !got.Truncated {
		t.Fatalf("got = %#v", got)
	}
	source[5] = 'x'
	if string(got.Data) != "345" {
		t.Fatalf("bounded read aliases source: %q", got.Data)
	}
}

func TestBoundTerminalReadZeroLimitIsBounded(t *testing.T) {
	got := BoundTerminalRead([]byte("secret"), 0)
	if len(got.Data) != 0 || !got.Truncated {
		t.Fatalf("got = %#v", got)
	}
}
