package config

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/BurntSushi/toml"
	"github.com/gastownhall/gascity/internal/formula"
	"github.com/gastownhall/gascity/internal/fsys"
)

func TestOmnigentExamplePackDeclaresPrivateSupervisedSidecar(t *testing.T) {
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("resolve test source path")
	}
	packPath := filepath.Join(filepath.Dir(file), "..", "..", "examples", "omnigent", "pack.toml")
	var pack PackConfig
	if _, err := toml.DecodeFile(packPath, &pack); err != nil {
		t.Fatalf("decode Omnigent example pack: %v", err)
	}
	if pack.Pack.Name != "omnigent-local" || pack.Pack.Schema != 2 {
		t.Fatalf("pack metadata = %+v", pack.Pack)
	}
	if err := ValidateServices(pack.Services); err != nil {
		t.Fatalf("ValidateServices: %v", err)
	}
	if len(pack.Services) != 1 {
		t.Fatalf("services = %#v, want one Omnigent sidecar", pack.Services)
	}
	service := pack.Services[0]
	if service.Name != "omnigent" || service.KindOrDefault() != "proxy_process" || service.PublicationVisibilityOrDefault() != "private" {
		t.Fatalf("service = %#v", service)
	}
	if got := service.Process.StartupTimeoutDuration(); got != 30*time.Second {
		t.Fatalf("Omnigent process startup timeout = %s, want 30s", got)
	}
	wantCommand := []string{"gc", "omnigent", "serve"}
	if len(service.Process.Command) != len(wantCommand) {
		t.Fatalf("process command = %q, want %q", service.Process.Command, wantCommand)
	}
	for i := range wantCommand {
		if service.Process.Command[i] != wantCommand[i] {
			t.Fatalf("process command = %q, want %q", service.Process.Command, wantCommand)
		}
	}
	if service.Process.HealthPath != "/health" {
		t.Fatalf("health path = %q, want /health", service.Process.HealthPath)
	}
	if service.Publication.AllowWebSockets {
		t.Fatal("Omnigent local service must not opt into public WebSocket publication")
	}
	provider, ok := pack.Providers["omnigent"]
	if !ok {
		t.Fatal("Omnigent provider missing from example pack")
	}
	if provider.Command != "gc" || provider.PromptMode != "none" || provider.SessionIDFlag != "" {
		t.Fatalf("provider = %#v", provider)
	}
	if provider.OptionDefaults["profile"] != "offline-mock" || len(provider.OptionsSchema) != 1 {
		t.Fatalf("profile defaults/schema = %#v / %#v", provider.OptionDefaults, provider.OptionsSchema)
	}
	if provider.Capsule.Kind != "omnigent" || provider.Capsule.ProfileOption != "profile" || provider.Capsule.Catalog != ".gc/services/omnigent/config/profiles.yaml" {
		t.Fatalf("Omnigent capsule declaration = %+v", provider.Capsule)
	}
}

func TestOmnigentImportedPackCityAliasAndAgentOverrideCompose(t *testing.T) {
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("resolve test source path")
	}
	packPath := filepath.Join(filepath.Dir(file), "..", "..", "examples", "omnigent", "pack.toml")
	packData, err := os.ReadFile(packPath)
	if err != nil {
		t.Fatal(err)
	}
	fs := fsys.NewFake()
	fs.Files["/city/city.toml"] = []byte(`
[workspace]
name = "profile-precedence"

[imports.omnigent]
source = "packs/omnigent"

[providers.omnigent-city]
base = "provider:omnigent"

[providers.omnigent-city.option_defaults]
profile = "codex"

[[agent]]
name = "worker"
provider = "omnigent-city"

[agent.option_defaults]
profile = "claude-secondary"
`)
	fs.Files["/city/packs/omnigent/pack.toml"] = packData
	cfg, _, err := LoadWithIncludes(fs, "/city/city.toml")
	if err != nil {
		t.Fatalf("LoadWithIncludes: %v", err)
	}
	if len(cfg.Services) != 1 || cfg.Services[0].Name != "omnigent" {
		t.Fatalf("imported services = %#v", cfg.Services)
	}
	explicit := explicitAgents(cfg.Agents)
	if len(explicit) != 1 || explicit[0].Name != "worker" {
		t.Fatalf("explicit agents = %#v", explicit)
	}
	resolved, err := ResolveProvider(&explicit[0], &cfg.Workspace, cfg.Providers, lookPathOnly("gc"))
	if err != nil {
		t.Fatalf("ResolveProvider: %v", err)
	}
	if got := resolved.EffectiveDefaults["profile"]; got != "claude-secondary" {
		t.Fatalf("profile = %q, want agent override claude-secondary", got)
	}
	if got := cfg.Providers["omnigent-city"].OptionDefaults["profile"]; got != "codex" {
		t.Fatalf("city alias default = %q, want codex", got)
	}
	if got := cfg.Providers["omnigent"].OptionDefaults["profile"]; got != "offline-mock" {
		t.Fatalf("imported pack default = %q, want offline-mock", got)
	}
}

