package omnigent

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/gastownhall/gascity/internal/runtime"
)

const (
	// CapsuleStateRoot is the fixed capsule-internal durable mount. Providers
	// own its host/PVC backing; workspace content never chooses it.
	CapsuleStateRoot = "/var/lib/gascity/omnigent"
	// CapsuleSocketPath is the fixed private supervisor socket inside a Place.
	CapsuleSocketPath = "/run/gascity/omnigent/sidecar.sock"
	// CapsuleCatalogPath is the fixed immutable staged catalog destination.
	CapsuleCatalogPath = "/etc/gascity/omnigent/profiles.yaml"
)

// AttachmentLocation selects the only two supported Omnigent control
// boundaries. There is deliberately no automatic or managed-host mode.
type AttachmentLocation string

const (
	// AttachmentLocationController uses the existing city-local supervised
	// service and is valid only for local runtime placement.
	AttachmentLocationController AttachmentLocation = "controller"
	// AttachmentLocationCapsule uses a private Unix socket in the selected
	// remote Place.
	AttachmentLocationCapsule AttachmentLocation = "capsule"
)

// AttachmentLaunchInput contains the fully resolved, non-secret inputs for an
// Omnigent attachment. HybridRouteSet is required for hybrid so resolution
// cannot guess whether the concrete session is local or remote.
type AttachmentLaunchInput struct {
	Runtime          string
	HybridRouteSet   bool
	HybridRemote     bool
	ProfileID        string
	Catalog          *Catalog
	Workspace        string
	CityScope        string
	SessionID        string
	StateRoot        string
	SocketPath       string
	CatalogPath      string
	CatalogSHA256    string
	Pin              Pin
	SecretReferences []runtime.SecretReference
}

// AttachmentLaunchPlan is the deterministic launch boundary consumed by the
// command resolver and capsule supervisor. SecretReferences contain identities
// only; selected provider source values are never represented.
type AttachmentLaunchPlan struct {
	Location           AttachmentLocation
	Runtime            string
	ProfileID          string
	Workspace          string
	StateRoot          string
	SocketPath         string
	CatalogPath        string
	CatalogSHA256      string
	CatalogInputs      []runtime.CapsuleInput
	CapsuleKey         runtime.CapsuleKey
	Pin                Pin
	SecretProvider     runtime.SecretProvider
	Network            runtime.CapsuleNetworkMode
	SecretReferences   []runtime.SecretReference
	ProfileCredentials []ProfileCredentialProjection
}

// RemoteAttachmentStatus is the public, non-secret projection of a capsule
// launch. Provider-confined environment names, credential paths, state paths,
// catalog paths, and secret source identifiers are intentionally absent.
type RemoteAttachmentStatus struct {
	Mode                 AttachmentLocation     `json:"mode"`
	Runtime              string                 `json:"runtime"`
	SecretProvider       runtime.SecretProvider `json:"secret_provider"`
	SessionID            string                 `json:"session_id"`
	CapsuleFingerprint   string                 `json:"capsule_fingerprint"`
	LaunchFingerprint    string                 `json:"launch_fingerprint"`
	ProfileID            string                 `json:"profile_id"`
	Harness              string                 `json:"harness"`
	Backend              string                 `json:"backend"`
	Blurb                string                 `json:"blurb"`
	CredentialReferences int                    `json:"credential_references"`
	PinCommit            string                 `json:"pin_commit"`
	PinPackageVersion    string                 `json:"pin_package_version"`
	PinSHA256            string                 `json:"pin_sha256"`
}

// RemoteStatus returns the only launch-plan shape suitable for public status,
// CLI JSON, events, metrics, and Beads metadata.
func (p AttachmentLaunchPlan) RemoteStatus() RemoteAttachmentStatus {
	status := RemoteAttachmentStatus{
		Mode: p.Location, Runtime: remoteRuntimeKind(p.Runtime), SecretProvider: p.SecretProvider,
		SessionID: p.CapsuleKey.SessionID, CapsuleFingerprint: p.CapsuleKey.Digest,
		LaunchFingerprint: p.Fingerprint(), ProfileID: p.ProfileID,
		CredentialReferences: len(p.SecretReferences), PinCommit: p.Pin.Commit,
		PinPackageVersion: p.Pin.PackageVersion, PinSHA256: p.Pin.SHA256,
	}
	if len(p.ProfileCredentials) > 0 {
		profile := p.ProfileCredentials[0]
		status.Harness = profile.Harness
		status.Backend = profile.Backend
		status.Blurb = profile.Blurb
	}
	return status
}

func remoteRuntimeKind(value string) string {
	value = strings.TrimSpace(value)
	if kind, _, ok := strings.Cut(value, ":"); ok {
		return kind
	}
	return value
}

