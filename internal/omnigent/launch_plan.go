package omnigent

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"path/filepath"
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
	Location         AttachmentLocation
	Runtime          string
	Workspace        string
	StateRoot        string
	SocketPath       string
	CatalogPath      string
	CatalogSHA256    string
	CapsuleKey       runtime.CapsuleKey
	Pin              Pin
	SecretProvider   runtime.SecretProvider
	SecretReferences []runtime.SecretReference
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
	selected, err := runtime.SelectSecretReferences(provider, input.SecretReferences)
	if err != nil {
		return AttachmentLaunchPlan{}, err
	}
	plan.StateRoot = CapsuleStateRoot
	plan.SocketPath = CapsuleSocketPath
	plan.CatalogPath = CapsuleCatalogPath
	plan.CatalogSHA256 = input.CatalogSHA256
	plan.CapsuleKey = key
	plan.Pin = input.Pin
	plan.SecretProvider = provider
	plan.SecretReferences = selected
	return plan, nil
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

// Fingerprint returns a versioned non-secret identity for provisioning drift.
func (p AttachmentLaunchPlan) Fingerprint() string {
	h := sha256.New()
	for _, value := range []string{
		string(p.Location), p.Runtime, p.Workspace, p.StateRoot, p.SocketPath, p.CatalogPath,
		p.CapsuleKey.Digest, p.CatalogSHA256, p.Pin.Commit, p.Pin.PackageVersion, p.Pin.Executable, p.Pin.SHA256,
		string(p.SecretProvider), runtime.ProvisionFingerprint(runtime.Config{SecretReferences: p.SecretReferences}),
	} {
		_, _ = h.Write([]byte(value))
		_, _ = h.Write([]byte{0})
	}
	return "v1:" + hex.EncodeToString(h.Sum(nil))
}
