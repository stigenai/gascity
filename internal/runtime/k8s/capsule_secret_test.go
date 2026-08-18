package k8s

import (
	"encoding/json"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"

	"github.com/gastownhall/gascity/internal/runtime"
)

func TestBuildPodCapsuleProjectsOnlySelectedProfileChainThroughNativeSecrets(t *testing.T) {
	t.Parallel()
	capsule := testK8sCapsuleLaunch(t)
	cfg := runtime.Config{
		Capsule: capsule,
		Env: map[string]string{
			"LINUX_USERNAME":   "capsule-user",
			"BACKEND_BASE_URL": "https://compatible.example.invalid",
		},
		SecretReferences: []runtime.SecretReference{
			{
				ID: "primary-backend-token", Environment: "BACKEND_API_KEY",
				Kubernetes: &runtime.KubernetesSecretKeyReference{Name: "compatible-primary", Key: "token"},
				SSH:        &runtime.SSHSecretPathReference{Path: "/srv/private/primary-token"},
			},
			{
				ID: "claude-primary-home", Environment: "CLAUDE_CONFIG_DIR", MountPath: "/run/gascity/omnigent/credentials/claude-primary",
				Kubernetes: &runtime.KubernetesSecretKeyReference{Name: "claude-primary", Key: "config"},
				SSH:        &runtime.SSHSecretPathReference{Path: "/srv/private/claude-primary"},
			},
			{
				ID: "claude-fallback-home", MountPath: "/run/gascity/omnigent/credentials/claude-fallback",
				Kubernetes: &runtime.KubernetesSecretKeyReference{Name: "claude-fallback", Key: "config", Optional: true},
				SSH:        &runtime.SSHSecretPathReference{Path: "/srv/private/claude-fallback"},
			},
		},
	}
	pod, err := buildPod("profile-chain", cfg, newProviderWithOps(newFakeK8sOps()))
	if err != nil {
		t.Fatalf("buildPod: %v", err)
	}

	agent := pod.Spec.Containers[0]
	env := envByName(agent.Env)
	apiKey := env["BACKEND_API_KEY"]
	if apiKey.Value != "" || apiKey.ValueFrom == nil || apiKey.ValueFrom.SecretKeyRef == nil {
		t.Fatalf("BACKEND_API_KEY = %#v, want SecretKeyRef", apiKey)
	}
	if ref := apiKey.ValueFrom.SecretKeyRef; ref.Name != "compatible-primary" || ref.Key != "token" || ref.Optional == nil || *ref.Optional {
		t.Fatalf("BACKEND_API_KEY reference = %#v", ref)
	}
	if env["BACKEND_BASE_URL"].Value != "https://compatible.example.invalid" {
		t.Fatalf("compatible backend metadata was not preserved: %#v", env["BACKEND_BASE_URL"])
	}
	claudeHome := env["CLAUDE_CONFIG_DIR"]
	if claudeHome.Value != "/run/gascity/omnigent/credentials/claude-primary" || claudeHome.ValueFrom != nil {
		t.Fatalf("CLAUDE_CONFIG_DIR = %#v, want literal projected mount path", claudeHome)
	}

	mounts := podVolumeMountsByName(agent.VolumeMounts)
	volumes := podVolumesByName(pod.Spec.Volumes)
	for _, want := range []struct {
		id, path, secret, key string
		optional              bool
	}{
		{id: "claude-primary-home", path: "/run/gascity/omnigent/credentials/claude-primary", secret: "claude-primary", key: "config"},
		{id: "claude-fallback-home", path: "/run/gascity/omnigent/credentials/claude-fallback", secret: "claude-fallback", key: "config", optional: true},
	} {
		name := capsuleSecretVolumeName(want.id)
		mount := mounts[name]
		if mount.MountPath != want.path || !mount.ReadOnly || mount.SubPath != "" {
			t.Fatalf("%s mount = %#v; want live-updating read-only directory", want.id, mount)
		}
		secret := volumes[name].Secret
		if secret == nil || secret.SecretName != want.secret || secret.Optional == nil || *secret.Optional != want.optional || len(secret.Items) != 1 || secret.Items[0].Key != want.key || secret.Items[0].Path != want.key {
			t.Fatalf("%s volume = %#v", want.id, secret)
		}
		if secret.DefaultMode == nil || *secret.DefaultMode != 0o440 || secret.Items[0].Mode == nil || *secret.Items[0].Mode != 0o440 {
			t.Fatalf("%s credential mode = %#v", want.id, secret)
		}
	}
	if _, exists := volumes["claude-config"]; exists {
		t.Fatal("capsule retained the legacy shared Claude credential volume")
	}
	if pod.Spec.SecurityContext == nil || pod.Spec.SecurityContext.FSGroup == nil || *pod.Spec.SecurityContext.FSGroup != agentUserGroupID {
		t.Fatalf("pod credential group = %#v", pod.Spec.SecurityContext)
	}
	if command := strings.Join(agent.Args, " "); !strings.Contains(command, "su -m capsule-user") || !strings.Contains(command, "usermod -a -G 1001") {
		t.Fatalf("dynamic-user entrypoint does not preserve secret env and credential group: %s", command)
	}
	if command := buildRespawnCommand(cfg); !strings.Contains(command, "su -m capsule-user") {
		t.Fatalf("dynamic-user relaunch does not preserve secret env: %s", command)
	}

	manifest, err := json.Marshal(pod)
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{"/srv/private", "codex-unselected", "credential-sentinel-must-never-project"} {
		if strings.Contains(string(manifest), forbidden) {
			t.Fatalf("Pod manifest leaked %q: %s", forbidden, manifest)
		}
	}
}

func TestBuildPodCapsuleRejectsLiteralCredentialsAndUnsafeProfileMounts(t *testing.T) {
	t.Parallel()
	capsule := testK8sCapsuleLaunch(t)
	sentinel := "credential-sentinel-must-never-project"
	_, err := buildPod("literal", runtime.Config{
		Capsule: capsule,
		Env:     map[string]string{"ANTHROPIC_API_KEY": sentinel},
	}, newProviderWithOps(newFakeK8sOps()))
	if err == nil || !strings.Contains(err.Error(), "ANTHROPIC_API_KEY") || strings.Contains(err.Error(), sentinel) {
		t.Fatalf("literal credential error = %v", err)
	}

	for name, mount := range map[string]string{
		"workspace overlap": "/workspace/private",
		"state overlap":     capsule.State.MountPath + "/private",
	} {
		t.Run(name, func(t *testing.T) {
			_, err := buildPod("unsafe-mount", runtime.Config{
				Capsule: capsule,
				SecretReferences: []runtime.SecretReference{{
					ID: "profile-home", MountPath: mount,
					Kubernetes: &runtime.KubernetesSecretKeyReference{Name: "profile", Key: "config"},
				}},
			}, newProviderWithOps(newFakeK8sOps()))
			if err == nil || !strings.Contains(err.Error(), "credential mount") {
				t.Fatalf("unsafe mount error = %v", err)
			}
		})
	}
}

func envByName(entries []corev1.EnvVar) map[string]corev1.EnvVar {
	result := make(map[string]corev1.EnvVar, len(entries))
	for _, entry := range entries {
		result[entry.Name] = entry
	}
	return result
}
