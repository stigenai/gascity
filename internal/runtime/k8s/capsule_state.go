package k8s

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"
	"strconv"
	"strings"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	"github.com/gastownhall/gascity/internal/runtime"
)

const (
	capsuleStateMountPath      = "/var/lib/gascity/omnigent"
	capsuleStateLabel          = "gc-capsule-state"
	capsuleTokenLabel          = "gc-capsule-token"
	capsuleDigestAnnotation    = "gascity.dev/capsule-digest"
	capsuleSessionAnnotation   = "gascity.dev/capsule-session"
	capsuleVersionAnnotation   = "gascity.dev/capsule-identity-version"
	capsuleCityScopeAnnotation = "gascity.dev/capsule-city-scope-sha256"
)

// EnsureCapsuleState creates or reopens one session-owned ReadWriteOnce PVC.
// Existing resources are adopted only after full identity validation.
func (p *Provider) EnsureCapsuleState(ctx context.Context, key runtime.CapsuleKey) (runtime.CapsuleStateReference, bool, error) {
	if err := key.Validate(); err != nil {
		return runtime.CapsuleStateReference{}, false, err
	}
	if err := p.validateCapsuleCityScope(key); err != nil {
		return runtime.CapsuleStateReference{}, false, err
	}
	name := key.ResourceStem()
	if pvc, err := p.pvcOps.getPVC(ctx, name); err == nil {
		ref, validateErr := p.capsuleStateReference(key, pvc)
		return ref, false, validateErr
	} else if !apierrors.IsNotFound(err) {
		return runtime.CapsuleStateReference{}, false, fmt.Errorf("get capsule PVC %q: %w", name, err)
	}

	quantity, err := resource.ParseQuantity(p.capsuleStorageRequest)
	if err != nil {
		return runtime.CapsuleStateReference{}, false, fmt.Errorf("parse GC_K8S_CAPSULE_STORAGE_REQUEST %q: %w", p.capsuleStorageRequest, err)
	}
	pvc := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name: name, Namespace: p.namespace,
			Labels:      map[string]string{capsuleStateLabel: "true", capsuleTokenLabel: key.Token},
			Annotations: capsuleStateAnnotations(key),
		},
		Spec: corev1.PersistentVolumeClaimSpec{
			AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
			Resources:   corev1.VolumeResourceRequirements{Requests: corev1.ResourceList{corev1.ResourceStorage: quantity}},
		},
	}
	if p.capsuleStorageClassName != "" {
		storageClass := p.capsuleStorageClassName
		pvc.Spec.StorageClassName = &storageClass
	}
	created, err := p.pvcOps.createPVC(ctx, pvc)
	if err == nil {
		ref, validateErr := p.capsuleStateReference(key, created)
		return ref, true, validateErr
	}
	if !apierrors.IsAlreadyExists(err) {
		return runtime.CapsuleStateReference{}, false, fmt.Errorf("create capsule PVC %q: %w", name, err)
	}
	current, getErr := p.pvcOps.getPVC(ctx, name)
	if getErr != nil {
		return runtime.CapsuleStateReference{}, false, fmt.Errorf("reopen concurrently created capsule PVC %q: %w", name, getErr)
	}
	ref, validateErr := p.capsuleStateReference(key, current)
	return ref, false, validateErr
}

// OpenCapsuleState returns an existing, ownership-verified PVC without creating
// or modifying it.
func (p *Provider) OpenCapsuleState(ctx context.Context, key runtime.CapsuleKey) (runtime.CapsuleStateReference, bool, error) {
	if err := key.Validate(); err != nil {
		return runtime.CapsuleStateReference{}, false, err
	}
	if err := p.validateCapsuleCityScope(key); err != nil {
		return runtime.CapsuleStateReference{}, false, err
	}
	pvc, err := p.pvcOps.getPVC(ctx, key.ResourceStem())
	if apierrors.IsNotFound(err) {
		return runtime.CapsuleStateReference{}, false, nil
	}
	if err != nil {
		return runtime.CapsuleStateReference{}, false, fmt.Errorf("get capsule PVC %q: %w", key.ResourceStem(), err)
	}
	ref, err := p.capsuleStateReference(key, pvc)
	return ref, err == nil, err
}

