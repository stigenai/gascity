package k8s

import (
	"context"
	"errors"
	"fmt"
	"testing"

	execerr "k8s.io/client-go/util/exec"

	"github.com/gastownhall/gascity/internal/runtime"
)

// TestProviderExec covers the new ExecProvider adapter directly — in
// particular the exit-code extraction, which the driving-method tests (which
// discard Exec's error) do not exercise.
func TestProviderExec(t *testing.T) {
	t.Run("success returns stdout and code 0", func(t *testing.T) {
		fake := newFakeK8sOps()
		p := newProviderWithOps(fake)
		addRunningPod(fake, "s", "s")
		fake.setExecResult("s", []string{"echo", "hi"}, "hi\n", nil)

		out, code, err := p.Exec(context.Background(), "s", []string{"echo", "hi"})
		if err != nil {
			t.Fatalf("Exec: %v", err)
		}
		if code != 0 {
			t.Errorf("code = %d, want 0", code)
		}
		if string(out) != "hi\n" {
			t.Errorf("out = %q, want %q", out, "hi\n")
		}
	})

	t.Run("non-zero command exit returns the code, not an error", func(t *testing.T) {
		fake := newFakeK8sOps()
		p := newProviderWithOps(fake)
		addRunningPod(fake, "s", "s")
		// k8s remotecommand surfaces a non-zero in-pod exit as a util/exec.ExitError.
		fake.setExecResult("s", []string{"false"}, "", execerr.CodeExitError{Err: fmt.Errorf("command terminated with exit code 7"), Code: 7})

		_, code, err := p.Exec(context.Background(), "s", []string{"false"})
		if err != nil {
			t.Fatalf("a non-zero command exit must not be a transport error: %v", err)
		}
		if code != 7 {
			t.Errorf("code = %d, want 7 (extracted from the ExitError)", code)
		}
	})

	t.Run("transport failure returns code -1 and an error", func(t *testing.T) {
		fake := newFakeK8sOps()
		p := newProviderWithOps(fake)
		addRunningPod(fake, "s", "s")
		cause := errors.New("stream closed unexpectedly")
		fake.setExecResult("s", []string{"echo"}, "", cause)

		_, code, err := p.Exec(context.Background(), "s", []string{"echo"})
		if !errors.Is(err, runtime.ErrRuntimeUnavailable) {
			t.Fatalf("error = %v, want ErrRuntimeUnavailable", err)
		}
		if !errors.Is(err, cause) {
			t.Fatalf("error = %v, want wrapped transport cause", err)
		}
		if code != -1 {
			t.Errorf("code = %d, want -1 on transport failure", code)
		}
	})

	t.Run("no running pod returns code -1 and an error", func(t *testing.T) {
		fake := newFakeK8sOps()
		p := newProviderWithOps(fake)
		// no pod added

		_, code, err := p.Exec(context.Background(), "missing", []string{"echo"})
		if !errors.Is(err, runtime.ErrSessionNotFound) {
			t.Fatalf("error = %v, want ErrSessionNotFound", err)
		}
		if code != -1 {
			t.Errorf("code = %d, want -1 when the box is unreachable", code)
		}
	})

	t.Run("pod list failure is unavailable, not missing", func(t *testing.T) {
		cause := errors.New("apiserver unavailable")
		fake := newFakeK8sOps()
		fake.listErr = cause
		p := newProviderWithOps(fake)

		_, code, err := p.Exec(context.Background(), "unknown", []string{"echo"})
		if !errors.Is(err, runtime.ErrRuntimeUnavailable) {
			t.Fatalf("error = %v, want ErrRuntimeUnavailable", err)
		}
		if !errors.Is(err, cause) {
			t.Fatalf("error = %v, want wrapped list cause", err)
		}
		if errors.Is(err, runtime.ErrSessionNotFound) {
			t.Fatalf("error = %v, must not classify a failed observation as missing", err)
		}
		if code != -1 {
			t.Errorf("code = %d, want -1 when pod state cannot be observed", code)
		}
	})
}
