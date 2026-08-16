package omnigent

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoadCatalogResolvesOrderedProfilesWithoutAgentContents(t *testing.T) {
	root := t.TempDir()
	writeCatalogTestFile(t, root, "agents/codex.yaml", "name: codex-local\nsecret: $CODEX_TOKEN\n")
	writeCatalogTestFile(t, root, "agents/claude-a.yaml", "name: claude-primary\nsecret: $CLAUDE_A_TOKEN\n")
	writeCatalogTestFile(t, root, "agents/claude-b.yaml", "name: claude-secondary\nsecret: $CLAUDE_B_TOKEN\n")
	catalogPath := writeCatalogTestFile(t, root, "catalog.yaml", `version: 1
omnigent:
  commit: 2aba5079d4d3a2a84d8c9927884fc4b8ce0eeecc
  package_version: 0.10.0.dev0
  executable: omnigent
  sha256: sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa
profiles:
  codex-local:
    display_name: Codex
    blurb: Local Codex subscription.
    harness: codex
    backend: openai-subscription
    network: external-model
    agent: agents/codex.yaml
  claude-primary:
    display_name: Claude primary
    blurb: Primary compatible gateway.
    harness: claude-sdk
    backend: primary-gateway
    network: external-model
    agent: agents/claude-a.yaml
    fallbacks: [claude-secondary]
  claude-secondary:
    display_name: Claude secondary
    blurb: Backup compatible gateway.
    harness: claude-sdk
    backend: backup-gateway
    network: external-model
    agent: agents/claude-b.yaml
`)

	catalog, err := LoadCatalog(catalogPath)
	if err != nil {
		t.Fatalf("LoadCatalog: %v", err)
	}
	if catalog.Version != 1 {
		t.Fatalf("Version = %d, want 1", catalog.Version)
	}
	primary, ok := catalog.Profile("claude-primary")
	if !ok {
		t.Fatal("Profile(claude-primary) not found")
	}
	wantAgent, err := filepath.EvalSymlinks(filepath.Join(root, "agents", "claude-a.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if primary.AgentPath != wantAgent {
		t.Fatalf("AgentPath = %q, want %q", primary.AgentPath, wantAgent)
	}
	chain, err := catalog.Chain("claude-primary")
	if err != nil {
		t.Fatalf("Chain: %v", err)
	}
	if got, want := profileIDs(chain), []string{"claude-primary", "claude-secondary"}; !equalStrings(got, want) {
		t.Fatalf("Chain IDs = %v, want %v", got, want)
	}
	public := catalog.PublicProfiles()
	if len(public) != 3 {
		t.Fatalf("PublicProfiles len = %d, want 3", len(public))
	}
	for _, profile := range public {
		if strings.Contains(profile.Blurb, "TOKEN") {
			t.Fatalf("public profile leaked agent contents: %#v", profile)
		}
	}
}

func TestLoadCatalogRejectsInvalidContracts(t *testing.T) {
	tests := []struct {
		name    string
		catalog string
		want    string
	}{
		{
			name: "unsupported version",
			catalog: baseCatalogYAML() + `profiles:
  p:
    display_name: P
    blurb: profile
    harness: codex
    backend: local
    network: offline
    agent: agent.yaml
`,
			want: "version must be 1",
		},
		{
			name: "bad id",
			catalog: validCatalogHeader() + `profiles:
  ../p:
    display_name: P
    blurb: profile
    harness: codex
    backend: local
    network: offline
    agent: agent.yaml
`,
			want: "profile id",
		},
		{
			name: "unknown fallback",
			catalog: validCatalogHeader() + `profiles:
  p:
    display_name: P
    blurb: profile
    harness: codex
    backend: local
    network: offline
    agent: agent.yaml
    fallbacks: [missing]
`,
			want: `unknown fallback "missing"`,
		},
		{
			name: "cycle",
			catalog: validCatalogHeader() + `profiles:
  a:
    display_name: A
    blurb: profile a
    harness: claude-sdk
    backend: a
    network: offline
    agent: agent.yaml
    fallbacks: [b]
  b:
    display_name: B
    blurb: profile b
    harness: claude-sdk
    backend: b
    network: offline
    agent: agent.yaml
    fallbacks: [a]
`,
			want: "fallback cycle",
		},
		{
			name: "cross harness",
			catalog: validCatalogHeader() + `profiles:
  a:
    display_name: A
    blurb: profile a
    harness: claude-sdk
    backend: a
    network: offline
    agent: agent.yaml
    fallbacks: [b]
  b:
    display_name: B
    blurb: profile b
    harness: codex
    backend: b
    network: offline
    agent: agent.yaml
`,
			want: "changes harness",
		},
		{
			name: "path escape",
			catalog: validCatalogHeader() + `profiles:
  p:
    display_name: P
    blurb: profile
    harness: codex
    backend: local
    network: offline
    agent: ../agent.yaml
`,
			want: "must stay beneath catalog directory",
		},
		{
			name: "secret-like blurb",
			catalog: validCatalogHeader() + `profiles:
  p:
    display_name: P
    blurb: token=sk-live-secret
    harness: codex
    backend: local
    network: offline
    agent: agent.yaml
`,
			want: "blurb appears to contain secret material",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			root := t.TempDir()
			writeCatalogTestFile(t, root, "agent.yaml", "name: fixture\n")
			path := writeCatalogTestFile(t, root, "catalog.yaml", tt.catalog)
			_, err := LoadCatalog(path)
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("LoadCatalog error = %v, want substring %q", err, tt.want)
			}
		})
	}
}

