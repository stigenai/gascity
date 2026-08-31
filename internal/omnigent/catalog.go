// Package omnigent implements the local, provider-specific compatibility
// adapter between Gas City sessions and an externally installed Omnigent
// server.
package omnigent

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

// CatalogBundleSHA256 hashes the validated non-secret catalog and every
// referenced agent definition in stable profile-ID order. Remote providers use
// it as staged-input identity so catalog edits require an explicit relaunch.
func CatalogBundleSHA256(path string) (string, error) {
	catalog, err := LoadCatalog(path)
	if err != nil {
		return "", err
	}
	h := sha256.New()
	writeFile := func(label, filePath string) error {
		data, err := os.ReadFile(filePath)
		if err != nil {
			return fmt.Errorf("read omnigent catalog bundle %s: %w", label, err)
		}
		_, _ = h.Write([]byte(label))
		_, _ = h.Write([]byte{0})
		_, _ = h.Write(data)
		_, _ = h.Write([]byte{0})
		return nil
	}
	if err := writeFile("catalog", path); err != nil {
		return "", err
	}
	ids := make([]string, 0, len(catalog.profiles))
	for id := range catalog.profiles {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		if err := writeFile("profile:"+id, catalog.profiles[id].AgentPath); err != nil {
			return "", err
		}
	}
	return "sha256:" + hex.EncodeToString(h.Sum(nil)), nil
}

const catalogVersion = 1

// ProfileEnvironmentVariable is the optional local fallback used when a
// provider command does not carry an explicit --profile value.
const ProfileEnvironmentVariable = "GC_OMNIGENT_PROFILE"

// ProfileSelectionSource identifies the non-secret configuration layer that
// supplied an opaque profile ID.
type ProfileSelectionSource string

const (
	// ProfileSourceExplicit means provider or agent configuration rendered an
	// explicit profile argument.
	ProfileSourceExplicit ProfileSelectionSource = "explicit"
	// ProfileSourceEnvironment means GC_OMNIGENT_PROFILE supplied the profile
	// because no explicit argument was present.
	ProfileSourceEnvironment ProfileSelectionSource = "environment"
)

// ProfileSelection is one validated opaque profile choice and its source.
type ProfileSelection struct {
	ID     string
	Source ProfileSelectionSource
}

