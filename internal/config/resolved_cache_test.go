package config

import (
	"strings"
	"testing"
)

func TestBuildResolvedProviderCache_Empty(t *testing.T) {
	cfg := &City{}
	if err := BuildResolvedProviderCache(cfg); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.ResolvedProviders != nil {
		t.Errorf("expected nil cache for empty providers, got %v", cfg.ResolvedProviders)
	}
}

func TestBuildResolvedProviderCache_BasicChain(t *testing.T) {
	base := "builtin:codex"
	cfg := &City{
		Providers: map[string]ProviderSpec{
			"codex-max": {
				Base:          &base,
				Command:       "aimux",
				Args:          []string{"run", "codex"},
				ResumeCommand: "aimux run codex -- resume {{.SessionKey}}",
			},
		},
	}
	if err := BuildResolvedProviderCache(cfg); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	got, ok := ResolvedProviderCached(cfg, "codex-max")
	if !ok {
		t.Fatalf("cache miss for codex-max")
	}
	if got.BuiltinAncestor != "codex" {
		t.Errorf("BuiltinAncestor = %q, want codex", got.BuiltinAncestor)
	}
	if got.PromptMode != "arg" {
		t.Errorf("PromptMode = %q, want arg (inherited from builtin)", got.PromptMode)
	}
}

func TestBuildResolvedProviderCache_CycleLeavesOldCache(t *testing.T) {
	base := "provider:b"
	base2 := "provider:a"
	cfg := &City{
		Providers: map[string]ProviderSpec{
			"a": {Base: &base, Command: "a"},
			"b": {Base: &base2, Command: "b"},
		},
	}
	// Pre-populate with a known-good cache entry.
	priorBase := "builtin:codex"
	cfg.ResolvedProviders = map[string]ResolvedProvider{
		"sentinel": {Name: "sentinel", BuiltinAncestor: "codex"},
	}
	prior := cfg.ResolvedProviders
	_ = priorBase

	err := BuildResolvedProviderCache(cfg)
	if err == nil {
		t.Fatal("expected cycle error")
	}
	// Cache must not be overwritten on error.
	if len(cfg.ResolvedProviders) != 1 {
		t.Fatalf("cache was overwritten despite error: %+v", cfg.ResolvedProviders)
	}
	if _, ok := prior["sentinel"]; !ok {
		t.Errorf("sentinel entry missing from preserved cache")
	}
}

func TestBuildResolvedProviderCache_ReportsAllChainErrors(t *testing.T) {
	aBase := "provider:b"
	bBase := "provider:a"
	missingBase := "provider:missing"
	cfg := &City{
		Providers: map[string]ProviderSpec{
			"a":       {Base: &aBase, Command: "a"},
			"b":       {Base: &bBase, Command: "b"},
			"missing": {Base: &missingBase, Command: "missing"},
		},
	}
	err := BuildResolvedProviderCache(cfg)
	if err == nil {
		t.Fatal("expected cache build error")
	}
	msg := err.Error()
	for _, want := range []string{`resolving provider "a"`, `resolving provider "b"`, `resolving provider "missing"`} {
		if !strings.Contains(msg, want) {
			t.Fatalf("error %q missing %q", msg, want)
		}
	}
}

func TestBuildResolvedProviderCache_RejectsInvalidLegacyBuiltinOptionDefaults(t *testing.T) {
	cfg := &City{
		Providers: map[string]ProviderSpec{
			"codex-fast": {
				Command: "codex",
				OptionDefaults: map[string]string{
					"permission_mode": "typo",
				},
			},
		},
	}

	err := BuildResolvedProviderCache(cfg)
	if err == nil {
		t.Fatal("expected invalid option_defaults to fail cache build")
	}
	msg := err.Error()
	if !strings.Contains(msg, `provider "codex-fast" option_defaults`) {
		t.Fatalf("error = %q, want provider option_defaults context", msg)
	}
	if !strings.Contains(msg, `"permission_mode"`) || !strings.Contains(msg, `"typo"`) {
		t.Fatalf("error = %q, want invalid option default details", msg)
	}
}

func TestBuildResolvedProviderCache_AllowsValidLegacyBuiltinOptionDefaults(t *testing.T) {
	cfg := &City{
		Providers: map[string]ProviderSpec{
			"codex-fast": {
				Command: "codex",
				OptionDefaults: map[string]string{
					"permission_mode": "unrestricted",
				},
			},
		},
	}

	if err := BuildResolvedProviderCache(cfg); err != nil {
		t.Fatalf("unexpected error for valid legacy builtin option defaults: %v", err)
	}
}