func TestLoadCatalogRejectsSymlinkEscape(t *testing.T) {
	root := t.TempDir()
	outside := filepath.Join(t.TempDir(), "agent.yaml")
	if err := os.WriteFile(outside, []byte("name: outside\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "agent.yaml")); err != nil {
		t.Fatal(err)
	}
	path := writeCatalogTestFile(t, root, "catalog.yaml", validCatalogHeader()+`profiles:
  p:
    display_name: P
    blurb: profile
    harness: codex
    backend: local
    network: offline
    agent: agent.yaml
`)
	_, err := LoadCatalog(path)
	if err == nil || !strings.Contains(err.Error(), "symlink target must stay beneath catalog directory") {
		t.Fatalf("LoadCatalog error = %v, want symlink escape", err)
	}
}

func TestCatalogEnvironmentAllowlistControlsAvailabilityWithoutPublicNamesOrValues(t *testing.T) {
	root := t.TempDir()
	writeCatalogTestFile(t, root, "agents/agent.yaml", "name: claude-profile\n")
	path := writeCatalogTestFile(t, root, "catalog.yaml", validCatalogHeader()+`profiles:
  claude-profile:
    display_name: Claude profile
    blurb: Compatible backend with operator-selected authentication.
    harness: claude-sdk
    backend: compatible-gateway
    network: external-model
    agent: agents/agent.yaml
    environment: [HOME, CLAUDE_PROFILE_TOKEN]
`)
	catalog, err := LoadCatalog(path)
	if err != nil {
		t.Fatal(err)
	}
	profile, ok := catalog.Profile("claude-profile")
	if !ok || !equalStrings(profile.Environment, []string{"HOME", "CLAUDE_PROFILE_TOKEN"}) {
		t.Fatalf("profile environment = %#v", profile.Environment)
	}
	missing := catalog.PublicProfilesWithEnvironment(func(name string) (string, bool) {
		if name == "HOME" {
			return "/operator", true
		}
		return "", false
	})
	if len(missing) != 1 || missing[0].Availability != "unavailable" {
		t.Fatalf("missing availability = %#v", missing)
	}
	available := catalog.PublicProfilesWithEnvironment(func(string) (string, bool) { return "configured", true })
	if available[0].Availability != "available" {
		t.Fatalf("available profile = %#v", available[0])
	}
	encoded, err := json.Marshal(available)
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{"HOME", "CLAUDE_PROFILE_TOKEN", "configured"} {
		if strings.Contains(string(encoded), forbidden) {
			t.Fatalf("public profiles leaked %q: %s", forbidden, encoded)
		}
	}
	if got := catalog.EnvironmentNames(); !equalStrings(got, []string{"CLAUDE_PROFILE_TOKEN", "HOME"}) {
		t.Fatalf("EnvironmentNames = %v", got)
	}
}

func TestLoadCatalogRejectsUnsafeEnvironmentAllowlist(t *testing.T) {
	for _, tt := range []struct {
		name string
		env  string
	}{
		{name: "invalid", env: "NOT-AN-ENV"},
		{name: "duplicate", env: "HOME, HOME"},
		{name: "managed omnigent", env: "OMNIGENT_DATA_DIR"},
		{name: "managed service", env: "GC_SERVICE_SOCKET"},
		{name: "loader injection", env: "DYLD_INSERT_LIBRARIES"},
		{name: "python injection", env: "PYTHONPATH"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			root := t.TempDir()
			writeCatalogTestFile(t, root, "agent.yaml", "name: fixture\n")
			path := writeCatalogTestFile(t, root, "catalog.yaml", validCatalogHeader()+`profiles:
  p:
    display_name: P
    blurb: Profile metadata.
    harness: codex
    backend: local
    network: offline
    agent: agent.yaml
    environment: [`+tt.env+`]
`)
			if _, err := LoadCatalog(path); err == nil || !strings.Contains(err.Error(), "environment") {
				t.Fatalf("LoadCatalog error = %v, want environment rejection", err)
			}
		})
	}
}

func TestSelectProfilePrecedenceIsExplicitThenEnvironment(t *testing.T) {
	lookup := func(name string) (string, bool) {
		if name != ProfileEnvironmentVariable {
			t.Fatalf("lookup name = %q", name)
		}
		return "claude-secondary", true
	}
	selection, err := SelectProfile("claude-primary", lookup)
	if err != nil || selection.ID != "claude-primary" || selection.Source != ProfileSourceExplicit {
		t.Fatalf("explicit selection = %#v, %v", selection, err)
	}
	selection, err = SelectProfile("", lookup)
	if err != nil || selection.ID != "claude-secondary" || selection.Source != ProfileSourceEnvironment {
		t.Fatalf("environment selection = %#v, %v", selection, err)
	}
	if _, err := SelectProfile("", func(string) (string, bool) { return "", false }); err == nil || !strings.Contains(err.Error(), "profile is required") {
		t.Fatalf("missing selection error = %v", err)
	}
	if _, err := SelectProfile("../unsafe", nil); err == nil || !strings.Contains(err.Error(), "invalid") {
		t.Fatalf("invalid selection error = %v", err)
	}
}

func validCatalogHeader() string {
	return `version: 1
omnigent:
  commit: 2aba5079d4d3a2a84d8c9927884fc4b8ce0eeecc
  package_version: 0.10.0.dev0
  executable: omnigent
  sha256: sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa
`
}

func baseCatalogYAML() string {
	return strings.Replace(validCatalogHeader(), "version: 1", "version: 2", 1)
}

func writeCatalogTestFile(t *testing.T, root, rel, body string) string {
	t.Helper()
	path := filepath.Join(root, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func profileIDs(profiles []ResolvedProfile) []string {
	ids := make([]string, 0, len(profiles))
	for _, profile := range profiles {
		ids = append(ids, profile.ID)
	}
	return ids
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