// ListCapsuleStates inventories only explicitly Gas City-owned capsule PVCs.
// A malformed owned resource makes the inventory ambiguous and fails closed.
func (p *Provider) ListCapsuleStates(ctx context.Context) ([]runtime.CapsuleStateReference, error) {
	if p.pvcOps == nil {
		return nil, fmt.Errorf("%w: Kubernetes PVC operations are unavailable", runtime.ErrCapsuleStateConflict)
	}
	pvcs, err := p.pvcOps.listPVCs(ctx, capsuleStateLabel+"=true")
	if err != nil {
		return nil, fmt.Errorf("list capsule PVCs: %w", err)
	}
	sort.Slice(pvcs, func(i, j int) bool { return pvcs[i].Name < pvcs[j].Name })
	refs := make([]runtime.CapsuleStateReference, 0, len(pvcs))
	for i := range pvcs {
		key, err := p.capsuleKeyFromPVC(&pvcs[i])
		if err != nil {
			return nil, err
		}
		ref, err := p.capsuleStateReference(key, &pvcs[i])
		if err != nil {
			return nil, err
		}
		refs = append(refs, ref)
	}
	return refs, nil
}

// PurgeCapsuleState deletes one exact unattached PVC using a UID precondition.
// Missing state is idempotent; live or malformed ownership fails closed.
func (p *Provider) PurgeCapsuleState(ctx context.Context, key runtime.CapsuleKey) error {
	ref, ok, err := p.OpenCapsuleState(ctx, key)
	if err != nil || !ok {
		return err
	}
	attached, err := p.podsUsingCapsuleClaim(ctx, ref.ResourceID)
	if err != nil {
		return err
	}
	if len(attached) != 0 {
		return fmt.Errorf("%w: capsule PVC %q is attached", runtime.ErrCapsuleStateConflict, ref.ResourceID)
	}
	err = p.pvcOps.deletePVC(ctx, ref.ResourceID, typesUID(ref.ResourceUID))
	if apierrors.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("delete capsule PVC %q: %w", ref.ResourceID, err)
	}
	return nil
}

// AttachCapsuleState verifies an exact PVC and refuses cross-Place sharing.
// Kubernetes performs the physical attach when the validated pod is created.
func (p *Provider) AttachCapsuleState(ctx context.Context, placeName string, ref runtime.CapsuleStateReference) error {
	opened, ok, err := p.OpenCapsuleState(ctx, ref.Key)
	if err != nil {
		return err
	}
	if !ok || opened != ref {
		return fmt.Errorf("%w: capsule PVC reference is missing or stale", runtime.ErrCapsuleStateConflict)
	}
	attached, err := p.podsUsingCapsuleClaim(ctx, ref.ResourceID)
	if err != nil {
		return err
	}
	wantPod := SanitizeName(placeName)
	for _, pod := range attached {
		if pod != wantPod {
			return fmt.Errorf("%w: capsule PVC is attached to another Place", runtime.ErrCapsuleStateConflict)
		}
	}
	return nil
}

// DetachCapsuleState verifies Kubernetes has released the Place's volume. Pod
// teardown owns physical detachment, so a still-present pod is a conflict.
func (p *Provider) DetachCapsuleState(ctx context.Context, placeName string) error {
	wantPod := SanitizeName(placeName)
	pods, err := p.ops.listPods(ctx, "", "")
	if err != nil {
		return fmt.Errorf("list pods before capsule detach: %w", err)
	}
	for i := range pods {
		if pods[i].Name == wantPod && podCapsuleClaim(&pods[i]) != "" {
			return fmt.Errorf("%w: capsule Place must be torn down before detach completes", runtime.ErrCapsuleStateConflict)
		}
	}
	return nil
}

func (p *Provider) validateCapsuleStateForStart(ctx context.Context, cfg runtime.Config) error {
	if cfg.Capsule == nil {
		return nil
	}
	if err := cfg.Capsule.Validate(); err != nil {
		return err
	}
	opened, ok, err := p.OpenCapsuleState(ctx, cfg.Capsule.Key)
	if err != nil {
		return err
	}
	if !ok || opened != cfg.Capsule.State {
		return fmt.Errorf("%w: capsule PVC is missing or does not match launch plan", runtime.ErrCapsuleStateConflict)
	}
	return nil
}

