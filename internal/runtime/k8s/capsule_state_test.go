package k8s

import (
	"context"
	"errors"
	"sync"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	"github.com/gastownhall/gascity/internal/runtime"
)

func TestProviderCapsuleStateLifecycleRetainsAcrossPodReplacementAndPurgesExplicitly(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	ops := newFakeK8sOps()
	provider := newProviderWithOps(ops)
	key, err := runtime.NewCapsuleKey("cluster/test-ns/city", "ga-session")
	if err != nil {
		t.Fatal(err)
	}

	ref, created, err := provider.EnsureCapsuleState(ctx, key)
	if err != nil || !created {
		t.Fatalf("EnsureCapsuleState = %#v, %t, %v", ref, created, err)
	}
	if ref.Provider != "k8s" || ref.ResourceID != key.ResourceStem() || ref.ResourceUID == "" || ref.MountPath != capsuleStateMountPath {
		t.Fatalf("state reference = %#v", ref)
	}
	pvc := ops.pvcs[ref.ResourceID]
	if pvc == nil || pvc.Labels[capsuleStateLabel] != "true" || pvc.Annotations[capsuleDigestAnnotation] != key.Digest || pvc.Annotations[capsuleSessionAnnotation] != key.SessionID {
		t.Fatalf("PVC ownership metadata = %#v", pvc)
	}
	if len(pvc.Spec.AccessModes) != 1 || pvc.Spec.AccessModes[0] != corev1.ReadWriteOnce {
		t.Fatalf("PVC access modes = %v, want ReadWriteOnce", pvc.Spec.AccessModes)
	}

	reopened, created, err := provider.EnsureCapsuleState(ctx, key)
	if err != nil || created || reopened != ref {
		t.Fatalf("reopen = %#v, %t, %v; want existing %#v", reopened, created, err, ref)
	}
	if got, ok, err := provider.OpenCapsuleState(ctx, key); err != nil || !ok || got != ref {
		t.Fatalf("OpenCapsuleState = %#v, %t, %v", got, ok, err)
	}

	if err := provider.AttachCapsuleState(ctx, "replacement", ref); err != nil {
		t.Fatalf("AttachCapsuleState before pod create: %v", err)
	}
	ops.pods["replacement"] = capsulePodUsingClaim("replacement", "pod-uid", ref)
	if err := provider.PurgeCapsuleState(ctx, key); !errors.Is(err, runtime.ErrCapsuleStateConflict) {
		t.Fatalf("purge attached state error = %v, want conflict", err)
	}
	if err := provider.DetachCapsuleState(ctx, "replacement"); !errors.Is(err, runtime.ErrCapsuleStateConflict) {
		t.Fatalf("detach live pod error = %v, want teardown-first conflict", err)
	}
	delete(ops.pods, "replacement")
	if err := provider.DetachCapsuleState(ctx, "replacement"); err != nil {
		t.Fatalf("DetachCapsuleState after replacement teardown: %v", err)
	}
	if _, ok, err := provider.OpenCapsuleState(ctx, key); err != nil || !ok {
		t.Fatalf("state was not retained across pod replacement: ok=%t err=%v", ok, err)
	}
	if err := provider.PurgeCapsuleState(ctx, key); err != nil {
		t.Fatalf("PurgeCapsuleState: %v", err)
	}
	if _, ok, err := provider.OpenCapsuleState(ctx, key); err != nil || ok {
		t.Fatalf("OpenCapsuleState after purge = ok=%t err=%v", ok, err)
	}
	if err := provider.PurgeCapsuleState(ctx, key); err != nil {
		t.Fatalf("repeated purge: %v", err)
	}
}

func TestProviderCapsuleStateConcurrentEnsureConvergesAndInventoryRejectsConflict(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	ops := newFakeK8sOps()
	provider := newProviderWithOps(ops)
	key, _ := runtime.NewCapsuleKey("cluster/test-ns/city", "ga-concurrent")

	const contenders = 12
	refs := make(chan runtime.CapsuleStateReference, contenders)
	errs := make(chan error, contenders)
	var wg sync.WaitGroup
	for range contenders {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ref, _, err := provider.EnsureCapsuleState(ctx, key)
			refs <- ref
			errs <- err
		}()
	}
	wg.Wait()
	close(refs)
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("concurrent EnsureCapsuleState: %v", err)
		}
	}
	var first runtime.CapsuleStateReference
	for ref := range refs {
		if first.ResourceID == "" {
			first = ref
		} else if ref != first {
			t.Fatalf("concurrent references differ: %#v vs %#v", first, ref)
		}
	}
	if len(ops.pvcs) != 1 {
		t.Fatalf("PVC count = %d, want one", len(ops.pvcs))
	}

	foreign := &corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{
		Name: "foreign", UID: types.UID("foreign-uid"), Labels: map[string]string{"unrelated": "true"},
	}}
	ops.pvcs[foreign.Name] = foreign
	listed, err := provider.ListCapsuleStates(ctx)
	if err != nil || len(listed) != 1 || listed[0] != first {
		t.Fatalf("ListCapsuleStates = %#v, %v", listed, err)
	}

	ownedConflict := ops.pvcs[first.ResourceID].DeepCopy()
	ownedConflict.Annotations[capsuleDigestAnnotation] = "wrong"
	ops.pvcs[first.ResourceID] = ownedConflict
	if _, err := provider.ListCapsuleStates(ctx); !errors.Is(err, runtime.ErrCapsuleStateConflict) {
		t.Fatalf("conflicting inventory error = %v, want conflict", err)
	}
	if err := provider.PurgeCapsuleState(ctx, key); !errors.Is(err, runtime.ErrCapsuleStateConflict) {
		t.Fatalf("conflicting purge error = %v, want conflict", err)
	}
	if _, ok := ops.pvcs[foreign.Name]; !ok {
		t.Fatal("foreign PVC was deleted")
	}
}

