package main

import (
	"testing"

	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/runtime"
)

func TestRuntimeSecretReferencesProjectsTypedIdentityWithoutValues(t *testing.T) {
	t.Parallel()
	refs := []config.SecretReference{{
		ID: "claude", Environment: "CLAUDE_AUTH_TOKEN",
		Kubernetes: &config.KubernetesSecretKeyReference{Name: "claude-primary", Key: "token", Optional: true},
		SSH:        &config.SSHSecretPathReference{Path: "/srv/secrets/claude-primary-token"},
	}}
	got := runtimeSecretReferences(refs)
	if len(got) != 1 || got[0].ID != "claude" || got[0].Kubernetes.Name != "claude-primary" || !got[0].Kubernetes.Optional || got[0].SSH.Path != "/srv/secrets/claude-primary-token" {
		t.Fatalf("runtimeSecretReferences = %+v", got)
	}
	got[0].Kubernetes.Name = "mutated"
	got[0].SSH.Path = "/mutated"
	if refs[0].Kubernetes.Name != "claude-primary" || refs[0].SSH.Path != "/srv/secrets/claude-primary-token" {
		t.Fatal("runtime conversion aliased config pointers")
	}
}

func TestTemplateAndWorkerResolutionPreserveSecretReferences(t *testing.T) {
	t.Parallel()
	refs := []runtime.SecretReference{{
		ID: "codex", MountPath: "/run/secrets/codex",
		Kubernetes: &runtime.KubernetesSecretKeyReference{Name: "codex-auth", Key: "config"},
	}}
	got := templateParamsToConfig(TemplateParams{SecretReferences: refs})
	if len(got.SecretReferences) != 1 || got.SecretReferences[0].ID != "codex" {
		t.Fatalf("template config SecretReferences = %+v", got.SecretReferences)
	}

	agent := config.Agent{Name: "worker", SecretReferences: []config.SecretReference{{
		ID: "codex", MountPath: "/run/secrets/codex",
		Kubernetes: &config.KubernetesSecretKeyReference{Name: "codex-auth", Key: "config"},
	}}}
	cfg := &config.City{Agents: []config.Agent{agent}}
	hints := runtime.Config{}
	applyWorkerOverlayHints(&hints, cfg, "/city", "worker", &config.ResolvedProvider{Name: "codex"})
	if len(hints.SecretReferences) != 1 || hints.SecretReferences[0].Kubernetes.Name != "codex-auth" {
		t.Fatalf("worker hints SecretReferences = %+v", hints.SecretReferences)
	}
}
