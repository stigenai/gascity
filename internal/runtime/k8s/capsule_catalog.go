package k8s

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	"github.com/gastownhall/gascity/internal/runtime"
)

const (
	capsuleCatalogLabel              = "gascity.dev/capsule-catalog"
	capsuleCatalogDigestAnnotation   = "gascity.dev/capsule-catalog-sha256"
	capsuleCatalogSessionAnnotation  = "gascity.dev/capsule-session"
	capsuleCatalogResourceAnnotation = "gascity.dev/capsule-catalog-resource"
	capsuleCatalogMaxBytes           = 900 * 1024
)

func (p *Provider) ensureCapsuleCatalog(ctx context.Context, sessionName string, cfg runtime.Config) (*corev1.ConfigMap, bool, error) {
	if cfg.Capsule == nil {
		return nil, false, nil
	}
	if p.configMapOps == nil {
		return nil, false, errors.New("kubernetes ConfigMap operations are unavailable")
	}
	desired, err := buildCapsuleCatalogConfigMap(sessionName, cfg)
	if err != nil {
		return nil, false, err
	}
	existing, err := p.configMapOps.getConfigMap(ctx, desired.Name)
	if err == nil {
		if err := validateCapsuleCatalogConfigMap(existing, desired); err != nil {
			return nil, false, err
		}
		return existing, false, nil
	}
	if !apierrors.IsNotFound(err) {
		return nil, false, fmt.Errorf("get capsule catalog ConfigMap %q: %w", desired.Name, err)
	}

	created, err := p.configMapOps.createConfigMap(ctx, desired)
	if apierrors.IsAlreadyExists(err) {
		created, err = p.configMapOps.getConfigMap(ctx, desired.Name)
	}
	if err != nil {
		return nil, false, fmt.Errorf("create capsule catalog ConfigMap %q: %w", desired.Name, err)
	}
	if created == nil || created.UID == "" {
		return nil, false, fmt.Errorf("create capsule catalog ConfigMap %q returned no immutable UID", desired.Name)
	}
	if err := validateCapsuleCatalogConfigMap(created, desired); err != nil {
		return nil, false, err
	}
	return created, true, nil
}

func buildCapsuleCatalogConfigMap(sessionName string, cfg runtime.Config) (*corev1.ConfigMap, error) {
	if cfg.Capsule == nil {
		return nil, errors.New("capsule launch config is required")
	}
	if err := cfg.Capsule.Validate(); err != nil {
		return nil, err
	}
	binaryData := make(map[string][]byte, len(cfg.Capsule.CatalogInputs))
	totalBytes := 0
	for i, input := range cfg.Capsule.CatalogInputs {
		data, err := os.ReadFile(input.SourcePath)
		if err != nil {
			return nil, fmt.Errorf("read capsule catalog input %q: %w", input.RelativePath, err)
		}
		sum := sha256.Sum256(data)
		gotDigest := "sha256:" + hex.EncodeToString(sum[:])
		if gotDigest != input.SHA256 {
			return nil, fmt.Errorf("capsule catalog input %q changed after launch planning", input.RelativePath)
		}
		totalBytes += len(data)
		if totalBytes > capsuleCatalogMaxBytes {
			return nil, fmt.Errorf("capsule catalog inputs exceed %d-byte ConfigMap budget", capsuleCatalogMaxBytes)
		}
		binaryData[capsuleCatalogInputKey(i)] = data
	}
	immutable := true
	return &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name: cfg.Capsule.CatalogResourceID,
			Labels: map[string]string{
				capsuleCatalogLabel: "true",
				"gc-session":        SanitizeLabel(sessionName),
			},
			Annotations: map[string]string{
				capsuleCatalogDigestAnnotation:  cfg.Capsule.CatalogSHA256,
				capsuleCatalogSessionAnnotation: cfg.Capsule.Key.SessionID,
				capsuleCityScopeAnnotation:      capsuleCityScopeFingerprint(cfg.Capsule.Key.CityScope),
				"gc-capsule-digest":             cfg.Capsule.Key.Digest,
			},
		},
		Immutable:  &immutable,
		BinaryData: binaryData,
	}, nil
}