func TestOmnigentProviderProfilePrecedenceUsesStandardTypedOptions(t *testing.T) {
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("resolve test source path")
	}
	packPath := filepath.Join(filepath.Dir(file), "..", "..", "examples", "omnigent", "pack.toml")
	var pack PackConfig
	if _, err := toml.DecodeFile(packPath, &pack); err != nil {
		t.Fatal(err)
	}
	packProvider := pack.Providers["omnigent"]
	base := "provider:omnigent"
	providers := map[string]ProviderSpec{
		"omnigent": packProvider,
		"omnigent-city": {
			Base:           &base,
			OptionDefaults: map[string]string{"profile": "codex"},
		},
	}

	cityResolved, err := ResolveProvider(&Agent{Name: "city-default", Provider: "omnigent-city"}, nil, providers, lookPathOnly("gc"))
	if err != nil {
		t.Fatalf("ResolveProvider(city default): %v", err)
	}
	if got := cityResolved.EffectiveDefaults["profile"]; got != "codex" {
		t.Fatalf("city profile = %q, want codex", got)
	}
	chainResolved, err := ResolveProviderChain("omnigent-city", providers["omnigent-city"], providers)
	if err != nil {
		t.Fatalf("ResolveProviderChain(city default): %v", err)
	}
	if got := chainResolved.Provenance.MapKeyLayer["option_defaults"]["profile"]; got != "providers.omnigent-city" {
		t.Fatalf("profile provenance = %q, want providers.omnigent-city", got)
	}

	agentResolved, err := ResolveProvider(&Agent{
		Name: "agent-override", Provider: "omnigent-city",
		OptionDefaults: map[string]string{"profile": "claude-primary"},
	}, nil, providers, lookPathOnly("gc"))
	if err != nil {
		t.Fatalf("ResolveProvider(agent override): %v", err)
	}
	if got := agentResolved.EffectiveDefaults["profile"]; got != "claude-primary" {
		t.Fatalf("agent profile = %q, want claude-primary", got)
	}
	launch, err := BuildProviderLaunchCommand("", agentResolved, nil, "tmux")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(launch.Command, "gc omnigent attach --mode controller --profile claude-primary") {
		t.Fatalf("launch command = %q", launch.Command)
	}
	if !strings.Contains(agentResolved.ResumeCommand, "--conversation {{.SessionKey}} --profile claude-primary") {
		t.Fatalf("resume command = %q", agentResolved.ResumeCommand)
	}

	agentResolved.EffectiveDefaults["profile"] = "mutated"
	if providers["omnigent-city"].OptionDefaults["profile"] != "codex" || pack.Providers["omnigent"].OptionDefaults["profile"] != "offline-mock" {
		t.Fatal("resolved profile mutation leaked into city or imported pack config")
	}
}

func TestOmnigentExampleProfilesKeepOneGasCityTopology(t *testing.T) {
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("resolve test source path")
	}
	packPath := filepath.Join(filepath.Dir(file), "..", "..", "examples", "omnigent", "pack.toml")
	var pack PackConfig
	if _, err := toml.DecodeFile(packPath, &pack); err != nil {
		t.Fatal(err)
	}

	profiles := []string{"offline-mock", "codex", "claude-primary", "claude-secondary"}
	var topology string
	var graph []byte
	for _, profile := range profiles {
		agent := Agent{Name: "worker", Provider: "omnigent", OptionDefaults: map[string]string{"profile": profile}}
		resolved, err := ResolveProvider(&agent, nil, pack.Providers, lookPathOnly("gc"))
		if err != nil {
			t.Fatalf("ResolveProvider(%s): %v", profile, err)
		}
		launch, err := BuildProviderLaunchCommand("", resolved, nil, "tmux")
		if err != nil {
			t.Fatalf("BuildProviderLaunchCommand(%s): %v", profile, err)
		}
		if !strings.Contains(launch.Command, "gc omnigent attach --mode controller --profile "+profile) {
			t.Fatalf("launch(%s) = %q", profile, launch.Command)
		}
		gotTopology := agent.Name + ":" + agent.Provider + ":omnigent-service"
		if topology == "" {
			topology = gotTopology
		} else if gotTopology != topology {
			t.Fatalf("profile %s changed topology from %q to %q", profile, topology, gotTopology)
		}
		formulaDir := filepath.Join(filepath.Dir(packPath), "formulas")
		recipe, err := formula.Compile(context.Background(), "portable-worker", []string{formulaDir}, nil)
		if err != nil {
			t.Fatalf("Compile(portable-worker, %s): %v", profile, err)
		}
		encoded, err := json.Marshal(recipe)
		if err != nil {
			t.Fatal(err)
		}
		if graph == nil {
			graph = encoded
		} else if string(encoded) != string(graph) {
			t.Fatalf("profile %s changed formula/bead dependency graph", profile)
		}
		if len(recipe.Steps) != 4 || len(recipe.Deps) != 5 {
			t.Fatalf("portable workflow topology steps=%d deps=%d", len(recipe.Steps), len(recipe.Deps))
		}
	}
}
