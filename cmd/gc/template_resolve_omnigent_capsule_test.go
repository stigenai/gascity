package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/runtime"
	sessionhybrid "github.com/gastownhall/gascity/internal/runtime/hybrid"
)

func TestResolveTemplatePreparedMaterializesOmnigentCapsuleForRemoteRuntimes(t *testing.T) {
	for _, runtimeName := range []string{"k8s", "ssh:agent@example.test"} {
		t.Run(runtimeName, func(t *testing.T) {
			cityPath, cfg, agent := omnigentCapsuleResolutionFixture(t)
			provider := runtime.NewFake()
			params := newAgentBuildParams("capsule-city", cityPath, cfg, provider, time.Unix(0, 0), nil, nil)
			params.sessionProvider = runtimeName
			params.lookPath = func(string) (string, error) { return "/test/bin/gc", nil }

			tp, err := resolveTemplatePrepared(params, agent, agent.QualifiedName(), nil)
			if err != nil {
				t.Fatalf("resolveTemplatePrepared(%s): %v", runtimeName, err)
			}
			if tp.Capsule == nil {
				t.Fatalf("resolveTemplatePrepared(%s) omitted runtime capsule", runtimeName)
			}
			if err := tp.Capsule.Validate(); err != nil {
				t.Fatalf("runtime capsule validation: %v", err)
			}
			if got := strings.Join(tp.Capsule.Command, " "); !strings.Contains(got, "omnigent attach --mode capsule") || strings.Contains(got, "--mode controller") {
				t.Fatalf("capsule command = %q", got)
			}
			if tp.Command != strings.Join(tp.Capsule.Command, " ") {
				t.Fatalf("template command = %q, capsule command = %q", tp.Command, tp.Capsule.Command)
			}
			if len(tp.Capsule.CatalogInputs) != 2 || len(tp.Capsule.State.ResourceUID) == 0 {
				t.Fatalf("capsule inputs/state = %+v / %+v", tp.Capsule.CatalogInputs, tp.Capsule.State)
			}
			if len(tp.SecretReferences) != 1 || tp.SecretReferences[0].ID != "codex-home" {
				t.Fatalf("template secret references = %+v", tp.SecretReferences)
			}
			if ref := tp.SecretReferences[0]; ref.Environment != "CODEX_HOME" || ref.MountPath != "/run/gascity/omnigent/credentials/codex" {
				t.Fatalf("template auth-home projection = %+v", ref)
			}
			if _, leaked := tp.Env["CODEX_HOME"]; leaked {
				t.Fatalf("provider credential destination leaked into runtime env: %+v", tp.Env)
			}
			got := templateParamsToConfig(tp)
			if got.Capsule == nil || got.Capsule.Key != tp.Capsule.Key {
				t.Fatalf("runtime config capsule = %+v, want %+v", got.Capsule, tp.Capsule)
			}
			if calls := provider.Calls; len(calls) == 0 || calls[len(calls)-1].Method != "EnsureCapsuleState" {
				t.Fatalf("provider calls = %+v, want EnsureCapsuleState", calls)
			}
		})
	}
}

func TestResolveTemplatePreparedKeepsLocalOmnigentControllerMode(t *testing.T) {
	cityPath, cfg, agent := omnigentCapsuleResolutionFixture(t)
	provider := runtime.NewFake()
	params := newAgentBuildParams("capsule-city", cityPath, cfg, provider, time.Unix(0, 0), nil, nil)
	params.sessionProvider = "tmux"
	params.lookPath = func(string) (string, error) { return "/test/bin/gc", nil }

	tp, err := resolveTemplatePrepared(params, agent, agent.QualifiedName(), nil)
	if err != nil {
		t.Fatalf("resolveTemplatePrepared(local): %v", err)
	}
	if tp.Capsule != nil {
		t.Fatalf("local template unexpectedly materialized capsule: %+v", tp.Capsule)
	}
	if !strings.Contains(tp.Command, "omnigent attach --mode controller") {
		t.Fatalf("local command = %q", tp.Command)
	}
	for _, call := range provider.Calls {
		if call.Method == "EnsureCapsuleState" {
			t.Fatalf("local resolution allocated remote capsule state: %+v", provider.Calls)
		}
	}
}

func TestResolveTemplatePreparedRejectsRemoteOmnigentWithoutCapsuleCapabilities(t *testing.T) {
	cityPath, cfg, agent := omnigentCapsuleResolutionFixture(t)
	provider := struct{ runtime.Provider }{Provider: runtime.NewFake()}
	params := newAgentBuildParams("capsule-city", cityPath, cfg, provider, time.Unix(0, 0), nil, nil)
	params.sessionProvider = "k8s"
	params.lookPath = func(string) (string, error) { return "/test/bin/gc", nil }

	_, err := resolveTemplatePrepared(params, agent, agent.QualifiedName(), nil)
	if err == nil || !strings.Contains(err.Error(), "capsule state") {
		t.Fatalf("resolveTemplatePrepared error = %v, want missing capsule state capability", err)
	}
}