func TestProviderCapsuleStartValidationFailsBeforePodMutation(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	ops := newFakeK8sOps()
	provider := newProviderWithOps(ops)
	capsule := testK8sCapsuleLaunch(t)
	capsule.Network = runtime.CapsuleNetworkOffline

	cfg := runtime.Config{Capsule: capsule}
	if err := provider.Start(ctx, "ga-target", cfg); !errors.Is(err, runtime.ErrCapsuleStateConflict) {
		t.Fatalf("missing state error = %v, want conflict", err)
	}
	assertNoPodMutation(t, ops)

	provider.capsuleCityScope = "cluster/other/city"
	if err := provider.Start(ctx, "ga-target", cfg); !errors.Is(err, runtime.ErrCapsuleStateConflict) {
		t.Fatalf("scope mismatch error = %v, want conflict", err)
	}
	assertNoPodMutation(t, ops)
	provider.capsuleCityScope = capsule.Key.CityScope

	ref, _, err := provider.EnsureCapsuleState(ctx, capsule.Key)
	if err != nil {
		t.Fatal(err)
	}
	capsule.State = ref
	if err := provider.validateCapsuleStateForStart(ctx, runtime.Config{Capsule: capsule}); err != nil {
		t.Fatalf("valid state rejected: %v", err)
	}
	stale := ref
	stale.ResourceUID = "stale-uid"
	capsule.State = stale
	if err := provider.Start(ctx, "ga-target", cfg); !errors.Is(err, runtime.ErrCapsuleStateConflict) {
		t.Fatalf("stale UID error = %v, want conflict", err)
	}
	assertNoPodMutation(t, ops)

	capsule.State = ref
	ops.pods["other-place"] = capsulePodUsingClaim("other-place", "other-pod-uid", ref)
	if err := provider.Start(ctx, "ga-target", cfg); !errors.Is(err, runtime.ErrCapsuleStateConflict) {
		t.Fatalf("cross-Place attach error = %v, want conflict", err)
	}
	assertNoPodMutation(t, ops)
}

func TestProviderCapsuleStartMountsSameStateAcrossPodReplacement(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	ops := newFakeK8sOps()
	provider := newProviderWithOps(ops)
	provider.prebaked = true
	provider.postStartSettle = 0
	capsule := testK8sCapsuleLaunch(t)
	capsule.Network = runtime.CapsuleNetworkOffline
	ref, _, err := provider.EnsureCapsuleState(ctx, capsule.Key)
	if err != nil {
		t.Fatal(err)
	}
	capsule.State = ref
	name := "ga-replacement"
	ops.setExecResult(name, []string{"tmux", "has-session", "-t", tmuxSession}, "", nil)
	cfg := runtime.Config{Capsule: capsule, Env: map[string]string{}}

	if err := provider.Start(ctx, name, cfg); err != nil {
		t.Fatalf("first Start: %v", err)
	}
	firstPod := ops.pods[name]
	if got := podCapsuleClaim(firstPod); got != ref.ResourceID {
		t.Fatalf("first pod capsule claim = %q, want %q", got, ref.ResourceID)
	}
	delete(ops.pods, name)
	if err := provider.Start(ctx, name, cfg); err != nil {
		t.Fatalf("replacement Start: %v", err)
	}
	replacementPod := ops.pods[name]
	if got := podCapsuleClaim(replacementPod); got != ref.ResourceID {
		t.Fatalf("replacement pod capsule claim = %q, want %q", got, ref.ResourceID)
	}
	if _, ok := ops.pvcs[ref.ResourceID]; !ok {
		t.Fatal("replacement deleted durable capsule state")
	}
}

func assertNoPodMutation(t *testing.T, ops *fakeK8sOps) {
	t.Helper()
	for _, call := range ops.calls {
		if call.method == "createPod" || call.method == "deletePod" {
			t.Fatalf("capsule validation mutated pods: %#v", ops.calls)
		}
	}
}

func capsulePodUsingClaim(name, uid string, ref runtime.CapsuleStateReference) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: name, UID: types.UID(uid)},
		Spec: corev1.PodSpec{Volumes: []corev1.Volume{{
			Name: "capsule-state", VolumeSource: corev1.VolumeSource{PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: ref.ResourceID}},
		}}},
		Status: corev1.PodStatus{Phase: corev1.PodRunning},
	}
}
