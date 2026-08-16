package runtime

import (
	"context"
	"crypto/sha256"
	"encoding/base32"
	"encoding/hex"
	"errors"
	"fmt"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"unicode/utf8"
)

const capsuleIdentityVersion = 1

var (
	capsuleSecretIDPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]{0,62}$`)
	environmentNamePattern = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
	kubernetesNamePattern  = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`)
	kubernetesKeyPattern   = regexp.MustCompile(`^[A-Za-z0-9._-]+$`)
	capsuleDigestPattern   = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)
)

// SecretProvider identifies the runtime edge allowed to resolve a secret
// reference. These values intentionally match the built-in runtime selectors.
type SecretProvider string

const (
	// SecretProviderKubernetes selects Kubernetes SecretKeyRef sources.
	SecretProviderKubernetes SecretProvider = "k8s"
	// SecretProviderSSH selects pre-provisioned SSH path sources.
	SecretProviderSSH SecretProvider = "ssh"
)

var (
	// ErrUnsupportedSecretProvider reports that a runtime has no typed secret
	// projection contract. Callers must reject the launch rather than copy
	// ambient controller credentials or fall back to another runtime.
	ErrUnsupportedSecretProvider = errors.New("unsupported secret provider")
	// ErrSecretSourceUnavailable reports that a logical secret does not declare
	// a source for the selected runtime provider.
	ErrSecretSourceUnavailable = errors.New("secret source unavailable")
)

// SecretProviderError identifies the selected provider and logical reference
// that could not be projected. It never contains a credential value, path, or
// environment destination.
type SecretProviderError struct {
	Provider    SecretProvider
	ReferenceID string
	Err         error
}

func (e *SecretProviderError) Error() string {
	if e.ReferenceID == "" {
		return fmt.Sprintf("secret provider %q: %v", e.Provider, e.Err)
	}
	return fmt.Sprintf("secret reference %q for provider %q: %v", e.ReferenceID, e.Provider, e.Err)
}

// Unwrap supports errors.Is without exposing provider-specific source data.
func (e *SecretProviderError) Unwrap() error { return e.Err }

// ErrCapsuleStateConflict reports that durable state exists but cannot be
// safely attached, adopted, or purged because ownership or attachment is
// ambiguous. Callers must fail closed and preserve the state.
var ErrCapsuleStateConflict = errors.New("capsule durable state conflict")

// CapsuleKey is the provider-neutral durable identity for one session-owned
// capsule. Token is the 130-bit portable resource token; Digest is the full
// SHA-256 ownership proof. Neither value contains credentials.
type CapsuleKey struct {
	Version   int
	CityScope string
	SessionID string
	Token     string
	Digest    string
}

// NewCapsuleKey validates and derives one deterministic capsule identity.
func NewCapsuleKey(cityScope, sessionID string) (CapsuleKey, error) {
	cityScope = strings.TrimSpace(cityScope)
	sessionID = strings.TrimSpace(sessionID)
	if cityScope == "" || sessionID == "" {
		return CapsuleKey{}, errors.New("capsule city scope and session id are required")
	}
	if !utf8.ValidString(cityScope) || !utf8.ValidString(sessionID) || strings.ContainsRune(cityScope, 0) || strings.ContainsRune(sessionID, 0) {
		return CapsuleKey{}, errors.New("capsule city scope and session id must be valid UTF-8 without NUL")
	}
	canonical := fmt.Sprintf("%d\x00%s\x00%s", capsuleIdentityVersion, cityScope, sessionID)
	sum := sha256.Sum256([]byte(canonical))
	token := strings.ToLower(base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(sum[:]))[:26]
	return CapsuleKey{
		Version: capsuleIdentityVersion, CityScope: cityScope, SessionID: sessionID,
		Token: token, Digest: hex.EncodeToString(sum[:]),
	}, nil
}

// Validate proves that k was derived from its current city and session fields.
// Providers must call it before discovering or mutating durable resources so a
// hand-built or stale key cannot cross city or session boundaries.
func (k CapsuleKey) Validate() error {
	want, err := NewCapsuleKey(k.CityScope, k.SessionID)
	if err != nil {
		return err
	}
	if k != want {
		return fmt.Errorf("%w: capsule key does not match canonical city/session identity", ErrCapsuleStateConflict)
	}
	return nil
}

// ResourceStem returns the portable, diagnostic provider resource name for k.
// The human hint is never used as identity; Token and Digest carry identity.
func (k CapsuleKey) ResourceStem() string {
	hint := portableCapsuleHint(k.SessionID, 20)
	if hint == "" {
		return "gco-" + k.Token
	}
	return "gco-" + hint + "-" + k.Token
}

func portableCapsuleHint(value string, limit int) string {
	var b strings.Builder
	lastDash := false
	for _, r := range strings.ToLower(value) {
		valid := r >= 'a' && r <= 'z' || r >= '0' && r <= '9'
		if valid {
			if b.Len() >= limit {
				break
			}
			b.WriteRune(r)
			lastDash = false
			continue
		}
		if b.Len() > 0 && b.Len() < limit && !lastDash {
			b.WriteByte('-')
			lastDash = true
		}
	}
	return strings.TrimRight(b.String(), "-")
}