func TestResolveTemplatePreparedUsesFactualHybridCapsuleRoute(t *testing.T) {
	for _, remoteRoute := range []bool{false, true} {
		name := "local"
		if remoteRoute {
			name = "remote"
		}
		t.Run(name, func(t *testing.T) {
			cityPath, cfg, agent := omnigentCapsuleResolutionFixture(t)
			local := runtime.NewFake()
			remote := runtime.NewFake()
			provider := sessionhybrid.New(local, remote, func(string) bool { return remoteRoute })
			params := newAgentBuildParams("capsule-city", cityPath, cfg, provider, time.Unix(0, 0), nil, nil)
			params.sessionProvider = "hybrid"
			params.lookPath = func(string) (string, error) { return "/test/bin/gc", nil }

			tp, err := resolveTemplatePrepared(params, agent, agent.QualifiedName(), nil)
			if err != nil {
				t.Fatalf("resolveTemplatePrepared(hybrid %s): %v", name, err)
			}
			if remoteRoute {
				if tp.Capsule == nil || remote.CountCalls("EnsureCapsuleState", tp.SessionName) != 1 {
					t.Fatalf("remote hybrid capsule/calls = %+v / %+v", tp.Capsule, remote.Calls)
				}
				if local.CountCalls("EnsureCapsuleState", tp.SessionName) != 0 {
					t.Fatalf("local backend received capsule state call: %+v", local.Calls)
				}
				return
			}
			if tp.Capsule != nil || !strings.Contains(tp.Command, "--mode controller") {
				t.Fatalf("local hybrid resolution = capsule %+v command %q", tp.Capsule, tp.Command)
			}
		})
	}
}

func TestResolveTemplatePreparedRejectsOmnigentCatalogOutsideCity(t *testing.T) {
	tests := []struct {
		name    string
		catalog func(t *testing.T, cityPath string) string
	}{
		{
			name: "parent traversal",
			catalog: func(_ *testing.T, _ string) string {
				return "../profiles.yaml"
			},
		},
		{
			name: "symlink escape",
			catalog: func(t *testing.T, cityPath string) string {
				outside := filepath.Join(t.TempDir(), "profiles.yaml")
				if err := os.WriteFile(outside, []byte("outside"), 0o600); err != nil {
					t.Fatal(err)
				}
				link := filepath.Join(cityPath, "profiles-link.yaml")
				if err := os.Symlink(outside, link); err != nil {
					t.Fatal(err)
				}
				return "profiles-link.yaml"
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cityPath, cfg, agent := omnigentCapsuleResolutionFixture(t)
			spec := cfg.Providers["omnigent"]
			spec.Capsule.Catalog = tt.catalog(t, cityPath)
			cfg.Providers["omnigent"] = spec
			provider := runtime.NewFake()
			params := newAgentBuildParams("capsule-city", cityPath, cfg, provider, time.Unix(0, 0), nil, nil)
			params.sessionProvider = "k8s"
			params.lookPath = func(string) (string, error) { return "/test/bin/gc", nil }

			_, err := resolveTemplatePrepared(params, agent, agent.QualifiedName(), nil)
			if err == nil || !strings.Contains(err.Error(), "escapes the city") {
				t.Fatalf("resolveTemplatePrepared error = %v, want city containment failure", err)
			}
			for _, call := range provider.Calls {
				if call.Method == "EnsureCapsuleState" {
					t.Fatalf("catalog containment failure allocated state: %+v", provider.Calls)
				}
			}
		})
	}
}

func omnigentCapsuleResolutionFixture(t *testing.T) (string, *config.City, *config.Agent) {
	t.Helper()
	cityPath := t.TempDir()
	catalogDir := filepath.Join(cityPath, ".gc", "services", "omnigent", "config")
	agentDir := filepath.Join(catalogDir, "agents")
	if err := os.MkdirAll(agentDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(agentDir, "codex.yaml"), []byte("name: capsule-codex\nexecutor:\n  harness: codex\n  model: openai/test\nprompt: Run the assigned test task.\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	catalog := `version: 1
omnigent:
  commit: 0123456789abcdef0123456789abcdef01234567
  package_version: 0.1.0
  executable: /opt/gascity/bin/omnigent
  sha256: sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa
profiles:
  codex:
    display_name: Codex
    blurb: Compatible Codex backend.
    harness: codex
    backend: compatible
    network: external-model
    agent: agents/codex.yaml
    environment: [CODEX_HOME]
    secret_references: [codex-home]
`
	if err := os.WriteFile(filepath.Join(catalogDir, "profiles.yaml"), []byte(catalog), 0o600); err != nil {
		t.Fatal(err)
	}

	cfg := &config.City{
		Workspace: config.Workspace{Name: "capsule-city", Provider: "omnigent"},
		Providers: map[string]config.ProviderSpec{
			"omnigent": {
				Command:    "gc",
				Args:       []string{"omnigent", "attach", "--mode", "controller"},
				PromptMode: "none",
				Capsule: config.ProviderCapsuleSpec{
					Kind:          "omnigent",
					ProfileOption: "profile",
					Catalog:       ".gc/services/omnigent/config/profiles.yaml",
				},
				OptionDefaults: map[string]string{"profile": "codex"},
				OptionsSchema: []config.ProviderOption{{
					Key: "profile", Label: "Profile", Type: "select", Default: "codex",
					Choices: []config.OptionChoice{{Value: "codex", Label: "Codex", FlagArgs: []string{"--profile", "codex"}}},
				}},
			},
		},
	}
	agent := &config.Agent{
		Name:     "worker",
		Provider: "omnigent",
		SecretReferences: []config.SecretReference{{
			ID:          "codex-home",
			Environment: "CODEX_HOME",
			MountPath:   "/run/gascity/omnigent/credentials/codex",
			Kubernetes:  &config.KubernetesSecretKeyReference{Name: "codex-auth", Key: "config"},
			SSH:         &config.SSHSecretPathReference{Path: "/srv/gascity/secrets/codex-home"},
		}},
	}
	cfg.Agents = []config.Agent{*agent}
	return cityPath, cfg, agent
}
