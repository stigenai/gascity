package main

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/gastownhall/gascity/internal/api"
)

type fakeRemoteTerminalClient struct {
	mu         sync.Mutex
	snapshot   api.SessionTerminalSnapshot
	readErr    error
	input      []byte
	resizes    [][2]int
	detach     int
	inputCalls int
}

func (f *fakeRemoteTerminalClient) ReadSessionTerminal(context.Context, string, int, string) (api.SessionTerminalSnapshot, error) {
	return f.snapshot, f.readErr
}

func (f *fakeRemoteTerminalClient) SendSessionTerminalInput(_ context.Context, _ string, data []byte) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.inputCalls++
	f.input = append(f.input, data...)
	return nil
}

func (f *fakeRemoteTerminalClient) ResizeSessionTerminal(_ context.Context, _ string, rows, columns int) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.resizes = append(f.resizes, [2]int{rows, columns})
	return nil
}

func (f *fakeRemoteTerminalClient) DetachSessionTerminal(context.Context, string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.detach++
	return nil
}

func TestStreamRemoteSessionTerminalForwardsInputOnceAndDetaches(t *testing.T) {
	client := &fakeRemoteTerminalClient{snapshot: api.SessionTerminalSnapshot{Cursor: "cursor", Data: []byte("screen")}}
	var stdout bytes.Buffer
	err := streamRemoteSessionTerminal(
		context.Background(), client, "ga-session", strings.NewReader("hello"), &stdout, &bytes.Buffer{},
		false, nil,
	)
	if err != nil {
		t.Fatalf("streamRemoteSessionTerminal: %v", err)
	}
	if stdout.String() != "screen" {
		t.Fatalf("stdout = %q, want screen", stdout.String())
	}
	client.mu.Lock()
	defer client.mu.Unlock()
	if string(client.input) != "hello" || client.inputCalls != 1 {
		t.Fatalf("input = %q calls=%d, want one exact hello", client.input, client.inputCalls)
	}
	if client.detach != 1 {
		t.Fatalf("detach = %d, want 1", client.detach)
	}
}

func TestStreamRemoteSessionTerminalResizesAndDetachesAfterReadFailure(t *testing.T) {
	client := &fakeRemoteTerminalClient{readErr: errors.New("private endpoint detail")}
	err := streamRemoteSessionTerminal(
		context.Background(), client, "ga-session", strings.NewReader(""), &bytes.Buffer{}, &bytes.Buffer{},
		true, func() (int, int, error) { return 40, 120, nil },
	)
	if err == nil || !strings.Contains(err.Error(), "read remote terminal") {
		t.Fatalf("error = %v", err)
	}
	client.mu.Lock()
	defer client.mu.Unlock()
	if len(client.resizes) != 1 || client.resizes[0] != [2]int{40, 120} {
		t.Fatalf("resizes = %#v", client.resizes)
	}
	if client.detach != 1 {
		t.Fatalf("detach = %d, want 1", client.detach)
	}
}