var (
	profileIDPattern       = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_-]{0,62}$`)
	commitPattern          = regexp.MustCompile(`^[0-9a-f]{40}$`)
	digestPattern          = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)
	secretBlurb            = regexp.MustCompile(`(?i)(?:api[_-]?key|token|secret|password)\s*[:=]\s*\S|\bsk-[A-Za-z0-9_-]{8,}`)
	environmentNamePattern = regexp.MustCompile(`^[A-Z_][A-Z0-9_]*$`)
)

// Catalog is a validated local Omnigent profile catalog.
type Catalog struct {
	Version    int
	Pin        Pin
	profiles   map[string]ResolvedProfile
	sourcePath string
}

// Pin identifies the exact externally installed Omnigent executable contract.
type Pin struct {
	Commit         string `yaml:"commit"`
	PackageVersion string `yaml:"package_version"`
	Executable     string `yaml:"executable"`
	SHA256         string `yaml:"sha256"`
}

// ResolvedProfile is a validated profile with an absolute, contained agent
// path. It intentionally contains no credential or agent-spec bytes.
type ResolvedProfile struct {
	ID                  string
	DisplayName         string
	Blurb               string
	Harness             string
	Backend             string
	Network             string
	AgentName           string
	AgentPath           string
	AgentRelativePath   string
	Fallbacks           []string
	Environment         []string
	OptionalEnvironment []string
	SecretReferences    []string
	PolicyMailRecipient string
}

// PublicProfile is the non-secret profile discovery shape.
type PublicProfile struct {
	ID                string   `json:"id"`
	DisplayName       string   `json:"display_name"`
	Blurb             string   `json:"blurb"`
	Harness           string   `json:"harness"`
	Backend           string   `json:"backend"`
	Network           string   `json:"network"`
	Availability      string   `json:"availability"`
	Chain             []string `json:"chain"`
	PolicyMailEnabled bool     `json:"policy_mail_enabled"`
}

type catalogDocument struct {
	Version  int                        `yaml:"version"`
	Omnigent Pin                        `yaml:"omnigent"`
	Profiles map[string]profileDocument `yaml:"profiles"`
}

type profileDocument struct {
	DisplayName         string   `yaml:"display_name"`
	Blurb               string   `yaml:"blurb"`
	Harness             string   `yaml:"harness"`
	Backend             string   `yaml:"backend"`
	Network             string   `yaml:"network"`
	Agent               string   `yaml:"agent"`
	Fallbacks           []string `yaml:"fallbacks"`
	Environment         []string `yaml:"environment"`
	OptionalEnvironment []string `yaml:"optional_environment"`
	SecretReferences    []string `yaml:"secret_references"`
	PolicyMailRecipient string   `yaml:"policy_mail_recipient"`
}

// SelectProfile resolves the provider-rendered profile before the optional
// process environment fallback. It never invents a default; pack, city, and
// agent defaults are expected to flow through the standard typed provider
// option schema as an explicit value.
func SelectProfile(explicit string, lookup func(string) (string, bool)) (ProfileSelection, error) {
	id := strings.TrimSpace(explicit)
	source := ProfileSourceExplicit
	if id == "" {
		if lookup == nil {
			lookup = os.LookupEnv
		}
		value, ok := lookup(ProfileEnvironmentVariable)
		if !ok || strings.TrimSpace(value) == "" {
			return ProfileSelection{}, errors.New("omnigent profile is required; configure the provider profile option or GC_OMNIGENT_PROFILE")
		}
		id = strings.TrimSpace(value)
		source = ProfileSourceEnvironment
	}
	if !profileIDPattern.MatchString(id) {
		return ProfileSelection{}, fmt.Errorf("invalid omnigent profile id %q", id)
	}
	return ProfileSelection{ID: id, Source: source}, nil
}

// LoadCatalog reads and validates a local profile catalog. Agent references
// must resolve to regular files beneath the catalog directory, including after
// symlink evaluation.
func LoadCatalog(path string) (*Catalog, error) {
	path = strings.TrimSpace(path)
	if path == "" {
		return nil, errors.New("omnigent catalog path is required")
	}
	absPath, err := filepath.Abs(path)
	if err != nil {
		return nil, fmt.Errorf("resolve omnigent catalog path: %w", err)
	}
	f, err := os.Open(absPath)
	if err != nil {
		return nil, fmt.Errorf("open omnigent catalog: %w", err)
	}
	defer func() { _ = f.Close() }()

	var doc catalogDocument
	decoder := yaml.NewDecoder(f)
	decoder.KnownFields(true)
	if err := decoder.Decode(&doc); err != nil {
		return nil, fmt.Errorf("decode omnigent catalog: %w", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			return nil, errors.New("decode omnigent catalog: multiple YAML documents are not allowed")
		}
		return nil, fmt.Errorf("decode omnigent catalog trailing document: %w", err)
	}
	if doc.Version != catalogVersion {
		return nil, fmt.Errorf("omnigent catalog version must be %d, got %d", catalogVersion, doc.Version)
	}
	if err := validatePin(doc.Omnigent); err != nil {
		return nil, err
	}
	if len(doc.Profiles) == 0 {
		return nil, errors.New("omnigent catalog profiles must not be empty")
	}

	root := filepath.Dir(absPath)
	resolvedRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		return nil, fmt.Errorf("resolve omnigent catalog directory: %w", err)
	}
	profiles := make(map[string]ResolvedProfile, len(doc.Profiles))
	for id, raw := range doc.Profiles {
		profile, err := resolveProfile(root, resolvedRoot, id, raw)
		if err != nil {
			return nil, err
		}
		profiles[id] = profile
	}
	if err := validateFallbacks(profiles); err != nil {
		return nil, err
	}
	if err := validateProfileSecretReferenceOwnership(profiles); err != nil {
		return nil, err
	}
	resolvedCatalog, err := filepath.EvalSymlinks(absPath)
	if err != nil {
		return nil, fmt.Errorf("resolve omnigent catalog file: %w", err)
	}
	if !pathWithin(resolvedRoot, resolvedCatalog) {
		return nil, errors.New("omnigent catalog symlink target must stay beneath catalog directory")
	}
	return &Catalog{Version: doc.Version, Pin: doc.Omnigent, profiles: profiles, sourcePath: resolvedCatalog}, nil
}

func validatePin(pin Pin) error {
	if !commitPattern.MatchString(pin.Commit) {
		return errors.New("omnigent.commit must be a lowercase 40-hex Git commit")
	}
	if strings.TrimSpace(pin.PackageVersion) == "" {
		return errors.New("omnigent.package_version is required")
	}
	if strings.TrimSpace(pin.Executable) == "" {
		return errors.New("omnigent.executable is required")
	}
	if !digestPattern.MatchString(pin.SHA256) {
		return errors.New("omnigent.sha256 must use sha256:<64 lowercase hex> form")
	}
	return nil
}

func resolveProfile(root, resolvedRoot, id string, raw profileDocument) (ResolvedProfile, error) {
	if !profileIDPattern.MatchString(id) {
		return ResolvedProfile{}, fmt.Errorf("omnigent profile id %q must match %s", id, profileIDPattern)
	}
	profile := ResolvedProfile{
		ID:                  id,
		DisplayName:         strings.TrimSpace(raw.DisplayName),
		Blurb:               strings.TrimSpace(raw.Blurb),
		Harness:             strings.TrimSpace(raw.Harness),
		Backend:             strings.TrimSpace(raw.Backend),
		Network:             strings.TrimSpace(raw.Network),
		Fallbacks:           append([]string(nil), raw.Fallbacks...),
		Environment:         append([]string(nil), raw.Environment...),
		OptionalEnvironment: append([]string(nil), raw.OptionalEnvironment...),
		SecretReferences:    append([]string(nil), raw.SecretReferences...),
		PolicyMailRecipient: strings.TrimSpace(raw.PolicyMailRecipient),
	}
	if profile.DisplayName == "" {
		return ResolvedProfile{}, fmt.Errorf("omnigent profile %q: display_name is required", id)
	}
	if sensitiveExternalText.MatchString(profile.DisplayName) {
		return ResolvedProfile{}, fmt.Errorf("omnigent profile %q: display_name appears to contain secret material", id)
	}
	if profile.Blurb == "" {
		return ResolvedProfile{}, fmt.Errorf("omnigent profile %q: blurb is required", id)
	}
	if len(profile.Blurb) > 240 {
		return ResolvedProfile{}, fmt.Errorf("omnigent profile %q: blurb exceeds 240 bytes", id)
	}
	if secretBlurb.MatchString(profile.Blurb) || sensitiveExternalText.MatchString(profile.Blurb) {
		return ResolvedProfile{}, fmt.Errorf("omnigent profile %q: blurb appears to contain secret material", id)
	}
	if profile.PolicyMailRecipient != "" {
		if len(profile.PolicyMailRecipient) > 128 || strings.IndexFunc(profile.PolicyMailRecipient, func(r rune) bool { return r < 0x21 || r == 0x7f }) >= 0 {
			return ResolvedProfile{}, fmt.Errorf("omnigent profile %q: policy_mail_recipient must be a non-whitespace local identity of at most 128 bytes", id)
		}
	}
	switch profile.Harness {
	case "claude-sdk", "codex":
	default:
		return ResolvedProfile{}, fmt.Errorf("omnigent profile %q: harness must be claude-sdk or codex, got %q", id, profile.Harness)
	}
	if profile.Backend == "" {
		return ResolvedProfile{}, fmt.Errorf("omnigent profile %q: backend is required", id)
	}
	if sensitiveExternalText.MatchString(profile.Backend) {
		return ResolvedProfile{}, fmt.Errorf("omnigent profile %q: backend appears to contain secret material", id)
	}
	switch profile.Network {
	case "offline", "external-model":
	default:
		return ResolvedProfile{}, fmt.Errorf("omnigent profile %q: network must be offline or external-model, got %q", id, profile.Network)
	}
	seenEnvironment := make(map[string]string, len(profile.Environment)+len(profile.OptionalEnvironment))
	validateEnvironment := func(kind string, names []string) error {
		for i, name := range names {
			name = strings.TrimSpace(name)
			if !environmentNamePattern.MatchString(name) {
				return fmt.Errorf("omnigent profile %q: %s[%d] is not a valid uppercase environment name", id, kind, i)
			}
			if unsafeProfileEnvironmentName(name) {
				return fmt.Errorf("omnigent profile %q: %s name %q is managed or unsafe", id, kind, name)
			}
			if previous := seenEnvironment[name]; previous != "" {
				if previous != kind {
					return fmt.Errorf("omnigent profile %q: environment name %q appears in both environment and optional_environment", id, name)
				}
				return fmt.Errorf("omnigent profile %q: duplicate %s name %q", id, kind, name)
			}
			seenEnvironment[name] = kind
			names[i] = name
		}
		return nil
	}
	if err := validateEnvironment("environment", profile.Environment); err != nil {
		return ResolvedProfile{}, err
	}
	if err := validateEnvironment("optional_environment", profile.OptionalEnvironment); err != nil {
		return ResolvedProfile{}, err
	}
	seenSecretReferences := make(map[string]bool, len(profile.SecretReferences))
	for i, id := range profile.SecretReferences {
		id = strings.TrimSpace(id)
		if !profileIDPattern.MatchString(id) {
			return ResolvedProfile{}, fmt.Errorf("omnigent profile %q: secret_references[%d] is invalid", profile.ID, i)
		}
		if seenSecretReferences[id] {
			return ResolvedProfile{}, fmt.Errorf("omnigent profile %q: duplicate secret reference %q", profile.ID, id)
		}
		seenSecretReferences[id] = true
		profile.SecretReferences[i] = id
	}

	agentRef := strings.TrimSpace(raw.Agent)
	if agentRef == "" {
		return ResolvedProfile{}, fmt.Errorf("omnigent profile %q: agent is required", id)
	}
	if filepath.IsAbs(agentRef) {
		return ResolvedProfile{}, fmt.Errorf("omnigent profile %q: agent path must be relative to catalog directory", id)
	}
	agentPath := filepath.Clean(filepath.Join(root, filepath.FromSlash(agentRef)))
	if !pathWithin(root, agentPath) {
		return ResolvedProfile{}, fmt.Errorf("omnigent profile %q: agent path must stay beneath catalog directory", id)
	}
	resolvedAgent, err := filepath.EvalSymlinks(agentPath)
	if err != nil {
		return ResolvedProfile{}, fmt.Errorf("omnigent profile %q: resolve agent path: %w", id, err)
	}
	if !pathWithin(resolvedRoot, resolvedAgent) {
		return ResolvedProfile{}, fmt.Errorf("omnigent profile %q: agent symlink target must stay beneath catalog directory", id)
	}
	info, err := os.Stat(resolvedAgent)
	if err != nil {
		return ResolvedProfile{}, fmt.Errorf("omnigent profile %q: stat agent: %w", id, err)
	}
	if !info.Mode().IsRegular() {
		return ResolvedProfile{}, fmt.Errorf("omnigent profile %q: agent must be a regular standalone YAML file", id)
	}
	if err := validateLocalModeYAML(resolvedAgent, "agent"); err != nil {
		return ResolvedProfile{}, fmt.Errorf("omnigent profile %q: %w", id, err)
	}
	agentName, err := readAgentName(resolvedAgent)
	if err != nil {
		return ResolvedProfile{}, fmt.Errorf("omnigent profile %q: %w", id, err)
	}
	profile.AgentName = agentName
	profile.AgentPath = resolvedAgent
	profile.AgentRelativePath = filepath.ToSlash(filepath.Clean(filepath.FromSlash(agentRef)))
	return profile, nil
}

func unsafeProfileEnvironmentName(name string) bool {
	return strings.HasPrefix(name, "OMNIGENT_") ||
		strings.HasPrefix(name, "GC_SERVICE_") ||
		strings.HasPrefix(name, "DYLD_") ||
		strings.HasPrefix(name, "LD_") ||
		strings.HasPrefix(name, "PYTHON") ||
		strings.HasPrefix(name, "UV_") ||
		name == "VIRTUAL_ENV"
}

func readAgentName(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", fmt.Errorf("open agent: %w", err)
	}
	defer func() { _ = f.Close() }()
	var header struct {
		Name         string    `yaml:"name"`
		Prompt       yaml.Node `yaml:"prompt"`
		Instructions yaml.Node `yaml:"instructions"`
		SpecVersion  yaml.Node `yaml:"spec_version"`
	}
	if err := yaml.NewDecoder(f).Decode(&header); err != nil {
		return "", fmt.Errorf("decode agent name: %w", err)
	}
	name := strings.TrimSpace(header.Name)
	if !profileIDPattern.MatchString(name) {
		return "", fmt.Errorf("agent name %q must match %s", name, profileIDPattern)
	}
	if header.SpecVersion.Kind != 0 {
		return "", errors.New("standalone agent YAML must omit spec_version so the pinned Omnigent single-file loader accepts it")
	}
	if header.Prompt.Kind == 0 && header.Instructions.Kind == 0 {
		return "", errors.New("standalone agent YAML requires prompt or instructions")
	}
	return name, nil
}

func pathWithin(root, path string) bool {
	rel, err := filepath.Rel(root, path)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) && !filepath.IsAbs(rel)
}

func validateFallbacks(profiles map[string]ResolvedProfile) error {
	for id, profile := range profiles {
		seenDirect := make(map[string]bool, len(profile.Fallbacks))
		for i, fallback := range profile.Fallbacks {
			fallback = strings.TrimSpace(fallback)
			if fallback == "" {
				return fmt.Errorf("omnigent profile %q: fallback[%d] is empty", id, i)
			}
			if seenDirect[fallback] {
				return fmt.Errorf("omnigent profile %q: duplicate fallback %q", id, fallback)
			}
			seenDirect[fallback] = true
			target, ok := profiles[fallback]
			if !ok {
				return fmt.Errorf("omnigent profile %q: unknown fallback %q", id, fallback)
			}
			if target.Harness != profile.Harness {
				return fmt.Errorf("omnigent profile %q: fallback %q changes harness from %q to %q", id, fallback, profile.Harness, target.Harness)
			}
		}
	}
	visiting := make(map[string]bool, len(profiles))
	visited := make(map[string]bool, len(profiles))
	var visit func(string) error
	visit = func(id string) error {
		if visiting[id] {
			return fmt.Errorf("omnigent profile %q: fallback cycle detected", id)
		}
		if visited[id] {
			return nil
		}
		visiting[id] = true
		for _, next := range profiles[id].Fallbacks {
			if err := visit(next); err != nil {
				return err
			}
		}
		delete(visiting, id)
		visited[id] = true
		return nil
	}
	ids := sortedProfileIDs(profiles)
	for _, id := range ids {
		if err := visit(id); err != nil {
			return err
		}
	}
	return nil
}

func validateProfileSecretReferenceOwnership(profiles map[string]ResolvedProfile) error {
	owners := make(map[string]string)
	for _, profileID := range sortedProfileIDs(profiles) {
		for _, referenceID := range profiles[profileID].SecretReferences {
			if owner := owners[referenceID]; owner != "" {
				return fmt.Errorf("omnigent secret reference %q is shared by profiles %q and %q", referenceID, owner, profileID)
			}
			owners[referenceID] = profileID
		}
	}
	return nil
}

// Profile returns one validated profile. The returned value owns its fallback
// slice and may be mutated by the caller.
func (c *Catalog) Profile(id string) (ResolvedProfile, bool) {
	if c == nil {
		return ResolvedProfile{}, false
	}
	profile, ok := c.profiles[id]
	profile.Fallbacks = append([]string(nil), profile.Fallbacks...)
	profile.Environment = append([]string(nil), profile.Environment...)
	profile.OptionalEnvironment = append([]string(nil), profile.OptionalEnvironment...)
	profile.SecretReferences = append([]string(nil), profile.SecretReferences...)
	return profile, ok
}

// Chain returns the deterministic depth-first fallback chain for a profile.
// Duplicate descendants are emitted once at their first configured position.
func (c *Catalog) Chain(id string) ([]ResolvedProfile, error) {
	root, ok := c.Profile(id)
	if !ok {
		return nil, fmt.Errorf("unknown omnigent profile %q", id)
	}
	out := make([]ResolvedProfile, 0, 1+len(root.Fallbacks))
	seen := make(map[string]bool)
	var appendProfile func(ResolvedProfile)
	appendProfile = func(profile ResolvedProfile) {
		if seen[profile.ID] {
			return
		}
		seen[profile.ID] = true
		out = append(out, profile)
		for _, nextID := range profile.Fallbacks {
			next, _ := c.Profile(nextID)
			appendProfile(next)
		}
	}
	appendProfile(root)
	return out, nil
}

// PublicProfiles returns a stable, non-secret discovery view sorted by ID.
func (c *Catalog) PublicProfiles() []PublicProfile {
	return c.PublicProfilesWithEnvironment(os.LookupEnv)
}

// PublicProfilesWithEnvironment returns stable non-secret discovery metadata.
// Availability means every required environment reference is present and
// nonempty. Optional references do not affect availability. Neither names nor
// values enter the public shape.
func (c *Catalog) PublicProfilesWithEnvironment(lookup func(string) (string, bool)) []PublicProfile {
	if c == nil {
		return nil
	}
	if lookup == nil {
		lookup = func(string) (string, bool) { return "", false }
	}
	ids := sortedProfileIDs(c.profiles)
	out := make([]PublicProfile, 0, len(ids))
	for _, id := range ids {
		profile := c.profiles[id]
		chain, _ := c.Chain(id)
		chainIDs := make([]string, 0, len(chain))
		for _, member := range chain {
			chainIDs = append(chainIDs, member.ID)
		}
		availability := "available"
		for _, name := range profile.Environment {
			value, ok := lookup(name)
			if !ok || value == "" {
				availability = "unavailable"
				break
			}
		}
		out = append(out, PublicProfile{
			ID:                profile.ID,
			DisplayName:       profile.DisplayName,
			Blurb:             profile.Blurb,
			Harness:           profile.Harness,
			Backend:           profile.Backend,
			Network:           profile.Network,
			Availability:      availability,
			Chain:             chainIDs,
			PolicyMailEnabled: profile.PolicyMailRecipient != "",
		})
	}
	return out
}

// EnvironmentNames returns the sorted union of required and optional
// process-only environment references. Callers use it to construct a minimal
// child environment from the names that are actually present.
func (c *Catalog) EnvironmentNames() []string {
	if c == nil {
		return nil
	}
	set := make(map[string]bool)
	for _, profile := range c.profiles {
		for _, name := range profile.Environment {
			set[name] = true
		}
		for _, name := range profile.OptionalEnvironment {
			set[name] = true
		}
	}
	names := make([]string, 0, len(set))
	for name := range set {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

func sortedProfileIDs(profiles map[string]ResolvedProfile) []string {
	ids := make([]string, 0, len(profiles))
	for id := range profiles {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}