func TestResolvedProviderCached_DeepCopyIsolatesMutations(t *testing.T) {
	base := "builtin:codex"
	cfg := &City{
		Providers: map[string]ProviderSpec{
			"codex-max": {
				Base:          &base,
				Command:       "aimux",
				Args:          []string{"run", "codex"},
				ResumeCommand: "aimux run codex -- resume {{.SessionKey}}",
			},
		},
	}
	if err := BuildResolvedProviderCache(cfg); err != nil {
		t.Fatalf("build: %v", err)
	}
	first, _ := ResolvedProviderCached(cfg, "codex-max")
	// Mutate the returned copy.
	first.Args = append(first.Args, "MUTATED")
	if first.PermissionModes != nil {
		first.PermissionModes["INJECTED"] = "X"
	}
	// Second lookup should be pristine.
	second, _ := ResolvedProviderCached(cfg, "codex-max")
	for _, arg := range second.Args {
		if arg == "MUTATED" {
			t.Errorf("mutation of Args leaked back into cache: %v", second.Args)
		}
	}
	if _, poisoned := second.PermissionModes["INJECTED"]; poisoned {
		t.Errorf("mutation of PermissionModes leaked back into cache")
	}
}

func TestResolvedProviderCached_MissReturnsFalse(t *testing.T) {
	cfg := &City{
		Providers: map[string]ProviderSpec{},
	}
	_ = BuildResolvedProviderCache(cfg)
	_, ok := ResolvedProviderCached(cfg, "nonexistent")
	if ok {
		t.Error("expected miss for nonexistent provider")
	}
}

func TestResolvedProviderCached_NilCityReturnsFalse(t *testing.T) {
	_, ok := ResolvedProviderCached(nil, "anything")
	if ok {
		t.Error("nil city should produce cache miss")
	}
}

func TestBuildResolvedProviderCache_ValidatesCapsuleDeclaration(t *testing.T) {
	validSchema := []ProviderOption{{
		Key:     "profile",
		Default: "offline",
		Choices: []OptionChoice{{Value: "offline"}},
	}}
	tests := []struct {
		name    string
		capsule ProviderCapsuleSpec
		wantErr string
	}{
		{
			name: "unsupported kind",
			capsule: ProviderCapsuleSpec{
				Kind:          "other",
				ProfileOption: "profile",
				Catalog:       ".gc/services/omnigent/config/profiles.yaml",
			},
			wantErr: `capsule kind "other" is unsupported`,
		},
		{
			name: "missing profile option",
			capsule: ProviderCapsuleSpec{
				Kind:    "omnigent",
				Catalog: ".gc/services/omnigent/config/profiles.yaml",
			},
			wantErr: "capsule profile_option is required",
		},
		{
			name: "undeclared profile option",
			capsule: ProviderCapsuleSpec{
				Kind:          "omnigent",
				ProfileOption: "missing",
				Catalog:       ".gc/services/omnigent/config/profiles.yaml",
			},
			wantErr: `capsule profile_option "missing" is not declared`,
		},
		{
			name: "missing catalog",
			capsule: ProviderCapsuleSpec{
				Kind:          "omnigent",
				ProfileOption: "profile",
			},
			wantErr: "capsule catalog is required",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := &City{Providers: map[string]ProviderSpec{
				"adapter": {
					Command:       "omnigent",
					PromptMode:    "none",
					Capsule:       tt.capsule,
					OptionsSchema: validSchema,
				},
			}}
			err := BuildResolvedProviderCache(cfg)
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("BuildResolvedProviderCache() error = %v, want containing %q", err, tt.wantErr)
			}
		})
	}
}

func TestBuildResolvedProviderCache_PreservesInheritedCapsuleDeclaration(t *testing.T) {
	base := "provider:adapter-base"
	cfg := &City{Providers: map[string]ProviderSpec{
		"adapter-base": {
			Command:    "omnigent",
			PromptMode: "none",
			Capsule: ProviderCapsuleSpec{
				Kind:          "omnigent",
				ProfileOption: "profile",
				Catalog:       ".gc/services/omnigent/config/profiles.yaml",
			},
			OptionsSchema: []ProviderOption{{
				Key:     "profile",
				Default: "offline",
				Choices: []OptionChoice{{Value: "offline"}},
			}},
		},
		"adapter-child": {
			Base:       &base,
			Command:    "wrapper",
			PromptMode: "none",
		},
	}}

	if err := BuildResolvedProviderCache(cfg); err != nil {
		t.Fatalf("BuildResolvedProviderCache() error = %v", err)
	}
	got, ok := ResolvedProviderCached(cfg, "adapter-child")
	if !ok {
		t.Fatal("adapter-child missing from resolved cache")
	}
	if got.Capsule.Kind != "omnigent" || got.Capsule.ProfileOption != "profile" || got.Capsule.Catalog == "" {
		t.Fatalf("resolved capsule = %+v", got.Capsule)
	}
}