// KubernetesSecretKeyReference identifies one Kubernetes Secret key without
// resolving or serializing its value.
type KubernetesSecretKeyReference struct {
	Name string
	Key  string
}

// SSHSecretPathReference identifies one pre-provisioned, owner-only SSH-host
// path. The file or directory contents remain provider-side.
type SSHSecretPathReference struct {
	Path string
}

// SecretReference maps one logical credential to exactly one environment or
// mounted-file destination. It may carry both Kubernetes and SSH sources so a
// hybrid agent retains one logical profile while each provider resolves only
// its own source. It deliberately has no field capable of holding a secret
// value.
type SecretReference struct {
	ID          string
	Environment string
	MountPath   string
	Kubernetes  *KubernetesSecretKeyReference
	SSH         *SSHSecretPathReference
}

// ValidateSecretReferences validates provider-neutral identity, destination,
// and provider source shapes without resolving credentials.
func ValidateSecretReferences(refs []SecretReference) error {
	ids := make(map[string]bool, len(refs))
	destinations := make(map[string]bool, len(refs))
	for i := range refs {
		ref := refs[i]
		if !capsuleSecretIDPattern.MatchString(ref.ID) {
			return fmt.Errorf("secret reference[%d]: id must match [A-Za-z0-9][A-Za-z0-9_-]{0,62}", i)
		}
		if ids[ref.ID] {
			return fmt.Errorf("secret reference %q: duplicate id", ref.ID)
		}
		ids[ref.ID] = true
		hasEnv := ref.Environment != ""
		hasMount := ref.MountPath != ""
		if hasEnv == hasMount {
			return fmt.Errorf("secret reference %q: exactly one environment or mount_path destination is required", ref.ID)
		}
		destination := "env:" + ref.Environment
		if hasEnv {
			if !environmentNamePattern.MatchString(ref.Environment) || forbiddenSecretEnvironment(ref.Environment) {
				return fmt.Errorf("secret reference %q: environment destination is reserved or invalid", ref.ID)
			}
		} else {
			clean := filepath.Clean(ref.MountPath)
			if !filepath.IsAbs(ref.MountPath) || clean != ref.MountPath || clean == string(filepath.Separator) {
				return fmt.Errorf("secret reference %q: mount_path must be a clean absolute path", ref.ID)
			}
			destination = "mount:" + clean
		}
		if destinations[destination] {
			return fmt.Errorf("secret reference %q: duplicate destination", ref.ID)
		}
		destinations[destination] = true
		if ref.Kubernetes == nil && ref.SSH == nil {
			return fmt.Errorf("secret reference %q: at least one provider source is required", ref.ID)
		}
		if ref.Kubernetes != nil {
			if len(ref.Kubernetes.Name) > 253 || !kubernetesNamePattern.MatchString(ref.Kubernetes.Name) ||
				len(ref.Kubernetes.Key) > 253 || !kubernetesKeyPattern.MatchString(ref.Kubernetes.Key) {
				return fmt.Errorf("secret reference %q: invalid Kubernetes Secret name or key", ref.ID)
			}
		}
		if ref.SSH != nil {
			clean := filepath.Clean(ref.SSH.Path)
			if !filepath.IsAbs(ref.SSH.Path) || clean != ref.SSH.Path || clean == string(filepath.Separator) {
				return fmt.Errorf("secret reference %q: SSH path must be a clean absolute path", ref.ID)
			}
		}
	}
	return nil
}

// SelectSecretReferences validates refs and returns a provider-confined copy.
// The result includes only the source the selected runtime may resolve, so a
// provider cannot accidentally inspect or serialize another provider's source.
func SelectSecretReferences(provider SecretProvider, refs []SecretReference) ([]SecretReference, error) {
	if provider != SecretProviderKubernetes && provider != SecretProviderSSH {
		return nil, &SecretProviderError{Provider: provider, Err: ErrUnsupportedSecretProvider}
	}
	if err := ValidateSecretReferences(refs); err != nil {
		return nil, err
	}
	selected := make([]SecretReference, len(refs))
	for i, ref := range refs {
		selected[i] = SecretReference{ID: ref.ID, Environment: ref.Environment, MountPath: ref.MountPath}
		switch provider {
		case SecretProviderKubernetes:
			if ref.Kubernetes == nil {
				return nil, &SecretProviderError{Provider: provider, ReferenceID: ref.ID, Err: ErrSecretSourceUnavailable}
			}
			v := *ref.Kubernetes
			selected[i].Kubernetes = &v
		case SecretProviderSSH:
			if ref.SSH == nil {
				return nil, &SecretProviderError{Provider: provider, ReferenceID: ref.ID, Err: ErrSecretSourceUnavailable}
			}
			v := *ref.SSH
			selected[i].SSH = &v
		}
	}
	return selected, nil
}

