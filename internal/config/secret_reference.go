package config

import (
	"fmt"
	"path/filepath"
	"regexp"
	"strings"
)

var (
	secretReferenceIDPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]{0,62}$`)
	secretEnvironmentPattern = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
	secretKubernetesName     = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`)
	secretKubernetesKey      = regexp.MustCompile(`^[A-Za-z0-9._-]+$`)
)

// KubernetesSecretKeyReference identifies one Kubernetes Secret key. It never
// contains or resolves the key's value.
type KubernetesSecretKeyReference struct {
	Name string `toml:"name" jsonschema:"required"`
	Key  string `toml:"key" jsonschema:"required"`
}

// SSHSecretPathReference identifies an owner-only credential file or directory
// already provisioned on the SSH host.
type SSHSecretPathReference struct {
	Path string `toml:"path" jsonschema:"required"`
}

// SecretReference maps a logical credential to one environment or mount
// destination. Kubernetes and SSH sources may coexist for hybrid placement;
// each provider resolves only its own source at the provider edge.
type SecretReference struct {
	ID          string                        `toml:"id" jsonschema:"required"`
	Environment string                        `toml:"environment,omitempty"`
	MountPath   string                        `toml:"mount_path,omitempty"`
	Kubernetes  *KubernetesSecretKeyReference `toml:"kubernetes,omitempty"`
	SSH         *SSHSecretPathReference       `toml:"ssh,omitempty"`
}

func validateSecretReferences(refs []SecretReference) error {
	ids := make(map[string]bool, len(refs))
	destinations := make(map[string]bool, len(refs))
	for i := range refs {
		ref := refs[i]
		if !secretReferenceIDPattern.MatchString(ref.ID) {
			return fmt.Errorf("secret reference[%d]: id must match [A-Za-z0-9][A-Za-z0-9_-]{0,62}", i)
		}
		if ids[ref.ID] {
			return fmt.Errorf("secret reference %q: duplicate id", ref.ID)
		}
		ids[ref.ID] = true
		hasEnv, hasMount := ref.Environment != "", ref.MountPath != ""
		if hasEnv == hasMount {
			return fmt.Errorf("secret reference %q: exactly one environment or mount_path destination is required", ref.ID)
		}
		destination := "env:" + ref.Environment
		if hasEnv {
			if !secretEnvironmentPattern.MatchString(ref.Environment) || forbiddenAgentSecretEnvironment(ref.Environment) {
				return fmt.Errorf("secret reference %q: environment destination is reserved or invalid", ref.ID)
			}
		} else {
			clean := filepath.Clean(ref.MountPath)
			if !filepath.IsAbs(ref.MountPath) || clean != ref.MountPath || clean == string(filepath.Separator) {
				return fmt.Errorf("secret reference %q: mount_path must be a clean absolute path", ref.ID)
			}
			destination = "mount:" + clean
		}
		if destinations[destination] {
			return fmt.Errorf("secret reference %q: duplicate destination", ref.ID)
		}
		destinations[destination] = true
		if ref.Kubernetes == nil && ref.SSH == nil {
			return fmt.Errorf("secret reference %q: at least one provider source is required", ref.ID)
		}
		if ref.Kubernetes != nil && (len(ref.Kubernetes.Name) > 253 || !secretKubernetesName.MatchString(ref.Kubernetes.Name) || len(ref.Kubernetes.Key) > 253 || !secretKubernetesKey.MatchString(ref.Kubernetes.Key)) {
			return fmt.Errorf("secret reference %q: invalid Kubernetes Secret name or key", ref.ID)
		}
		if ref.SSH != nil {
			clean := filepath.Clean(ref.SSH.Path)
			if !filepath.IsAbs(ref.SSH.Path) || clean != ref.SSH.Path || clean == string(filepath.Separator) {
				return fmt.Errorf("secret reference %q: SSH path must be a clean absolute path", ref.ID)
			}
		}
	}
	return nil
}

func forbiddenAgentSecretEnvironment(name string) bool {
	if strings.HasPrefix(name, "GC_") || strings.HasPrefix(name, "OMNIGENT_") {
		return true
	}
	switch name {
	case "HOME", "PATH", "SHELL", "USER", "LOGNAME", "TMPDIR", "PWD", "OLDPWD":
		return true
	default:
		return false
	}
}

func cloneSecretReferences(in []SecretReference) []SecretReference {
	if in == nil {
		return nil
	}
	out := make([]SecretReference, len(in))
	for i := range in {
		out[i] = in[i]
		if in[i].Kubernetes != nil {
			v := *in[i].Kubernetes
			out[i].Kubernetes = &v
		}
		if in[i].SSH != nil {
			v := *in[i].SSH
			out[i].SSH = &v
		}
	}
	return out
}
