package k8s

import (
	"errors"
	"fmt"
	"testing"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime/schema"
	execerr "k8s.io/client-go/util/exec"

	"github.com/gastownhall/gascity/internal/runtime"
)

type providerInteraction struct {
	name string
	run  func(runtime.Provider) error
}

func providerInteractions() []providerInteraction {
	return []providerInteraction{
		{
			name: "Nudge",
			run: func(p runtime.Provider) error {
				return p.Nudge("session", runtime.TextContent("keep going"))
			},
		},
		{
			name: "SendKeys",
			run: func(p runtime.Provider) error {
				return p.SendKeys("session", "Enter")
			},
		},
		{
			name: "SetMeta",
			run: func(p runtime.Provider) error {
				return p.SetMeta("session", "GC_DRAIN", "true")
			},
		},
		{
			name: "GetMeta",
			run: func(p runtime.Provider) error {
				_, err := p.GetMeta("session", "GC_DRAIN")
				return err
			},
		},
		{
			name: "RemoveMeta",
			run: func(p runtime.Provider) error {
				return p.RemoveMeta("session", "GC_DRAIN")
			},
		},
		{
			name: "Peek",
			run: func(p runtime.Provider) error {
				_, err := p.Peek("session", 20)
				return err
			},
		},
	}
}

func TestProviderInteractionsClassifyMissingSession(t *testing.T) {
	for _, op := range providerInteractions() {
		t.Run(op.name, func(t *testing.T) {
			p := newProviderWithOps(newFakeK8sOps())

			err := op.run(p)

			if !errors.Is(err, runtime.ErrSessionNotFound) {
				t.Fatalf("%s error = %v, want ErrSessionNotFound", op.name, err)
			}
			if errors.Is(err, runtime.ErrRuntimeUnavailable) {
				t.Fatalf("%s error = %v, must not report an observable missing session as unavailable", op.name, err)
			}
		})
	}
}

func TestProviderInteractionsClassifyPodListFailure(t *testing.T) {
	for _, op := range providerInteractions() {
		t.Run(op.name, func(t *testing.T) {
			cause := errors.New("apiserver request timed out")
			fake := newFakeK8sOps()
			fake.listErr = cause
			p := newProviderWithOps(fake)

			err := op.run(p)

			if !errors.Is(err, runtime.ErrRuntimeUnavailable) {
				t.Fatalf("%s error = %v, want ErrRuntimeUnavailable", op.name, err)
			}
			if !errors.Is(err, cause) {
				t.Fatalf("%s error = %v, want wrapped list cause", op.name, err)
			}
			if errors.Is(err, runtime.ErrSessionNotFound) {
				t.Fatalf("%s error = %v, must not turn an API failure into session absence", op.name, err)
			}
		})
	}
}

func TestProviderInteractionsClassifyExecTransportFailure(t *testing.T) {
	for _, op := range providerInteractions() {
		t.Run(op.name, func(t *testing.T) {
			cause := errors.New("SPDY stream closed unexpectedly")
			fake := newFakeK8sOps()
			addRunningPod(fake, "session", "session")
			fake.execFunc = func(string, []string) (string, error) {
				return "", cause
			}
			p := newProviderWithOps(fake)

			err := op.run(p)

			if !errors.Is(err, runtime.ErrRuntimeUnavailable) {
				t.Fatalf("%s error = %v, want ErrRuntimeUnavailable", op.name, err)
			}
			if !errors.Is(err, cause) {
				t.Fatalf("%s error = %v, want wrapped exec cause", op.name, err)
			}
		})
	}
}

func TestProviderInteractionsClassifyPodDisappearingDuringExec(t *testing.T) {
	for _, op := range providerInteractions() {
		t.Run(op.name, func(t *testing.T) {
			cause := apierrors.NewNotFound(
				schema.GroupResource{Resource: "pods"},
				"session",
			)
			fake := newFakeK8sOps()
			addRunningPod(fake, "session", "session")
			fake.execFunc = func(string, []string) (string, error) {
				return "", cause
			}
			p := newProviderWithOps(fake)

			err := op.run(p)

			if !errors.Is(err, runtime.ErrSessionNotFound) {
				t.Fatalf("%s error = %v, want ErrSessionNotFound", op.name, err)
			}
			if !errors.Is(err, cause) {
				t.Fatalf("%s error = %v, want wrapped Kubernetes NotFound cause", op.name, err)
			}
			if errors.Is(err, runtime.ErrRuntimeUnavailable) {
				t.Fatalf("%s error = %v, must not report a confirmed pod deletion as unavailable", op.name, err)
			}
		})
	}
}

func TestProviderInteractionsDoNotTreatTmuxExitAsSuccess(t *testing.T) {
	for _, op := range providerInteractions() {
		t.Run(op.name, func(t *testing.T) {
			fake := newFakeK8sOps()
			addRunningPod(fake, "session", "session")
			fake.execFunc = func(string, []string) (string, error) {
				return "", execerr.CodeExitError{
					Err:  fmt.Errorf("tmux target %q is unavailable", tmuxSession),
					Code: 1,
				}
			}
			p := newProviderWithOps(fake)

			err := op.run(p)

			if !errors.Is(err, runtime.ErrRuntimeUnavailable) {
				t.Fatalf("%s error = %v, want ErrRuntimeUnavailable for a failed tmux command", op.name, err)
			}
		})
	}
}

func TestGetMetaRejectsMalformedSuccessfulOutput(t *testing.T) {
	fake := newFakeK8sOps()
	addRunningPod(fake, "session", "session")
	fake.setExecResult(
		"session",
		[]string{"tmux", "show-environment", "-t", tmuxSession, "GC_DRAIN"},
		"unexpected output",
		nil,
	)
	p := newProviderWithOps(fake)

	_, err := p.GetMeta("session", "GC_DRAIN")

	if !errors.Is(err, runtime.ErrRuntimeUnavailable) {
		t.Fatalf("GetMeta error = %v, want ErrRuntimeUnavailable for an unreadable response", err)
	}
}

func TestSeamBackedProviderPreservesK8sInteractionErrors(t *testing.T) {
	for _, op := range providerInteractions() {
		t.Run(op.name, func(t *testing.T) {
			cause := errors.New("apiserver unavailable")
			fake := newFakeK8sOps()
			fake.listErr = cause
			p := newSeamBacked(newProviderWithOps(fake))

			err := op.run(p)

			if !errors.Is(err, runtime.ErrRuntimeUnavailable) {
				t.Fatalf("%s error through production seam wrapper = %v, want ErrRuntimeUnavailable", op.name, err)
			}
			if !errors.Is(err, cause) {
				t.Fatalf("%s error through production seam wrapper = %v, want wrapped API cause", op.name, err)
			}
		})
	}
}