func forbiddenSecretEnvironment(name string) bool {
	if strings.HasPrefix(name, "GC_") || strings.HasPrefix(name, "OMNIGENT_") {
		return true
	}
	switch name {
	case "HOME", "PATH", "SHELL", "USER", "LOGNAME", "TMPDIR", "PWD", "OLDPWD":
		return true
	default:
		return false
	}
}

func normalizedSecretReferenceIdentities(refs []SecretReference) []string {
	identities := make([]string, 0, len(refs))
	for _, ref := range refs {
		var k8sName, k8sKey, sshPath string
		if ref.Kubernetes != nil {
			k8sName, k8sKey = ref.Kubernetes.Name, ref.Kubernetes.Key
		}
		if ref.SSH != nil {
			sshPath = ref.SSH.Path
		}
		identities = append(identities, strings.Join([]string{ref.ID, ref.Environment, ref.MountPath, k8sName, k8sKey, sshPath}, "\x00"))
	}
	sort.Strings(identities)
	return identities
}

// CapsuleStateReference is an opaque provider-owned durable allocation. The
// mount path is a fixed capsule-internal location, not an operator path.
type CapsuleStateReference struct {
	Key         CapsuleKey
	Provider    string
	ResourceID  string
	ResourceUID string
	MountPath   string
}

// CapsuleLaunchConfig is the provider-neutral, non-secret plan for starting a
// capsule-local service and its interactive client inside one runtime Place.
// ResourceID fields are opaque provider-owned names, not operator paths.
type CapsuleLaunchConfig struct {
	Key               CapsuleKey
	State             CapsuleStateReference
	Command           []string
	RunRoot           string
	SocketPath        string
	CatalogResourceID string
	CatalogMountPath  string
	CatalogSHA256     string
}

// Validate checks provider-neutral identity, path, command, and catalog
// invariants before a concrete runtime authors infrastructure.
func (c CapsuleLaunchConfig) Validate() error {
	if err := c.Key.Validate(); err != nil {
		return err
	}
	if c.State.Key != c.Key {
		return fmt.Errorf("%w: capsule state key does not match launch key", ErrCapsuleStateConflict)
	}
	if strings.TrimSpace(c.State.ResourceID) == "" {
		return errors.New("capsule state resource id is required")
	}
	if strings.TrimSpace(c.State.ResourceUID) == "" {
		return errors.New("capsule state resource uid is required")
	}
	for kind, value := range map[string]string{
		"state mount":   c.State.MountPath,
		"run root":      c.RunRoot,
		"socket":        c.SocketPath,
		"catalog mount": c.CatalogMountPath,
	} {
		if value == "" || !filepath.IsAbs(value) || filepath.Clean(value) != value || value == string(filepath.Separator) {
			return fmt.Errorf("capsule %s must be a clean absolute non-root path", kind)
		}
	}
	relSocket, err := filepath.Rel(c.RunRoot, c.SocketPath)
	if err != nil || relSocket == "." || relSocket == ".." || strings.HasPrefix(relSocket, ".."+string(filepath.Separator)) {
		return errors.New("capsule socket must stay beneath run root")
	}
	if capsulePathsOverlap(c.State.MountPath, c.RunRoot) || capsulePathsOverlap(c.State.MountPath, c.CatalogMountPath) || capsulePathsOverlap(c.RunRoot, c.CatalogMountPath) {
		return errors.New("capsule state, run, and catalog mounts must be distinct")
	}
	if len(c.Command) == 0 || strings.TrimSpace(c.Command[0]) == "" {
		return errors.New("capsule command is required")
	}
	for _, arg := range c.Command {
		if strings.ContainsRune(arg, 0) {
			return errors.New("capsule command arguments must not contain NUL")
		}
	}
	if strings.TrimSpace(c.CatalogResourceID) == "" {
		return errors.New("capsule catalog resource id is required")
	}
	if !capsuleDigestPattern.MatchString(c.CatalogSHA256) {
		return errors.New("capsule catalog digest must use sha256:<64 lowercase hex> form")
	}
	return nil
}

func capsulePathsOverlap(left, right string) bool {
	left = filepath.Clean(left)
	right = filepath.Clean(right)
	return left == right || strings.HasPrefix(left, right+string(filepath.Separator)) || strings.HasPrefix(right, left+string(filepath.Separator))
}

// CapsuleStateRuntime is the optional Runtime capability that owns durable
// state independently from a transient Place.
type CapsuleStateRuntime interface {
	EnsureCapsuleState(ctx context.Context, key CapsuleKey) (ref CapsuleStateReference, created bool, err error)
	OpenCapsuleState(ctx context.Context, key CapsuleKey) (ref CapsuleStateReference, ok bool, err error)
	ListCapsuleStates(ctx context.Context) ([]CapsuleStateReference, error)
	PurgeCapsuleState(ctx context.Context, key CapsuleKey) error
}

// CapsuleStatePlace is the optional Place-side capability for exclusive attach
// and detach of a provider-owned durable allocation.
type CapsuleStatePlace interface {
	AttachCapsuleState(ctx context.Context, placeName string, ref CapsuleStateReference) error
	DetachCapsuleState(ctx context.Context, placeName string) error
}
