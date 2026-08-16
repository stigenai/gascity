package k8s

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/gastownhall/gascity/internal/runtime"
)

type capsuleMutationFaultOps struct {
	*fakeK8sOps
	pvcCreateResponseErr     error
	podCreateResponseErr     error
	networkCreateResponseErr error
	cancelAfterPodCreate     context.CancelFunc
}

func (o *capsuleMutationFaultOps) createPVC(ctx context.Context, pvc *corev1.PersistentVolumeClaim) (*corev1.PersistentVolumeClaim, error) {
	created, err := o.fakeK8sOps.createPVC(ctx, pvc)
	if err == nil && o.pvcCreateResponseErr != nil {
		return nil, o.pvcCreateResponseErr
	}
	return created, err
}

func (o *capsuleMutationFaultOps) createNetworkPolicy(ctx context.Context, policy *networkingv1.NetworkPolicy) (*networkingv1.NetworkPolicy, error) {
	created, err := o.fakeK8sOps.createNetworkPolicy(ctx, policy)
	if err == nil && o.networkCreateResponseErr != nil {
		return nil, o.networkCreateResponseErr
	}
	return created, err
}

func (o *capsuleMutationFaultOps) createPod(ctx context.Context, pod *corev1.Pod) (*corev1.Pod, error) {
	created, err := o.fakeK8sOps.createPod(ctx, pod)
	if err == nil && o.cancelAfterPodCreate != nil {
		o.cancelAfterPodCreate()
	}
	if err == nil && o.podCreateResponseErr != nil {
		return nil, o.podCreateResponseErr
	}
	return created, err
}

func TestEnsureCapsuleStateRecoversCommittedCreateWithLostResponse(t *testing.T) {
	t.Parallel()
	ops := &capsuleMutationFaultOps{
		fakeK8sOps:           newFakeK8sOps(),
		pvcCreateResponseErr: errors.New("apiserver response lost after PVC commit"),
	}
	provider := newProviderWithOps(ops)
	key, err := runtime.NewCapsuleKey(provider.capsuleCityScope, "ga-pvc-response-loss")
	if err != nil {
		t.Fatal(err)
	}

	ref, created, err := provider.EnsureCapsuleState(context.Background(), key)
	if err != nil {
		t.Fatalf("EnsureCapsuleState after committed create: %v", err)
	}
	if created {
		t.Fatal("response-loss recovery claimed exclusive creation")
	}
	if ref.ResourceID != key.ResourceStem() || ref.ResourceUID == "" || len(ops.pvcs) != 1 {
		t.Fatalf("recovered state = %#v, PVCs=%d", ref, len(ops.pvcs))
	}
}

func TestProviderCapsuleStartRecoversCommittedCreatesWithLostResponses(t *testing.T) {
	t.Parallel()
	ops := &capsuleMutationFaultOps{fakeK8sOps: newFakeK8sOps()}
	provider := newProviderWithOps(ops)
	provider.prebaked = true
	provider.postStartSettle = 0
	capsule := testK8sCapsuleLaunch(t)
	capsule.Network = runtime.CapsuleNetworkOffline
	ref, _, err := provider.EnsureCapsuleState(context.Background(), capsule.Key)
	if err != nil {
		t.Fatal(err)
	}
	capsule.State = ref
	ops.networkCreateResponseErr = errors.New("apiserver response lost after NetworkPolicy commit")
	ops.podCreateResponseErr = errors.New("apiserver response lost after Pod commit")
	ops.setExecResult("ga-response-loss", []string{"tmux", "has-session", "-t", tmuxSession}, "", nil)

	err = provider.Start(context.Background(), "ga-response-loss", runtime.Config{
		Capsule: capsule,
		Env:     map[string]string{"GC_INSTANCE_TOKEN": "instance-response-loss"},
	})
	if err != nil {
		t.Fatalf("Start after committed creates: %v", err)
	}
	if len(ops.pods) != 1 || len(ops.networkPolicies) != 1 || len(ops.pvcs) != 1 {
		t.Fatalf("resources after recovered Start: pods=%d policies=%d PVCs=%d", len(ops.pods), len(ops.networkPolicies), len(ops.pvcs))
	}
}