func (p *Provider) capsuleStateReference(key runtime.CapsuleKey, pvc *corev1.PersistentVolumeClaim) (runtime.CapsuleStateReference, error) {
	if pvc == nil || pvc.Name != key.ResourceStem() || pvc.UID == "" {
		return runtime.CapsuleStateReference{}, fmt.Errorf("%w: capsule PVC identity is incomplete", runtime.ErrCapsuleStateConflict)
	}
	wantAnnotations := capsuleStateAnnotations(key)
	if pvc.Labels[capsuleStateLabel] != "true" || pvc.Labels[capsuleTokenLabel] != key.Token {
		return runtime.CapsuleStateReference{}, fmt.Errorf("%w: capsule PVC labels do not match identity", runtime.ErrCapsuleStateConflict)
	}
	for name, want := range wantAnnotations {
		if pvc.Annotations[name] != want {
			return runtime.CapsuleStateReference{}, fmt.Errorf("%w: capsule PVC annotation %q does not match identity", runtime.ErrCapsuleStateConflict, name)
		}
	}
	if len(pvc.Spec.AccessModes) != 1 || pvc.Spec.AccessModes[0] != corev1.ReadWriteOnce {
		return runtime.CapsuleStateReference{}, fmt.Errorf("%w: capsule PVC must use ReadWriteOnce", runtime.ErrCapsuleStateConflict)
	}
	if p.capsuleStorageClassName != "" && (pvc.Spec.StorageClassName == nil || *pvc.Spec.StorageClassName != p.capsuleStorageClassName) {
		return runtime.CapsuleStateReference{}, fmt.Errorf("%w: capsule PVC storage class does not match provider", runtime.ErrCapsuleStateConflict)
	}
	return runtime.CapsuleStateReference{
		Key: key, Provider: string(runtime.SecretProviderKubernetes), ResourceID: pvc.Name,
		ResourceUID: string(pvc.UID), MountPath: capsuleStateMountPath,
	}, nil
}

func capsuleStateAnnotations(key runtime.CapsuleKey) map[string]string {
	scope := sha256.Sum256([]byte(key.CityScope))
	return map[string]string{
		capsuleDigestAnnotation:    key.Digest,
		capsuleSessionAnnotation:   key.SessionID,
		capsuleVersionAnnotation:   strconv.Itoa(key.Version),
		capsuleCityScopeAnnotation: hex.EncodeToString(scope[:]),
	}
}

func (p *Provider) capsuleKeyFromPVC(pvc *corev1.PersistentVolumeClaim) (runtime.CapsuleKey, error) {
	if strings.TrimSpace(p.capsuleCityScope) == "" {
		return runtime.CapsuleKey{}, fmt.Errorf("%w: GC_K8S_CAPSULE_CITY_SCOPE is required", runtime.ErrCapsuleStateConflict)
	}
	version, err := strconv.Atoi(pvc.Annotations[capsuleVersionAnnotation])
	if err != nil {
		return runtime.CapsuleKey{}, fmt.Errorf("%w: capsule PVC has invalid identity version", runtime.ErrCapsuleStateConflict)
	}
	key, err := runtime.NewCapsuleKey(p.capsuleCityScope, pvc.Annotations[capsuleSessionAnnotation])
	if err != nil {
		return runtime.CapsuleKey{}, fmt.Errorf("%w: capsule PVC has invalid session identity: %w", runtime.ErrCapsuleStateConflict, err)
	}
	if version != key.Version || pvc.Labels[capsuleTokenLabel] != key.Token || pvc.Annotations[capsuleDigestAnnotation] != key.Digest {
		return runtime.CapsuleKey{}, fmt.Errorf("%w: capsule PVC derived identity does not match metadata", runtime.ErrCapsuleStateConflict)
	}
	return key, nil
}

func (p *Provider) validateCapsuleCityScope(key runtime.CapsuleKey) error {
	if p.pvcOps == nil {
		return fmt.Errorf("%w: Kubernetes PVC operations are unavailable", runtime.ErrCapsuleStateConflict)
	}
	if strings.TrimSpace(p.capsuleCityScope) == "" {
		return fmt.Errorf("%w: GC_K8S_CAPSULE_CITY_SCOPE is required", runtime.ErrCapsuleStateConflict)
	}
	if key.CityScope != p.capsuleCityScope {
		return fmt.Errorf("%w: capsule key city scope does not match provider", runtime.ErrCapsuleStateConflict)
	}
	return nil
}

func (p *Provider) podsUsingCapsuleClaim(ctx context.Context, claim string) ([]string, error) {
	pods, err := p.ops.listPods(ctx, "", "")
	if err != nil {
		return nil, fmt.Errorf("list pods using capsule PVC: %w", err)
	}
	var names []string
	for i := range pods {
		if podCapsuleClaim(&pods[i]) == claim {
			names = append(names, pods[i].Name)
		}
	}
	sort.Strings(names)
	return names, nil
}

func podCapsuleClaim(pod *corev1.Pod) string {
	if pod == nil {
		return ""
	}
	for _, volume := range pod.Spec.Volumes {
		if volume.Name == "capsule-state" && volume.PersistentVolumeClaim != nil {
			return volume.PersistentVolumeClaim.ClaimName
		}
	}
	return ""
}

// typesUID isolates the Kubernetes UID conversion at the API edge.
func typesUID(value string) types.UID { return types.UID(value) }
