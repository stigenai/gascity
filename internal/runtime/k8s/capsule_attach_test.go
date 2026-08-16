package k8s

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

func TestCapsuleAttachStreamsExactTerminalBytesAndFencesPodUID(t *testing.T) {
	t.Parallel()
	ops := newFakeK8sOps()
	addRunningPodWithAnnotation(ops, "ga-capsule", "ga-capsule", "ga-capsule")
	provider := newProviderWithOps(ops)
	provider.k8sContext = "isolated-context"
	input := []byte("first line\n第二行🙂\n\x03after-interrupt\n")
	provider.attachStdin = bytes.NewReader(input)
	var output bytes.Buffer
	provider.attachStdout = &output
	var gotArgs []string
	provider.runAttachCommand = func(_ context.Context, args []string, stdin io.Reader, stdout, _ io.Writer) error {
		gotArgs = append([]string(nil), args...)
		_, err := io.Copy(stdout, stdin)
		return err
	}

	if err := provider.Attach("ga-capsule"); err != nil {
		t.Fatalf("Attach: %v", err)
	}
	if !bytes.Equal(output.Bytes(), input) {
		t.Fatalf("terminal bytes = %q, want %q", output.Bytes(), input)
	}
	for _, required := range []string{"--context", "isolated-context", "-n", "test-ns", "exec", "-it", "ga-capsule", "test-uid-ga-capsule", tmuxSession} {
		if !slices.Contains(gotArgs, required) {
			t.Fatalf("attach argv missing %q: %q", required, gotArgs)
		}
	}
	command := strings.Join(gotArgs, " ")
	if !strings.Contains(command, `test "$GC_POD_UID" = "$1"`) || !strings.Contains(command, "exit 75") || !strings.Contains(command, `exec tmux attach -t "$2"`) {
		t.Fatalf("attach command lacks immutable UID fence: %s", command)
	}
	assertAttachDidNotMutateLifecycle(t, ops)
	if _, ok := ops.pods["ga-capsule"]; !ok {
		t.Fatal("terminal detach stopped capsule pod")
	}
}

func TestCapsuleAttachConnectionLossThenReconnectDoesNotDuplicateInput(t *testing.T) {
	t.Parallel()
	ops := newFakeK8sOps()
	addRunningPodWithAnnotation(ops, "ga-capsule", "ga-capsule", "ga-capsule")
	provider := newProviderWithOps(ops)
	wantErr := errors.New("SPDY connection reset")
	var received []string
	provider.runAttachCommand = func(_ context.Context, _ []string, stdin io.Reader, _ io.Writer, _ io.Writer) error {
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

	provider.attachStdin = strings.NewReader("first-only\n")
	if err := provider.Attach("ga-capsule"); !errors.Is(err, wantErr) || !errors.Is(err, runtime.ErrRuntimeUnavailable) {
		t.Fatalf("connection-loss error = %v", err)
	}
	if _, ok := ops.pods["ga-capsule"]; !ok {
		t.Fatal("connection loss stopped autonomous capsule")
	}
	provider.attachStdin = strings.NewReader("second-only\n")
	if err := provider.Attach("ga-capsule"); err != nil {
		t.Fatalf("reattach: %v", err)
	}
	if !slices.Equal(received, []string{"first-only\n", "second-only\n"}) {
		t.Fatalf("received input = %q; reconnect duplicated input", received)
	}
	assertAttachDidNotMutateLifecycle(t, ops)
}

func TestCapsuleAttachRejectsStalePodIncarnationWithoutLifecycleMutation(t *testing.T) {
	t.Parallel()
	ops := newFakeK8sOps()
	addRunningPodWithAnnotation(ops, "ga-capsule", "ga-capsule", "ga-capsule")
	provider := newProviderWithOps(ops)
	provider.runAttachCommand = func(context.Context, []string, io.Reader, io.Writer, io.Writer) error {
		return attachExitError(75)
	}

	if err := provider.Attach("ga-capsule"); !errors.Is(err, runtime.ErrInstanceTokenMismatch) {
		t.Fatalf("stale attach error = %v, want instance mismatch", err)
	}
	if _, ok := ops.pods["ga-capsule"]; !ok {
		t.Fatal("stale attach deleted replacement pod")
	}
	assertAttachDidNotMutateLifecycle(t, ops)
}

func TestBuildPodProjectsImmutableUIDForAttachFence(t *testing.T) {
	t.Parallel()
	pod, err := buildPod("ga-capsule", runtime.Config{Capsule: testK8sCapsuleLaunch(t)}, newProviderWithOps(newFakeK8sOps()))
	if err != nil {
		t.Fatal(err)
	}
	env := envByName(pod.Spec.Containers[0].Env)["GC_POD_UID"]
	if env.Value != "" || env.ValueFrom == nil || env.ValueFrom.FieldRef == nil || env.ValueFrom.FieldRef.APIVersion != "v1" || env.ValueFrom.FieldRef.FieldPath != "metadata.uid" {
		t.Fatalf("GC_POD_UID = %#v, want downward-API metadata.uid", env)
	}
}

type attachExitError int

func (e attachExitError) Error() string { return fmt.Sprintf("attach exited %d", e) }
func (e attachExitError) ExitCode() int { return int(e) }

func assertAttachDidNotMutateLifecycle(t *testing.T, ops *fakeK8sOps) {
	t.Helper()
	for _, call := range ops.calls {
		switch call.method {
		case "createPod", "deletePod", "createPVC", "deletePVC", "createNetworkPolicy", "deleteNetworkPolicy":
			t.Fatalf("attach mutated lifecycle: %#v", ops.calls)
		}
	}
}
