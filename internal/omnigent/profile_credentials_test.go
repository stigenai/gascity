package omnigent

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/runtime"
)

func TestProjectProfileCredentialsIsolatesSelectedChainAndProvider(t *testing.T) {
	catalog := loadProfileCredentialCatalog(t)
	refs := profileCredentialReferences()
	sentinel := "credential-sentinel-must-never-project"
	t.Setenv("CLAUDE_PRIMARY_TOKEN", sentinel)

	claude, err := catalog.ProjectProfileCredentials("claude-primary", runtime.SecretProviderKubernetes, refs)
	if err != nil {
		t.Fatal(err)
	}
	if len(claude) != 2 || claude[0].ProfileID != "claude-primary" || claude[1].ProfileID != "claude-secondary" {
		t.Fatalf("Claude projection = %#v", claude)
	}
	if claude[0].Backend != "compatible-primary" || claude[0].Blurb != "Primary compatible backend." || claude[1].Backend != "compatible-secondary" {
		t.Fatalf("Claude backend metadata = %#v", claude)
	}
	for i, wantID := range []string{"claude-primary-home", "claude-secondary-home"} {
		if len(claude[i].References) != 1 || claude[i].References[0].ID != wantID || claude[i].References[0].Kubernetes == nil || claude[i].References[0].SSH != nil {
			t.Fatalf("Claude projection[%d] = %#v", i, claude[i])
		}
	}
	if strings.Contains(profileCredentialProjectionText(claude), "codex-home") {
		t.Fatalf("Claude projection contains Codex credential: %#v", claude)
	}
	publicJSON, err := json.Marshal(catalog.PublicProfilesWithEnvironment(func(string) (string, bool) { return "available", true }))
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{"claude-primary-home", "/run/gascity", sentinel} {
		if strings.Contains(string(publicJSON), forbidden) {
			t.Fatalf("public profile discovery leaked %q: %s", forbidden, publicJSON)
		}
	}

	codex, err := catalog.ProjectProfileCredentials("codex", runtime.SecretProviderSSH, refs)
	if err != nil {
		t.Fatal(err)
	}
	if len(codex) != 1 || codex[0].ProfileID != "codex" || len(codex[0].References) != 1 || codex[0].References[0].ID != "codex-home" || codex[0].References[0].SSH == nil || codex[0].References[0].Kubernetes != nil {
		t.Fatalf("Codex SSH projection = %#v", codex)
	}
	if codex[0].References[0].MountPath != "/run/gascity/omnigent/credentials/codex" {
		t.Fatalf("Codex config home = %q", codex[0].References[0].MountPath)
	}
	if strings.Contains(profileCredentialProjectionText(codex), "claude-primary-home") || strings.Contains(profileCredentialProjectionText(codex), "claude-secondary-home") {
		t.Fatalf("Codex projection contains Claude credential: %#v", codex)
	}
}

func TestProjectProfileCredentialsFailsTypedOnMissingReferenceOrProviderSource(t *testing.T) {
	catalog := loadProfileCredentialCatalog(t)
	refs := profileCredentialReferences()

	_, err := catalog.ProjectProfileCredentials("claude-primary", runtime.SecretProviderKubernetes, refs[1:])
	if !errors.Is(err, ErrProfileSecretReferenceUnavailable) {
		t.Fatalf("missing logical reference error = %v", err)
	}
	var profileErr *ProfileSecretReferenceError
	if !errors.As(err, &profileErr) || profileErr.ProfileID != "claude-primary" || profileErr.ReferenceID != "claude-primary-home" {
		t.Fatalf("typed missing reference error = %#v", profileErr)
	}

	refs[0].SSH = nil
	_, err = catalog.ProjectProfileCredentials("claude-primary", runtime.SecretProviderSSH, refs)
	if !errors.Is(err, runtime.ErrSecretSourceUnavailable) {
		t.Fatalf("missing provider source error = %v", err)
	}
}