func validateCapsuleCatalogConfigMap(actual, desired *corev1.ConfigMap) error {
	conflict := func(format string, args ...any) error {
		return fmt.Errorf("%w: "+format, append([]any{runtime.ErrCapsuleStateConflict}, args...)...)
	}
	if actual == nil || desired == nil || actual.Name != desired.Name || actual.UID == "" {
		return conflict("capsule catalog ConfigMap identity is incomplete")
	}
	if actual.Immutable == nil || !*actual.Immutable {
		return conflict("capsule catalog ConfigMap %q is not immutable", actual.Name)
	}
	for key, value := range desired.Labels {
		if actual.Labels[key] != value {
			return conflict("capsule catalog ConfigMap %q label %q does not match", actual.Name, key)
		}
	}
	for key, value := range desired.Annotations {
		if actual.Annotations[key] != value {
			return conflict("capsule catalog ConfigMap %q annotation %q does not match", actual.Name, key)
		}
	}
	if len(actual.Data) != 0 || len(actual.BinaryData) != len(desired.BinaryData) {
		return conflict("capsule catalog ConfigMap %q data shape does not match", actual.Name)
	}
	for key, want := range desired.BinaryData {
		if !bytes.Equal(actual.BinaryData[key], want) {
			return conflict("capsule catalog ConfigMap %q input %q does not match", actual.Name, key)
		}
	}
	return nil
}

func (p *Provider) deleteCapsuleCatalog(ctx context.Context, name string, uid types.UID) error {
	if p.configMapOps == nil {
		return errors.New("kubernetes ConfigMap operations are unavailable")
	}
	err := p.configMapOps.deleteConfigMap(ctx, name, uid)
	if apierrors.IsNotFound(err) || apierrors.IsConflict(err) {
		return nil
	}
	return err
}

func (p *Provider) deleteCapsuleCatalogIfUnreferenced(ctx context.Context, sessionName string, catalog *corev1.ConfigMap) error {
	if catalog == nil {
		return nil
	}
	pods, err := p.ops.listPods(ctx, "gc-session="+SanitizeLabel(sessionName), "")
	if err != nil {
		return fmt.Errorf("listing pods before deleting capsule catalog %q: %w", catalog.Name, err)
	}
	for i := range pods {
		if pods[i].Annotations[capsuleCatalogResourceAnnotation] == catalog.Name {
			return nil
		}
	}
	return p.deleteCapsuleCatalog(ctx, catalog.Name, catalog.UID)
}

func (p *Provider) deleteCapsuleCatalogForPod(ctx context.Context, pod *corev1.Pod) error {
	if pod == nil || pod.Annotations == nil {
		return nil
	}
	name := pod.Annotations[capsuleCatalogResourceAnnotation]
	if name == "" {
		return nil
	}
	if p.configMapOps == nil {
		return errors.New("kubernetes ConfigMap operations are unavailable")
	}
	configMap, err := p.configMapOps.getConfigMap(ctx, name)
	if apierrors.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("get capsule catalog ConfigMap %q for cleanup: %w", name, err)
	}
	if configMap.Labels[capsuleCatalogLabel] != "true" ||
		configMap.Annotations[capsuleCityScopeAnnotation] != capsuleCityScopeFingerprint(p.capsuleCityScope) ||
		configMap.Annotations[capsuleCatalogDigestAnnotation] != pod.Annotations["gc-capsule-catalog-sha256"] {
		return fmt.Errorf("%w: capsule catalog ConfigMap %q ownership does not match pod", runtime.ErrCapsuleStateConflict, name)
	}
	return p.deleteCapsuleCatalog(ctx, name, configMap.UID)
}

func capsuleCatalogInputKey(index int) string {
	return fmt.Sprintf("input-%03d", index)
}
