package ssh

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/runtime"
)

func TestSSHCapsuleAttachStreamsExactTerminalBytesAndFencesStateGeneration(t *testing.T) {
	t.Parallel()
	input := []byte("first line\n第二行🙂\n\x03after-interrupt\n")
	f := &fakeRunner{respond: func(argv []string) ([]byte, int, error) {
		if isTmux("show-options")(argv) {
			return []byte("7:42\n"), 0, nil
		}
		return nil, 0, nil
	}}
	p := providerWith(f)
	p.attachStdin = bytes.NewReader(input)
	var output bytes.Buffer
	p.attachStdout = &output
	var gotArgs []string
	p.runAttachCommand = func(_ context.Context, args []string, stdin io.Reader, stdout, _ io.Writer) error {
		gotArgs = append([]string(nil), args...)
		_, err := io.Copy(stdout, stdin)
		return err
	}

	if err := p.Attach("ga-capsule"); err != nil {
		t.Fatalf("Attach: %v", err)
	}
	if !bytes.Equal(output.Bytes(), input) {
		t.Fatalf("terminal bytes = %q, want %q", output.Bytes(), input)
	}
	for _, required := range []string{"-t", "u@box", "ga-capsule", "7:42", capsuleStateTMUXOption} {
		if !slices.ContainsFunc(gotArgs, func(arg string) bool { return strings.Contains(arg, required) }) {
			t.Fatalf("attach argv missing %q: %q", required, gotArgs)
		}
	}
	remote := gotArgs[len(gotArgs)-1]
	if !strings.Contains(remote, `actual=$(tmux show-options -qv -t "$1" `+capsuleStateTMUXOption+`)`) ||
		!strings.Contains(remote, `[ "$actual" = "$2" ]`) || !strings.Contains(remote, `exec tmux attach -t "$1"`) {
		t.Fatalf("attach command lacks immutable state fence: %s", remote)
	}
	if firstCall(f, isTmux("kill-session")) != nil {
		t.Fatal("terminal detach mutated capsule lifecycle")
	}
}

func TestSSHCapsuleAttachConnectionLossThenReconnectDoesNotDuplicateInput(t *testing.T) {
	t.Parallel()
	f := &fakeRunner{respond: func(argv []string) ([]byte, int, error) {
		if isTmux("show-options")(argv) {
			return []byte("7:42\n"), 0, nil
		}
		return nil, 0, nil
	}}
	p := providerWith(f)
	wantErr := errors.New("ssh connection reset")
	var received []string
	p.runAttachCommand = func(_ context.Context, _ []string, stdin io.Reader, _ io.Writer, _ io.Writer) error {
		payload, err := io.ReadAll(stdin)
		if err != nil {
			return err
		}
		received = append(received, string(payload))
		if len(received) == 1 {
			return wantErr
		}
		return nil
	}

	p.attachStdin = strings.NewReader("first-only\n")
	if err := p.Attach("ga-capsule"); !errors.Is(err, wantErr) || !errors.Is(err, runtime.ErrRuntimeUnavailable) {
		t.Fatalf("connection-loss error = %v, want runtime unavailable", err)
	}
	p.attachStdin = strings.NewReader("second-only\n")
	if err := p.Attach("ga-capsule"); err != nil {
		t.Fatalf("reattach: %v", err)
	}
	if !slices.Equal(received, []string{"first-only\n", "second-only\n"}) {
		t.Fatalf("received input = %q; reconnect duplicated input", received)
	}
	if firstCall(f, isTmux("kill-session")) != nil {
		t.Fatal("connection loss stopped autonomous capsule")
	}
}

func TestSSHCapsuleAttachRejectsStaleStateGeneration(t *testing.T) {
	t.Parallel()
	f := &fakeRunner{respond: func(argv []string) ([]byte, int, error) {
		if isTmux("show-options")(argv) {
			return []byte("7:42\n"), 0, nil
		}
		return nil, 0, nil
	}}
	p := providerWith(f)
	p.runAttachCommand = func(context.Context, []string, io.Reader, io.Writer, io.Writer) error {
		return sshAttachExitError(75)
	}
	if err := p.Attach("ga-capsule"); !errors.Is(err, runtime.ErrCapsuleStateConflict) {
		t.Fatalf("stale attach error = %v, want capsule state conflict", err)
	}
	if firstCall(f, isTmux("kill-session")) != nil {
		t.Fatal("stale viewer attach stopped replacement capsule")
	}
}

func TestSSHAttachRejectsMissingSessionBeforeOpeningTTY(t *testing.T) {
	t.Parallel()
	f := &fakeRunner{respond: func(argv []string) ([]byte, int, error) {
		if isTmux("show-options")(argv) {
			return nil, 1, nil
		}
		return nil, 0, nil
	}}
	p := providerWith(f)
	called := false
	p.runAttachCommand = func(context.Context, []string, io.Reader, io.Writer, io.Writer) error {
		called = true
		return nil
	}
	if err := p.Attach("missing"); !errors.Is(err, runtime.ErrSessionNotFound) {
		t.Fatalf("Attach(missing) error = %v, want session not found", err)
	}
	if called {
		t.Fatal("missing session opened an interactive SSH process")
	}
}

type sshAttachExitError int

func (e sshAttachExitError) Error() string { return fmt.Sprintf("attach exited %d", e) }
func (e sshAttachExitError) ExitCode() int { return int(e) }
