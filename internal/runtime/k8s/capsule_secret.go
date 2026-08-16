package k8s

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"path/filepath"
	"sort"
	"strings"

	corev1 "k8s.io/api/core/v1"

	"github.com/gastownhall/gascity/internal/execenv"
	"github.com/gastownhall/gascity/internal/runtime"
)

const agentUserGroupID int64 = 1001

type k8sSecretProjection struct {
	environment  []corev1.EnvVar
	mounts       []corev1.VolumeMount
	volumes      []corev1.Volume
	hasFileMount bool
}

func projectK8sSecretReferences(cfg runtime.Config) (k8sSecretProjection, error) {
	if len(cfg.SecretReferences) == 0 {
		return k8sSecretProjection{}, rejectCapsuleCredentialLiterals(cfg)
	}
	refs, err := runtime.SelectSecretReferences(runtime.SecretProviderKubernetes, cfg.SecretReferences)
	if err != nil {
		return k8sSecretProjection{}, err
	}
	if err := rejectCapsuleCredentialLiterals(cfg); err != nil {
		return k8sSecretProjection{}, err
	}

	projection := k8sSecretProjection{}
	mountPaths := make([]string, 0, len(refs))
	for _, ref := range refs {
		source := ref.Kubernetes
		optional := source.Optional
		if ref.Environment != "" {
			if _, exists := cfg.Env[ref.Environment]; exists {
				return k8sSecretProjection{}, fmt.Errorf("secret reference %q: environment %s must not also have a literal Pod value", ref.ID, ref.Environment)
			}
			projection.environment = append(projection.environment, corev1.EnvVar{
				Name: ref.Environment,
				ValueFrom: &corev1.EnvVarSource{SecretKeyRef: &corev1.SecretKeySelector{
					LocalObjectReference: corev1.LocalObjectReference{Name: source.Name},
					Key:                  source.Key, Optional: boolPtr(optional),
				}},
			})
			continue
		}
		if source.Key == "." || source.Key == ".." {
			return k8sSecretProjection{}, fmt.Errorf("secret reference %q: Kubernetes key is not a safe projected filename", ref.ID)
		}
		if err := validateCapsuleCredentialMount(cfg, ref.ID, ref.MountPath, mountPaths); err != nil {
			return k8sSecretProjection{}, err
		}
		mountPaths = append(mountPaths, ref.MountPath)
		name := capsuleSecretVolumeName(ref.ID)
		mode := int32(0o440)
		projection.mounts = append(projection.mounts, corev1.VolumeMount{
			Name: name, MountPath: ref.MountPath, ReadOnly: true,
		})
		projection.volumes = append(projection.volumes, corev1.Volume{
			Name: name,
			VolumeSource: corev1.VolumeSource{Secret: &corev1.SecretVolumeSource{
				SecretName: source.Name, Optional: boolPtr(optional), DefaultMode: &mode,
				Items: []corev1.KeyToPath{{Key: source.Key, Path: source.Key, Mode: &mode}},
			}},
		})
		projection.hasFileMount = true
	}
	sort.Slice(projection.environment, func(i, j int) bool { return projection.environment[i].Name < projection.environment[j].Name })
	sort.Slice(projection.mounts, func(i, j int) bool { return projection.mounts[i].Name < projection.mounts[j].Name })
	sort.Slice(projection.volumes, func(i, j int) bool { return projection.volumes[i].Name < projection.volumes[j].Name })
	return projection, nil
}

func rejectCapsuleCredentialLiterals(cfg runtime.Config) error {
	if cfg.Capsule == nil {
		return nil
	}
	allowedNativeProjection := map[string]bool{
		"GITHUB_TOKEN": true, "LITELLM_MASTER_KEY": true,
		"GC_DOLT_PASSWORD": true, "BEADS_DOLT_PASSWORD": true,
		"GC_INSTANCE_TOKEN": true,
	}
	for key, value := range cfg.Env {
		if strings.TrimSpace(value) != "" && execenv.IsSensitiveKey(key) && !allowedNativeProjection[key] && !strings.HasPrefix(key, "GC_K8S_") {
			return fmt.Errorf("capsule credential environment %s requires a typed Kubernetes secret reference", key)
		}
	}
	return nil
}

func validateCapsuleCredentialMount(cfg runtime.Config, id, mountPath string, existing []string) error {
	for _, other := range existing {
		if pathsOverlap(mountPath, other) {
			return fmt.Errorf("secret reference %q: credential mounts overlap", id)
		}
	}
	reserved := []string{projectedPodWorkDir(cfg)}
	if cfg.Capsule != nil {
		if !pathStrictlyWithin(cfg.Capsule.RunRoot, mountPath) {
			return fmt.Errorf("secret reference %q: credential mount must be beneath capsule run root", id)
		}
		reserved = append(reserved, cfg.Capsule.State.MountPath, cfg.Capsule.CatalogMountPath)
	}
	for _, root := range reserved {
		if root != "" && pathsOverlap(mountPath, filepath.Clean(root)) {
			return fmt.Errorf("secret reference %q: credential mount overlaps runtime path", id)
		}
	}
	return nil
}

func pathStrictlyWithin(root, path string) bool {
	rel, err := filepath.Rel(filepath.Clean(root), filepath.Clean(path))
	return err == nil && rel != "." && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

func capsuleSecretVolumeName(id string) string {
	digest := sha256.Sum256([]byte(id))
	return "gcs-" + hex.EncodeToString(digest[:8])
}

func removePodEnv(environment []corev1.EnvVar, name string) []corev1.EnvVar {
	result := environment[:0]
	for _, entry := range environment {
		if entry.Name != name {
			result = append(result, entry)
		}
	}
	return result
}

func hasK8sSecretEnvironment(refs []runtime.SecretReference) bool {
	for _, ref := range refs {
		if ref.Environment != "" && ref.Kubernetes != nil {
			return true
		}
	}
	return false
}
