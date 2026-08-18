package omnigent

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestExampleCodexProfileUsesCodexCLISlug(t *testing.T) {
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("resolve test source path")
	}
	path := filepath.Join(filepath.Dir(file), "..", "..", "examples", "omnigent", "agents", "codex.yaml")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(strings.ToLower(string(data)), "model: openai/") {
		t.Fatal("Codex subscription profile must use a bare Codex CLI model slug")
	}
}

func TestExampleClaudeProfilesDoNotReferenceUndefinedProviders(t *testing.T) {
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("resolve test source path")
	}
	agentDir := filepath.Join(filepath.Dir(file), "..", "..", "examples", "omnigent", "agents")
	for _, name := range []string{"claude-primary.yaml", "claude-secondary.yaml"} {
		data, err := os.ReadFile(filepath.Join(agentDir, name))
		if err != nil {
			t.Fatal(err)
		}
		text := strings.ToLower(string(data))
		if strings.Contains(text, "\nauth:\n") {
			t.Errorf("%s references an Omnigent provider absent from local config", name)
		}
		if strings.Contains(text, "model: anthropic/") {
			t.Errorf("%s hardcodes an Anthropic provider despite backend-neutral profile metadata", name)
		}
	}
}

func TestExampleCatalogIsCredentialFreeAndDeclaresPortableProfiles(t *testing.T) {
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("resolve test source path")
	}
	root := filepath.Join(filepath.Dir(file), "..", "..", "examples", "omnigent")
	catalog, err := LoadCatalog(filepath.Join(root, "catalog.example.yaml"))
	if err != nil {
		t.Fatalf("LoadCatalog(example): %v", err)
	}
	profiles := catalog.PublicProfiles()
	if len(profiles) != 4 {
		t.Fatalf("public profiles = %#v", profiles)
	}
	byID := make(map[string]PublicProfile, len(profiles))
	for _, profile := range profiles {
		byID[profile.ID] = profile
		if strings.TrimSpace(profile.Blurb) == "" || profile.PolicyMailEnabled {
			t.Fatalf("profile metadata = %#v", profile)
		}
	}
	if got := byID["offline-mock"].Network; got != "offline" {
		t.Fatalf("offline mock network = %q", got)
	}
	if got := byID["claude-primary"].Chain; !equalStrings(got, []string{"claude-primary", "claude-secondary"}) {
		t.Fatalf("Claude fallback chain = %v", got)
	}
	if byID["claude-primary"].Backend == byID["claude-secondary"].Backend {
		t.Fatal("Claude profiles do not describe distinct backends")
	}

	for _, path := range []string{
		"catalog.example.yaml", "agents/offline-mock.yaml", "agents/codex.yaml",
		"agents/claude-primary.yaml", "agents/claude-secondary.yaml",
	} {
		data, readErr := os.ReadFile(filepath.Join(root, path))
		if readErr != nil {
			t.Fatal(readErr)
		}
		lower := strings.ToLower(string(data))
		for _, forbidden := range []string{"sk-", "bearer ", "api_key:"} {
			if strings.Contains(lower, forbidden) {
				t.Fatalf("example %s contains credential-shaped material %q", path, forbidden)
			}
		}
	}
}