func TestProviderCapsuleReadinessFailureReportsCleanupFaultsAndRetainsState(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	ops := &capsuleMutationFaultOps{fakeK8sOps: newFakeK8sOps(), cancelAfterPodCreate: cancel}
	provider := newProviderWithOps(ops)
	provider.prebaked = true
	capsule := testK8sCapsuleLaunch(t)
	capsule.Network = runtime.CapsuleNetworkOffline
	ref, _, err := provider.EnsureCapsuleState(context.Background(), capsule.Key)
	if err != nil {
		t.Fatal(err)
	}
	capsule.State = ref
	ops.deleteErr = errors.New("pod cleanup transport failed")
	ops.networkDeleteErr = errors.New("policy cleanup transport failed")

	err = provider.Start(ctx, "ga-readiness-fault", runtime.Config{Capsule: capsule})
	if err == nil {
		t.Fatal("Start unexpectedly succeeded")
	}
	for _, want := range []string{"ga-readiness-fault", "pod cleanup transport failed", "policy cleanup transport failed"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("Start error %q does not contain %q", err, want)
		}
	}
	if _, ok := ops.pvcs[ref.ResourceID]; !ok {
		t.Fatal("readiness cleanup deleted retained capsule state")
	}
	if len(ops.pods) != 1 || len(ops.networkPolicies) != 1 {
		t.Fatalf("injected cleanup failures did not leave observable resources: pods=%d policies=%d", len(ops.pods), len(ops.networkPolicies))
	}
}

func TestProviderCapsuleReplacementFaultMatrixLeavesNoEphemeralResources(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name        string
		podPhase    corev1.PodPhase
		tmuxCrashed bool
	}{
		{name: "harness or in-pod tmux crash", podPhase: corev1.PodRunning, tmuxCrashed: true},
		{name: "pod eviction", podPhase: corev1.PodFailed},
		{name: "node loss before reschedule", podPhase: corev1.PodUnknown},
		{name: "host interruption during scheduling", podPhase: corev1.PodPending},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			ops := newFakeK8sOps()
			firstProvider := newProviderWithOps(ops)
			firstProvider.prebaked = true
			firstProvider.postStartSettle = 0
			capsule := testK8sCapsuleLaunch(t)
			capsule.Network = runtime.CapsuleNetworkOffline
			ref, _, err := firstProvider.EnsureCapsuleState(ctx, capsule.Key)
			if err != nil {
				t.Fatal(err)
			}
			capsule.State = ref
			name := "ga-fault-replacement"
			cfg := runtime.Config{Capsule: capsule, Env: map[string]string{"GC_INSTANCE_TOKEN": "fault-instance"}}
			ops.setExecResult(name, []string{"tmux", "has-session", "-t", tmuxSession}, "", nil)
			if err := firstProvider.Start(ctx, name, cfg); err != nil {
				t.Fatalf("initial Start: %v", err)
			}
			ops.pods[name].UID = "pre-fault-pod-uid"
			ops.pods[name].Status.Phase = tc.podPhase
			ops.pods[name].CreationTimestamp = metav1.NewTime(time.Now().Add(-10 * time.Minute))
			if tc.tmuxCrashed {
				ops.execFunc = func(_ string, cmd []string) (string, error) {
					if len(cmd) >= 2 && cmd[0] == "tmux" && cmd[1] == "has-session" && ops.pods[name].UID == "pre-fault-pod-uid" {
						return "", errors.New("tmux server exited")
					}
					return "", nil
				}
			}

			// A new provider instance models controller restart after the fault.
			restartedProvider := newProviderWithOps(ops)
			restartedProvider.prebaked = true
			restartedProvider.postStartSettle = 0
			if err := restartedProvider.Start(ctx, name, cfg); err != nil {
				t.Fatalf("replacement Start: %v", err)
			}
			if got := podCapsuleClaim(ops.pods[name]); got != ref.ResourceID {
				t.Fatalf("replacement claim = %q, want retained %q", got, ref.ResourceID)
			}
			if ops.pods[name].UID == "pre-fault-pod-uid" {
				t.Fatal("faulted pod was not replaced")
			}
			if len(ops.pods) != 1 || len(ops.networkPolicies) != 1 || len(ops.pvcs) != 1 {
				t.Fatalf("replacement resources: pods=%d policies=%d PVCs=%d", len(ops.pods), len(ops.networkPolicies), len(ops.pvcs))
			}
			if err := restartedProvider.Stop(name); err != nil {
				t.Fatalf("Stop: %v", err)
			}
			if len(ops.pods) != 0 || len(ops.networkPolicies) != 0 || len(ops.pvcs) != 1 {
				t.Fatalf("teardown resources: pods=%d policies=%d PVCs=%d", len(ops.pods), len(ops.networkPolicies), len(ops.pvcs))
			}
			if err := restartedProvider.PurgeCapsuleState(ctx, capsule.Key); err != nil {
				t.Fatalf("terminal purge: %v", err)
			}
			if len(ops.pods) != 0 || len(ops.networkPolicies) != 0 || len(ops.pvcs) != 0 {
				t.Fatalf("terminal resources: pods=%d policies=%d PVCs=%d", len(ops.pods), len(ops.networkPolicies), len(ops.pvcs))
			}
		})
	}
}