func TestCatalogRejectsCredentialReferenceSharedAcrossProfiles(t *testing.T) {
	root := t.TempDir()
	writeCatalogTestFile(t, root, "agents/one.yaml", "name: one\nprompt: work\n")
	writeCatalogTestFile(t, root, "agents/two.yaml", "name: two\nprompt: work\n")
	path := writeCatalogTestFile(t, root, "catalog.yaml", validCatalogHeader()+`profiles:
  one:
    display_name: One
    blurb: First independent backend.
    harness: claude-sdk
    backend: first
    network: external-model
    agent: agents/one.yaml
    secret_references: [shared-home]
  two:
    display_name: Two
    blurb: Second independent backend.
    harness: claude-sdk
    backend: second
    network: external-model
    agent: agents/two.yaml
    secret_references: [shared-home]
`)
	if _, err := LoadCatalog(path); err == nil || !strings.Contains(err.Error(), "shared-home") || !strings.Contains(err.Error(), "profiles") {
		t.Fatalf("shared credential reference error = %v", err)
	}
}

func loadProfileCredentialCatalog(t *testing.T) *Catalog {
	t.Helper()
	root := t.TempDir()
	writeCatalogTestFile(t, root, "agents/claude-primary.yaml", "name: claude-primary\nprompt: work\n")
	writeCatalogTestFile(t, root, "agents/claude-secondary.yaml", "name: claude-secondary\nprompt: work\n")
	writeCatalogTestFile(t, root, "agents/codex.yaml", "name: codex\nprompt: work\n")
	path := writeCatalogTestFile(t, root, "catalog.yaml", validCatalogHeader()+`profiles:
  claude-primary:
    display_name: Claude primary
    blurb: Primary compatible backend.
    harness: claude-sdk
    backend: compatible-primary
    network: external-model
    agent: agents/claude-primary.yaml
    fallbacks: [claude-secondary]
    secret_references: [claude-primary-home]
  claude-secondary:
    display_name: Claude secondary
    blurb: Secondary compatible backend.
    harness: claude-sdk
    backend: compatible-secondary
    network: external-model
    agent: agents/claude-secondary.yaml
    secret_references: [claude-secondary-home]
  codex:
    display_name: Codex
    blurb: Codex local account.
    harness: codex
    backend: openai-compatible
    network: external-model
    agent: agents/codex.yaml
    secret_references: [codex-home]
`)
	catalog, err := LoadCatalog(path)
	if err != nil {
		t.Fatal(err)
	}
	return catalog
}

func profileCredentialReferences() []runtime.SecretReference {
	return []runtime.SecretReference{
		{ID: "claude-primary-home", MountPath: "/run/gascity/omnigent/credentials/claude-primary", Kubernetes: &runtime.KubernetesSecretKeyReference{Name: "claude-primary", Key: "config"}, SSH: &runtime.SSHSecretPathReference{Path: "/srv/gc-secrets/claude-primary"}},
		{ID: "claude-secondary-home", MountPath: "/run/gascity/omnigent/credentials/claude-secondary", Kubernetes: &runtime.KubernetesSecretKeyReference{Name: "claude-secondary", Key: "config"}, SSH: &runtime.SSHSecretPathReference{Path: "/srv/gc-secrets/claude-secondary"}},
		{ID: "codex-home", MountPath: "/run/gascity/omnigent/credentials/codex", Kubernetes: &runtime.KubernetesSecretKeyReference{Name: "codex", Key: "config"}, SSH: &runtime.SSHSecretPathReference{Path: "/srv/gc-secrets/codex"}},
	}
}

func profileCredentialProjectionText(projections []ProfileCredentialProjection) string {
	var parts []string
	for _, projection := range projections {
		parts = append(parts, projection.ProfileID, projection.Backend, projection.Blurb)
		for _, ref := range projection.References {
			parts = append(parts, ref.ID, ref.Environment, ref.MountPath)
		}
	}
	return strings.Join(parts, "\x00")
}