// ResolveAttachmentLaunchPlan maps an already selected Gas City runtime onto
// exactly one local or capsule boundary. Capability failures never reroute.
func ResolveAttachmentLaunchPlan(input AttachmentLaunchInput) (AttachmentLaunchPlan, error) {
	runtimeName := strings.TrimSpace(input.Runtime)
	location, provider, err := attachmentPlacement(runtimeName, input.HybridRouteSet, input.HybridRemote)
	if err != nil {
		return AttachmentLaunchPlan{}, err
	}
	workspace, err := cleanAbsolutePath("workspace", input.Workspace)
	if err != nil {
		return AttachmentLaunchPlan{}, err
	}
	plan := AttachmentLaunchPlan{Location: location, Runtime: runtimeName, Workspace: workspace}
	if location == AttachmentLocationController {
		if len(input.SecretReferences) != 0 {
			return AttachmentLaunchPlan{}, errors.New("local Omnigent attachment cannot resolve remote secret references")
		}
		return plan, nil
	}
	if input.StateRoot != CapsuleStateRoot || input.SocketPath != CapsuleSocketPath || input.CatalogPath != CapsuleCatalogPath {
		return AttachmentLaunchPlan{}, errors.New("remote Omnigent capsule paths must use the fixed state, socket, and catalog locations")
	}
	if err := validatePin(input.Pin); err != nil {
		return AttachmentLaunchPlan{}, err
	}
	if !digestPattern.MatchString(input.CatalogSHA256) {
		return AttachmentLaunchPlan{}, errors.New("omnigent capsule catalog digest must use sha256:<64 lowercase hex> form")
	}
	key, err := runtime.NewCapsuleKey(input.CityScope, input.SessionID)
	if err != nil {
		return AttachmentLaunchPlan{}, err
	}
	if input.Catalog == nil {
		return AttachmentLaunchPlan{}, errors.New("remote Omnigent attachment requires a validated profile catalog")
	}
	profileID := strings.TrimSpace(input.ProfileID)
	if !profileIDPattern.MatchString(profileID) {
		return AttachmentLaunchPlan{}, fmt.Errorf("invalid omnigent profile id %q", input.ProfileID)
	}
	profileCredentials, err := input.Catalog.ProjectProfileCredentials(profileID, provider, input.SecretReferences)
	if err != nil {
		return AttachmentLaunchPlan{}, err
	}
	selected := flattenProfileCredentialReferences(profileCredentials)
	catalogInputs, err := input.Catalog.capsuleInputs(filepath.Base(CapsuleCatalogPath))
	if err != nil {
		return AttachmentLaunchPlan{}, err
	}
	plan.StateRoot = CapsuleStateRoot
	plan.SocketPath = CapsuleSocketPath
	plan.CatalogPath = CapsuleCatalogPath
	plan.CatalogSHA256 = input.CatalogSHA256
	plan.CatalogInputs = catalogInputs
	plan.CapsuleKey = key
	plan.Pin = input.Pin
	plan.ProfileID = profileID
	plan.Network = runtime.CapsuleNetworkMode(profileCredentials[0].Network)
	plan.SecretProvider = provider
	plan.SecretReferences = selected
	plan.ProfileCredentials = profileCredentials
	return plan, nil
}

func flattenProfileCredentialReferences(projections []ProfileCredentialProjection) []runtime.SecretReference {
	var refs []runtime.SecretReference
	for _, projection := range projections {
		refs = append(refs, projection.References...)
	}
	return refs
}

func attachmentPlacement(runtimeName string, hybridRouteSet, hybridRemote bool) (AttachmentLocation, runtime.SecretProvider, error) {
	switch {
	case runtimeName == "", runtimeName == "tmux", runtimeName == "herdr":
		return AttachmentLocationController, "", nil
	case runtimeName == "k8s":
		return AttachmentLocationCapsule, runtime.SecretProviderKubernetes, nil
	case strings.HasPrefix(runtimeName, "ssh:") && strings.TrimSpace(strings.TrimPrefix(runtimeName, "ssh:")) != "":
		return AttachmentLocationCapsule, runtime.SecretProviderSSH, nil
	case runtimeName == "hybrid":
		if !hybridRouteSet {
			return "", "", errors.New("hybrid Omnigent placement requires an explicit resolved route")
		}
		if hybridRemote {
			return AttachmentLocationCapsule, runtime.SecretProviderKubernetes, nil
		}
		return AttachmentLocationController, "", nil
	default:
		return "", "", fmt.Errorf("omnigent attachment does not support runtime %q", runtimeName)
	}
}

func cleanAbsolutePath(kind, value string) (string, error) {
	value = strings.TrimSpace(value)
	if value == "" || !filepath.IsAbs(value) || filepath.Clean(value) != value {
		return "", fmt.Errorf("omnigent attachment %s must be a clean absolute path", kind)
	}
	return value, nil
}

// CommandArgs returns the shell-free argv prefix for this plan. Provider
// options such as the non-secret profile ID are appended by config resolution.
func (p AttachmentLaunchPlan) CommandArgs() []string {
	args := []string{"gc", "omnigent", "attach", "--mode", string(p.Location)}
	if p.Location == AttachmentLocationCapsule {
		args = append(args, "--socket", p.SocketPath, "--state-root", p.StateRoot, "--catalog", p.CatalogPath)
	}
	return args
}

