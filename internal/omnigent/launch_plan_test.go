package omnigent

import (
	"errors"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/runtime"
)

func TestResolveAttachmentLaunchPlanLocalAndRemotePlacement(t *testing.T) {
	t.Parallel()
	base := testCapsuleLaunchInput(t)
	tests := []struct {
		name         string
		runtime      string
		hybridSet    bool
		hybridRemote bool
		wantMode     AttachmentLocation
		wantProvider runtime.SecretProvider
		wantCapsule  bool
	}{
		{name: "local tmux", runtime: "tmux", wantMode: AttachmentLocationController},
		{name: "local herdr", runtime: "herdr", wantMode: AttachmentLocationController},
		{name: "kubernetes", runtime: "k8s", wantMode: AttachmentLocationCapsule, wantProvider: runtime.SecretProviderKubernetes, wantCapsule: true},
		{name: "ssh", runtime: "ssh:worker@example", wantMode: AttachmentLocationCapsule, wantProvider: runtime.SecretProviderSSH, wantCapsule: true},
		{name: "hybrid local", runtime: "hybrid", hybridSet: true, wantMode: AttachmentLocationController},
		{name: "hybrid remote", runtime: "hybrid", hybridSet: true, hybridRemote: true, wantMode: AttachmentLocationCapsule, wantProvider: runtime.SecretProviderKubernetes, wantCapsule: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			input := base
			input.Runtime = tt.runtime
			input.HybridRouteSet = tt.hybridSet
			input.HybridRemote = tt.hybridRemote
			if !tt.wantCapsule {
				input.SecretReferences = nil
			}
			plan, err := ResolveAttachmentLaunchPlan(input)
			if err != nil {
				t.Fatalf("ResolveAttachmentLaunchPlan: %v", err)
			}
			if plan.Location != tt.wantMode || plan.SecretProvider != tt.wantProvider {
				t.Fatalf("plan location/provider = %q/%q, want %q/%q", plan.Location, plan.SecretProvider, tt.wantMode, tt.wantProvider)
			}
			if tt.wantCapsule {
				if err := plan.CapsuleKey.Validate(); err != nil || len(plan.SecretReferences) != 1 || len(plan.ProfileCredentials) != 1 || plan.ProfileID != "profile" {
					t.Fatalf("capsule plan identity/secrets = %+v / %v", plan, err)
				}
				if strings.Contains(strings.Join(plan.CommandArgs(), " "), "controller") || strings.Contains(strings.Join(plan.CommandArgs(), " "), "managed") {
					t.Fatalf("capsule argv permits controller/managed fallback: %v", plan.CommandArgs())
				}
			}
		})
	}
}

func TestResolveAttachmentLaunchPlanRejectsAmbiguousOrUnsafePlacement(t *testing.T) {
	t.Parallel()
	for name, mutate := range map[string]func(*AttachmentLaunchInput){
		"hybrid route omitted": func(in *AttachmentLaunchInput) { in.Runtime = "hybrid" },
		"unsupported runtime":  func(in *AttachmentLaunchInput) { in.Runtime = "daytona" },
		"relative workspace":   func(in *AttachmentLaunchInput) { in.Runtime = "k8s"; in.Workspace = "work" },
		"state root escape":    func(in *AttachmentLaunchInput) { in.Runtime = "k8s"; in.StateRoot = "/var/lib/gascity/../foreign" },
		"socket outside run":   func(in *AttachmentLaunchInput) { in.Runtime = "k8s"; in.SocketPath = "/tmp/omnigent.sock" },
		"catalog outside root": func(in *AttachmentLaunchInput) { in.Runtime = "k8s"; in.CatalogPath = "/tmp/profiles.yaml" },
		"unsupported source": func(in *AttachmentLaunchInput) {
			in.Runtime = "ssh:worker@example"
			in.SecretReferences[0].SSH = nil
		},
	} {
		t.Run(name, func(t *testing.T) {
			input := testCapsuleLaunchInput(t)
			mutate(&input)
			if _, err := ResolveAttachmentLaunchPlan(input); err == nil {
				t.Fatal("ResolveAttachmentLaunchPlan succeeded, want error")
			}
		})
	}

	input := testCapsuleLaunchInput(t)
	input.Runtime = "ssh:worker@example"
	input.SecretReferences[0].SSH = nil
	_, err := ResolveAttachmentLaunchPlan(input)
	if !errors.Is(err, runtime.ErrSecretSourceUnavailable) {
		t.Fatalf("missing SSH source error = %v, want ErrSecretSourceUnavailable", err)
	}
}

func TestAttachmentLaunchPlanFingerprintIsDeterministicAndSensitiveToInputs(t *testing.T) {
	t.Parallel()
	input := testCapsuleLaunchInput(t)
	input.Runtime = "k8s"
	first, err := ResolveAttachmentLaunchPlan(input)
	if err != nil {
		t.Fatal(err)
	}
	second, err := ResolveAttachmentLaunchPlan(input)
	if err != nil || first.Fingerprint() != second.Fingerprint() {
		t.Fatalf("fingerprint is not deterministic: %q %q %v", first.Fingerprint(), second.Fingerprint(), err)
	}
	changed := input
	changed.CatalogSHA256 = "sha256:" + strings.Repeat("b", 64)
	third, err := ResolveAttachmentLaunchPlan(changed)
	if err != nil {
		t.Fatal(err)
	}
	if first.Fingerprint() == third.Fingerprint() {
		t.Fatal("pin change did not change launch plan fingerprint")
	}
	if !strings.HasPrefix(first.Fingerprint(), "v1:") {
		t.Fatalf("fingerprint = %q, want v1 prefix", first.Fingerprint())
	}
}

func testCapsuleLaunchInput(t *testing.T) AttachmentLaunchInput {
	t.Helper()
	return AttachmentLaunchInput{
		ProfileID: "profile", Catalog: testCapsuleCredentialCatalog(t),
		Workspace: "/workspace/rig", CityScope: "cluster-a/namespace-a/city-a", SessionID: "ga-session",
		StateRoot: CapsuleStateRoot, SocketPath: CapsuleSocketPath, CatalogPath: CapsuleCatalogPath,
		CatalogSHA256: "sha256:" + strings.Repeat("c", 64),
		Pin:           Pin{Commit: strings.Repeat("a", 40), PackageVersion: "0.10.0.dev0", Executable: "omnigent", SHA256: "sha256:" + strings.Repeat("1", 64)},
		SecretReferences: []runtime.SecretReference{{
			ID: "profile", Environment: "CLAUDE_AUTH_TOKEN",
			Kubernetes: &runtime.KubernetesSecretKeyReference{Name: "claude-primary", Key: "token"},
			SSH:        &runtime.SSHSecretPathReference{Path: "/srv/gc-secrets/claude-primary-token"},
		}},
	}
}

func testCapsuleCredentialCatalog(t *testing.T) *Catalog {
	t.Helper()
	root := t.TempDir()
	writeCatalogTestFile(t, root, "agent.yaml", "name: fixture-agent\nprompt: work\n")
	path := writeCatalogTestFile(t, root, "catalog.yaml", validCatalogHeader()+`profiles:
  profile:
    display_name: Profile
    blurb: Compatible backend profile.
    harness: claude-sdk
    backend: compatible
    network: external-model
    agent: agent.yaml
    secret_references: [profile]
`)
	catalog, err := LoadCatalog(path)
	if err != nil {
		t.Fatal(err)
	}
	return catalog
}