func TestProviderCapsuleVolumeOutageFailsBeforeEphemeralMutation(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	ops := newFakeK8sOps()
	provider := newProviderWithOps(ops)
	capsule := testK8sCapsuleLaunch(t)
	capsule.Network = runtime.CapsuleNetworkOffline
	ref, _, err := provider.EnsureCapsuleState(ctx, capsule.Key)
	if err != nil {
		t.Fatal(err)
	}
	capsule.State = ref
	ops.pvcGetErr = errors.New("persistent volume control plane unavailable")
	err = provider.Start(ctx, "ga-volume-outage", runtime.Config{Capsule: capsule})
	if err == nil || !strings.Contains(err.Error(), "ga-volume-outage") || !strings.Contains(err.Error(), "persistent volume control plane unavailable") {
		t.Fatalf("Start error = %v", err)
	}
	if len(ops.pods) != 0 || len(ops.networkPolicies) != 0 || len(ops.pvcs) != 1 {
		t.Fatalf("volume outage resources: pods=%d policies=%d PVCs=%d", len(ops.pods), len(ops.networkPolicies), len(ops.pvcs))
	}
}

func TestProviderCapsuleStartRefusesSameNamePodFromAnotherCity(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	ops := newFakeK8sOps()
	provider := newProviderWithOps(ops)
	capsule := testK8sCapsuleLaunch(t)
	capsule.Network = runtime.CapsuleNetworkOffline
	ref, _, err := provider.EnsureCapsuleState(ctx, capsule.Key)
	if err != nil {
		t.Fatal(err)
	}
	capsule.State = ref
	name := "ga-foreign-collision"
	foreignPod, err := buildPod(name, runtime.Config{Capsule: capsule}, provider)
	if err != nil {
		t.Fatal(err)
	}
	foreignPod.UID = "foreign-city-pod-uid"
	foreignPod.Annotations[capsuleCityScopeAnnotation] = capsuleCityScopeFingerprint("cluster/test-ns/another-city")
	foreignPod.Status.Phase = corev1.PodFailed
	ops.pods[foreignPod.Name] = foreignPod

	err = provider.Start(ctx, name, runtime.Config{Capsule: capsule})
	if !errors.Is(err, runtime.ErrCapsuleStateConflict) {
		t.Fatalf("Start error = %v, want capsule state conflict", err)
	}
	if got := ops.pods[foreignPod.Name]; got == nil || got.UID != foreignPod.UID {
		t.Fatalf("foreign pod was replaced: %#v", got)
	}
	for _, call := range ops.calls {
		if call.method == "createPod" || call.method == "deletePod" || call.method == "createNetworkPolicy" {
			t.Fatalf("foreign collision caused mutation: %#v", ops.calls)
		}
	}
}