// RuntimeCapsuleConfig projects a resolved remote attachment and provider-owned
// durable allocation into the shared runtime placement record. The catalog
// resource ID remains opaque so Kubernetes and SSH can use different staging
// mechanisms without adding provider details to the Omnigent adapter.
func (p AttachmentLaunchPlan) RuntimeCapsuleConfig(state runtime.CapsuleStateReference, catalogResourceID string) (*runtime.CapsuleLaunchConfig, error) {
	if p.Location != AttachmentLocationCapsule {
		return nil, errors.New("runtime capsule config requires capsule attachment location")
	}
	if state.Key != p.CapsuleKey || state.MountPath != p.StateRoot {
		return nil, fmt.Errorf("%w: runtime capsule state does not match attachment plan", runtime.ErrCapsuleStateConflict)
	}
	command := append(p.CommandArgs(), "--profile", p.ProfileID)
	capsule := &runtime.CapsuleLaunchConfig{
		Key: p.CapsuleKey, State: state, Command: command,
		RunRoot: filepath.Dir(p.SocketPath), SocketPath: p.SocketPath,
		CatalogResourceID: strings.TrimSpace(catalogResourceID),
		CatalogMountPath:  filepath.Dir(p.CatalogPath), CatalogSHA256: p.CatalogSHA256,
		CatalogInputs: append([]runtime.CapsuleInput(nil), p.CatalogInputs...),
		ExecutablePin: runtime.CapsuleExecutablePin{
			Executable: p.Pin.Executable, PackageVersion: p.Pin.PackageVersion,
			Commit: p.Pin.Commit, SHA256: p.Pin.SHA256,
		},
		Network: p.Network,
	}
	if err := capsule.Validate(); err != nil {
		return nil, err
	}
	return capsule, nil
}

// Fingerprint returns a versioned non-secret identity for provisioning drift.
func (p AttachmentLaunchPlan) Fingerprint() string {
	h := sha256.New()
	for _, value := range []string{
		string(p.Location), p.Runtime, p.ProfileID, p.Workspace, p.StateRoot, p.SocketPath, p.CatalogPath,
		p.CapsuleKey.Digest, p.CatalogSHA256, p.Pin.Commit, p.Pin.PackageVersion, p.Pin.Executable, p.Pin.SHA256,
		string(p.SecretProvider), string(p.Network), runtime.ProvisionFingerprint(runtime.Config{SecretReferences: p.SecretReferences}),
	} {
		_, _ = h.Write([]byte(value))
		_, _ = h.Write([]byte{0})
	}
	for _, projection := range p.ProfileCredentials {
		_, _ = h.Write([]byte(projection.ProfileID))
		_, _ = h.Write([]byte{0})
		for _, ref := range projection.References {
			_, _ = h.Write([]byte(ref.ID))
			_, _ = h.Write([]byte{0})
		}
	}
	for _, input := range p.CatalogInputs {
		_, _ = h.Write([]byte(input.RelativePath))
		_, _ = h.Write([]byte{0})
		_, _ = h.Write([]byte(input.SHA256))
		_, _ = h.Write([]byte{0})
	}
	return "v1:" + hex.EncodeToString(h.Sum(nil))
}

func (c *Catalog) capsuleInputs(catalogRelativePath string) ([]runtime.CapsuleInput, error) {
	if c == nil || c.sourcePath == "" {
		return nil, errors.New("remote Omnigent attachment catalog source is unavailable")
	}
	byDestination := make(map[string]string, len(c.profiles))
	for _, profile := range c.profiles {
		if previous, ok := byDestination[profile.AgentRelativePath]; ok && previous != profile.AgentPath {
			return nil, fmt.Errorf("omnigent catalog destination %q resolves to multiple agent files", profile.AgentRelativePath)
		}
		byDestination[profile.AgentRelativePath] = profile.AgentPath
	}
	destinations := make([]string, 0, len(byDestination))
	for destination := range byDestination {
		destinations = append(destinations, destination)
	}
	sort.Strings(destinations)
	inputs := make([]runtime.CapsuleInput, 0, len(destinations)+1)
	for _, destination := range destinations {
		input, err := capsuleInputForFile(byDestination[destination], destination)
		if err != nil {
			return nil, err
		}
		inputs = append(inputs, input)
	}
	catalogInput, err := capsuleInputForFile(c.sourcePath, catalogRelativePath)
	if err != nil {
		return nil, err
	}
	return append(inputs, catalogInput), nil
}

func capsuleInputForFile(sourcePath, relativePath string) (runtime.CapsuleInput, error) {
	data, err := os.ReadFile(sourcePath)
	if err != nil {
		return runtime.CapsuleInput{}, fmt.Errorf("read staged Omnigent input %q: %w", relativePath, err)
	}
	digest := sha256.Sum256(data)
	return runtime.CapsuleInput{
		SourcePath: sourcePath, RelativePath: relativePath,
		SHA256: "sha256:" + hex.EncodeToString(digest[:]), Mode: 0o644,
	}, nil
}
