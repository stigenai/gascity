// Package omnigentcapsule provides a deterministic, hermetic substitute for
// remote Omnigent capsule composition tests. It models observable boundaries,
// not Kubernetes, SSH, tmux, Herdr, or Omnigent implementation internals.
package omnigentcapsule

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
)

const (
	stateFileName  = "capsule-fixture-state.json"
	markerFileName = "capsule-fixture-created"
)

// Transport identifies the remote placement boundary represented by a
// fixture.
type Transport string

const (
	// TransportKubernetes models a Kubernetes pod and retained PVC.
	TransportKubernetes Transport = "kubernetes"
	// TransportSSH models an SSH host, remote tmux server, and retained state.
	TransportSSH Transport = "ssh"
)

// Harness identifies the coding-harness shape behind an Omnigent profile.
type Harness string

const (
	// HarnessCodex is the Codex-compatible fixture shape.
	HarnessCodex Harness = "codex"
	// HarnessClaudeCode is the Claude Code-compatible fixture shape.
	HarnessClaudeCode Harness = "claude-code"
)

const (
	// ProfileCodex is a credential-free Codex-shaped profile.
	ProfileCodex = "codex"
	// ProfileClaudePrimary is the primary Claude Code-shaped profile.
	ProfileClaudePrimary = "claude-primary"
	// ProfileClaudeSecondary is a separately authenticated Claude Code-shaped
	// profile whose backend deliberately differs from the primary profile.
	ProfileClaudeSecondary = "claude-secondary"
)

// Profile is non-secret profile metadata exposed to a test operator.
type Profile struct {
	ID          string  `json:"id"`
	Harness     Harness `json:"harness"`
	Backend     string  `json:"backend"`
	AuthProfile string  `json:"auth_profile"`
	Blurb       string  `json:"blurb"`
}

var fixtureProfiles = map[string]Profile{
	ProfileCodex: {
		ID: ProfileCodex, Harness: HarnessCodex, Backend: "openai-compatible",
		AuthProfile: "codex-fixture-auth",
		Blurb:       "Credential-free Codex-shaped deterministic profile.",
	},
	ProfileClaudePrimary: {
		ID: ProfileClaudePrimary, Harness: HarnessClaudeCode, Backend: "anthropic",
		AuthProfile: "claude-fixture-primary-auth",
		Blurb:       "Primary Claude Code-shaped profile.",
	},
	ProfileClaudeSecondary: {
		ID: ProfileClaudeSecondary, Harness: HarnessClaudeCode, Backend: "bedrock",
		AuthProfile: "claude-fixture-secondary-auth",
		Blurb:       "Independent Claude Code-shaped fallback profile.",
	},
}

// Config fixes one deterministic remote capsule identity.
type Config struct {
	CapsuleID string    `json:"capsule_id"`
	Transport Transport `json:"transport"`
	ProfileID string    `json:"profile_id"`
}

// Fault is an explicit event-driven fault trigger. Inject never sleeps or
// polls.
type Fault string

const (
	// FaultPrimaryRateLimit advances the primary Claude profile to its fallback.
	FaultPrimaryRateLimit Fault = "primary-rate-limit"
	// FaultPolicyRequired raises one policy request instead of running a model.
	FaultPolicyRequired Fault = "policy-required"
	// FaultTransportLoss makes model operations fail until RestoreTransport.
	FaultTransportLoss Fault = "transport-loss"
	// FaultVolumeLoss removes the durable fixture state.
	FaultVolumeLoss Fault = "volume-loss"
	// FaultServerCrash makes the capsule-local Omnigent service unavailable.
	FaultServerCrash Fault = "server-crash"
	// FaultHostCrash makes the capsule host unavailable.
	FaultHostCrash Fault = "host-crash"
	// FaultHarnessCrash makes the selected harness unavailable.
	FaultHarnessCrash Fault = "harness-crash"
	// FaultModelUnavailable makes the fake loopback model endpoint unavailable.
	FaultModelUnavailable Fault = "model-unavailable"
)

// EventKind is a stable typed fixture transition.
type EventKind string

// EventKind values describe every ordered fixture transition.
const (
	EventCapsuleStarted      EventKind = "capsule-started"
	EventCapsuleRestarted    EventKind = "capsule-restarted"
	EventConversationCreated EventKind = "conversation-created"
	EventProfileFailedOver   EventKind = "profile-failed-over"
	EventModelCompleted      EventKind = "model-completed"
	EventPolicyRequested     EventKind = "policy-requested"
	EventPolicyAnswered      EventKind = "policy-answered"
	EventTransportLost       EventKind = "transport-lost"
	EventTransportRestored   EventKind = "transport-restored"
	EventVolumeLost          EventKind = "volume-lost"
	EventHerdrViewerOpened   EventKind = "herdr-viewer-opened"
	EventHerdrViewerClosed   EventKind = "herdr-viewer-closed"
	EventCapsuleCleaned      EventKind = "capsule-cleaned"
	EventComponentFailed     EventKind = "component-failed"
	EventComponentRestored   EventKind = "component-restored"
)

// Event is one ordered, non-secret fixture transition.
type Event struct {
	Sequence       int64     `json:"sequence"`
	Kind           EventKind `json:"kind"`
	CapsuleID      string    `json:"capsule_id"`
	ConversationID string    `json:"conversation_id,omitempty"`
	ProfileID      string    `json:"profile_id,omitempty"`
	Transport      Transport `json:"transport"`
	RequestID      string    `json:"request_id,omitempty"`
	Component      string    `json:"component,omitempty"`
}

// ModelRequest records one fake loopback model endpoint request. Prompt is
// fixture-owned test data; credentials and network endpoint data are absent.
type ModelRequest struct {
	Sequence       int64  `json:"sequence"`
	ConversationID string `json:"conversation_id"`
	ProfileID      string `json:"profile_id"`
	Prompt         string `json:"prompt"`
}

// Result is the deterministic response from Start or Run.
type Result struct {
	ConversationID string
	Profile        Profile
}

// PolicyAction is an explicit operator response.
type PolicyAction string

const (
	// PolicyApprove approves the fixture request.
	PolicyApprove PolicyAction = "approve"
	// PolicyDeny denies the fixture request.
	PolicyDeny PolicyAction = "deny"
)

// PolicyRequest is a sanitized, conversation-bound policy mail fixture.
type PolicyRequest struct {
	RequestID      string       `json:"request_id"`
	ConversationID string       `json:"conversation_id"`
	ProfileID      string       `json:"profile_id"`
	Action         PolicyAction `json:"action,omitempty"`
	Response       string       `json:"response,omitempty"`
}

// Count is an exact fake process or remote-resource census value.
type Count int

// IsOne reports whether exactly one resource exists.
func (c Count) IsOne() bool { return c == 1 }

// Census reports every lifecycle-relevant fixture resource.
type Census struct {
	ProcessGroups    Count
	TmuxSessions     Count
	UnixSockets      Count
	HerdrViewers     Count
	KubernetesPods   Count
	NetworkPolicies  Count
	PersistentClaims Count
	SSHStateRoots    Count
	StagedCatalogs   Count
}

// Snapshot is the durable worker projection. Viewer state is intentionally
// absent because it must never drive worker reconciliation.
type Snapshot struct {
	CapsuleID      string
	ConversationID string
	ProfileID      string
	Transport      Transport
	Started        bool
}

var (
	// ErrPolicyPending indicates an explicit answer is required.
	ErrPolicyPending = errors.New("omnigent policy response pending")
	// ErrTransportLost indicates the remote transport is unavailable.
	ErrTransportLost = errors.New("capsule transport unavailable")
	// ErrDurableStateLost indicates exact conversation state is absent. A
	// replacement conversation is never created.
	ErrDurableStateLost = errors.New("capsule durable state lost")
	// ErrServerUnavailable indicates the capsule-local server is unavailable.
	ErrServerUnavailable = errors.New("capsule Omnigent server unavailable")
	// ErrHostUnavailable indicates the remote host is unavailable.
	ErrHostUnavailable = errors.New("capsule host unavailable")
	// ErrHarnessUnavailable indicates the selected harness is unavailable.
	ErrHarnessUnavailable = errors.New("capsule harness unavailable")
	// ErrModelUnavailable indicates the fake loopback model is unavailable.
	ErrModelUnavailable = errors.New("capsule model endpoint unavailable")
)

type durableState struct {
	Config         Config         `json:"config"`
	ConversationID string         `json:"conversation_id"`
	ActiveProfile  string         `json:"active_profile"`
	Started        bool           `json:"started"`
	Sequence       int64          `json:"sequence"`
	Events         []Event        `json:"events"`
	Requests       []ModelRequest `json:"requests"`
	PendingPolicy  *PolicyRequest `json:"pending_policy,omitempty"`
}

// Fixture composes fake capsule server, host, harness, loopback model,
// transport, terminal, viewer, and durable-state boundaries.
type Fixture struct {
	mu            sync.Mutex
	root          string
	state         durableState
	faults        map[Fault]bool
	transportLost bool
	volumeLost    bool
	restarted     bool
	census        Census
}

// New creates an unstarted fixture rooted in an isolated caller-owned
// directory.
func New(root string, cfg Config) (*Fixture, error) {
	if root == "" {
		return nil, errors.New("capsule fixture root is required")
	}
	if cfg.CapsuleID == "" {
		return nil, errors.New("capsule fixture ID is required")
	}
	if cfg.Transport != TransportKubernetes && cfg.Transport != TransportSSH {
		return nil, fmt.Errorf("unsupported capsule fixture transport %q", cfg.Transport)
	}
	if _, ok := fixtureProfiles[cfg.ProfileID]; !ok {
		return nil, fmt.Errorf("unknown capsule fixture profile %q", cfg.ProfileID)
	}
	if _, err := os.Stat(filepath.Join(root, stateFileName)); err == nil {
		return nil, errors.New("capsule fixture state already exists; use Restart")
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("inspect capsule fixture root: %w", err)
	}
	return &Fixture{
		root: root, faults: make(map[Fault]bool),
		state: durableState{Config: cfg, ActiveProfile: cfg.ProfileID},
	}, nil
}

// Restart loads the exact durable fixture state. Missing state fails closed.
func Restart(root string) (*Fixture, error) {
	data, err := os.ReadFile(filepath.Join(root, stateFileName))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, ErrDurableStateLost
		}
		return nil, fmt.Errorf("read capsule fixture state: %w", err)
	}
	var state durableState
	if err := json.Unmarshal(data, &state); err != nil {
		return nil, fmt.Errorf("decode capsule fixture state: %w", err)
	}
	if state.ConversationID == "" || state.Config.CapsuleID == "" {
		return nil, ErrDurableStateLost
	}
	if state.Config.Transport != TransportKubernetes && state.Config.Transport != TransportSSH {
		return nil, ErrDurableStateLost
	}
	if _, ok := fixtureProfiles[state.ActiveProfile]; !ok {
		return nil, ErrDurableStateLost
	}
	return &Fixture{root: root, state: state, faults: make(map[Fault]bool), restarted: true}, nil
}

// Start starts or re-adopts the fixture without replacing its conversation.
func (f *Fixture) Start() (Result, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.volumeLost {
		return Result{}, ErrDurableStateLost
	}
	if f.state.ConversationID == "" {
		sum := sha256.Sum256([]byte(f.state.Config.CapsuleID))
		f.state.ConversationID = "conv_" + hex.EncodeToString(sum[:8])
		f.state.Started = true
		f.startResourcesLocked()
		f.emitLocked(EventCapsuleStarted, "")
		f.emitLocked(EventConversationCreated, "")
	} else {
		f.state.Started = true
		f.startResourcesLocked()
		if f.restarted {
			f.emitLocked(EventCapsuleRestarted, "")
			f.restarted = false
		}
	}
	if err := f.persistLocked(); err != nil {
		return Result{}, err
	}
	return f.resultLocked(), nil
}

// Run sends one prompt to the deterministic fake model endpoint.
func (f *Fixture) Run(prompt string) (Result, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.volumeLost {
		return Result{}, ErrDurableStateLost
	}
	if f.transportLost {
		return Result{}, ErrTransportLost
	}
	if !f.state.Started || f.state.ConversationID == "" {
		return Result{}, errors.New("capsule fixture is not started")
	}
	for _, failure := range []struct {
		fault Fault
		err   error
	}{
		{FaultHostCrash, ErrHostUnavailable},
		{FaultServerCrash, ErrServerUnavailable},
		{FaultHarnessCrash, ErrHarnessUnavailable},
		{FaultModelUnavailable, ErrModelUnavailable},
	} {
		if f.faults[failure.fault] {
			return Result{}, failure.err
		}
	}
	if f.faults[FaultPrimaryRateLimit] {
		delete(f.faults, FaultPrimaryRateLimit)
		if f.state.ActiveProfile == ProfileClaudePrimary {
			f.state.ActiveProfile = ProfileClaudeSecondary
			f.emitLocked(EventProfileFailedOver, "")
		}
	}
	if f.faults[FaultPolicyRequired] {
		delete(f.faults, FaultPolicyRequired)
		requestID := fmt.Sprintf("policy_%04d", f.state.Sequence+1)
		f.state.PendingPolicy = &PolicyRequest{
			RequestID: requestID, ConversationID: f.state.ConversationID,
			ProfileID: f.state.ActiveProfile,
		}
		f.emitLocked(EventPolicyRequested, requestID)
		if err := f.persistLocked(); err != nil {
			return Result{}, err
		}
		return Result{}, ErrPolicyPending
	}
	if f.state.PendingPolicy != nil && f.state.PendingPolicy.Action == "" {
		return Result{}, ErrPolicyPending
	}
	f.state.Requests = append(f.state.Requests, ModelRequest{
		Sequence: int64(len(f.state.Requests) + 1), ConversationID: f.state.ConversationID,
		ProfileID: f.state.ActiveProfile, Prompt: prompt,
	})
	f.emitLocked(EventModelCompleted, "")
	if err := f.persistLocked(); err != nil {
		return Result{}, err
	}
	return f.resultLocked(), nil
}

// Inject triggers one named fault synchronously.
func (f *Fixture) Inject(fault Fault) {
	f.mu.Lock()
	defer f.mu.Unlock()
	switch fault {
	case FaultTransportLoss:
		f.transportLost = true
		f.emitLocked(EventTransportLost, "")
		_ = f.persistLocked()
	case FaultVolumeLoss:
		f.emitLocked(EventVolumeLost, "")
		f.volumeLost = true
		_ = os.Remove(filepath.Join(f.root, stateFileName))
	case FaultServerCrash, FaultHostCrash, FaultHarnessCrash, FaultModelUnavailable:
		if !f.faults[fault] {
			f.faults[fault] = true
			f.emitComponentLocked(EventComponentFailed, fault)
			_ = f.persistLocked()
		}
	default:
		f.faults[fault] = true
	}
}

// RestoreComponent clears one explicit component fault without replacing the
// capsule conversation.
func (f *Fixture) RestoreComponent(fault Fault) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	switch fault {
	case FaultServerCrash, FaultHostCrash, FaultHarnessCrash, FaultModelUnavailable:
		if f.faults[fault] {
			delete(f.faults, fault)
			f.emitComponentLocked(EventComponentRestored, fault)
			return f.persistLocked()
		}
		return nil
	default:
		return fmt.Errorf("fault %q is not a restorable component", fault)
	}
}

// RestoreTransport reconnects the fake remote transport without changing the
// capsule conversation or profile.
func (f *Fixture) RestoreTransport() {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.transportLost {
		f.transportLost = false
		f.emitLocked(EventTransportRestored, "")
		_ = f.persistLocked()
	}
}

// PendingPolicy returns a copy of the current policy request.
func (f *Fixture) PendingPolicy() *PolicyRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.state.PendingPolicy == nil {
		return nil
	}
	requestCopy := *f.state.PendingPolicy
	return &requestCopy
}

// AnswerPolicy records one explicit answer for the matching request.
func (f *Fixture) AnswerPolicy(requestID string, action PolicyAction, response string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.state.PendingPolicy == nil || f.state.PendingPolicy.RequestID != requestID {
		return fmt.Errorf("policy request %q not pending", requestID)
	}
	if action != PolicyApprove && action != PolicyDeny {
		return fmt.Errorf("unsupported policy action %q", action)
	}
	f.state.PendingPolicy.Action = action
	f.state.PendingPolicy.Response = response
	f.emitLocked(EventPolicyAnswered, requestID)
	return f.persistLocked()
}

// OpenHerdrViewer creates one lifecycle-neutral fake viewer.
func (f *Fixture) OpenHerdrViewer() {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.census.HerdrViewers == 0 {
		f.census.HerdrViewers = 1
		f.emitLocked(EventHerdrViewerOpened, "")
	}
}

// CloseHerdrViewer removes only the fake viewer.
func (f *Fixture) CloseHerdrViewer() {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.census.HerdrViewers != 0 {
		f.census.HerdrViewers = 0
		f.emitLocked(EventHerdrViewerClosed, "")
	}
}

// Cleanup removes all transient resources. When preserveState is false it
// also purges the fixture's exact durable state.
func (f *Fixture) Cleanup(preserveState bool) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.state.Started = false
	f.census = Census{}
	if preserveState {
		if f.state.Config.Transport == TransportKubernetes {
			f.census.PersistentClaims = 1
		} else {
			f.census.SSHStateRoots = 1
		}
		f.emitLocked(EventCapsuleCleaned, "")
		return f.persistLocked()
	}
	f.emitLocked(EventCapsuleCleaned, "")
	if err := os.Remove(filepath.Join(f.root, stateFileName)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("remove capsule fixture state: %w", err)
	}
	if err := os.Remove(filepath.Join(f.root, markerFileName)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("remove capsule fixture marker: %w", err)
	}
	return nil
}

// AssertClean fails when any fake process, socket, viewer, or remote resource
// remains.
func (f *Fixture) AssertClean() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.census != (Census{}) {
		return fmt.Errorf("capsule fixture leaked resources: %#v", f.census)
	}
	return nil
}

// Snapshot returns the current durable worker projection.
func (f *Fixture) Snapshot() Snapshot {
	f.mu.Lock()
	defer f.mu.Unlock()
	return Snapshot{
		CapsuleID: f.state.Config.CapsuleID, ConversationID: f.state.ConversationID,
		ProfileID: f.state.ActiveProfile, Transport: f.state.Config.Transport,
		Started: f.state.Started,
	}
}

// Events returns an ordered copy of typed fixture events.
func (f *Fixture) Events() []Event {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]Event(nil), f.state.Events...)
}

// ModelRequests returns an ordered copy of fake loopback model requests.
func (f *Fixture) ModelRequests() []ModelRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]ModelRequest(nil), f.state.Requests...)
}

// Census returns the exact current fake resource census.
func (f *Fixture) Census() Census {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.census
}

func (f *Fixture) startResourcesLocked() {
	f.census.ProcessGroups = 1
	f.census.TmuxSessions = 1
	f.census.UnixSockets = 1
	if f.state.Config.Transport == TransportKubernetes {
		f.census.KubernetesPods = 1
		f.census.NetworkPolicies = 1
		f.census.PersistentClaims = 1
	} else {
		f.census.SSHStateRoots = 1
		f.census.StagedCatalogs = 1
	}
}

func (f *Fixture) resultLocked() Result {
	return Result{ConversationID: f.state.ConversationID, Profile: fixtureProfiles[f.state.ActiveProfile]}
}

func (f *Fixture) emitLocked(kind EventKind, requestID string) {
	f.state.Sequence++
	f.state.Events = append(f.state.Events, Event{
		Sequence: f.state.Sequence, Kind: kind, CapsuleID: f.state.Config.CapsuleID,
		ConversationID: f.state.ConversationID, ProfileID: f.state.ActiveProfile,
		Transport: f.state.Config.Transport, RequestID: requestID,
	})
}

func (f *Fixture) emitComponentLocked(kind EventKind, fault Fault) {
	f.emitLocked(kind, "")
	f.state.Events[len(f.state.Events)-1].Component = string(fault)
}

func (f *Fixture) persistLocked() error {
	if err := os.MkdirAll(f.root, 0o700); err != nil {
		return fmt.Errorf("create capsule fixture root: %w", err)
	}
	data, err := json.Marshal(f.state)
	if err != nil {
		return fmt.Errorf("encode capsule fixture state: %w", err)
	}
	tmp, err := os.CreateTemp(f.root, ".capsule-fixture-*.tmp")
	if err != nil {
		return fmt.Errorf("create capsule fixture state: %w", err)
	}
	tmpPath := tmp.Name()
	defer func() { _ = os.Remove(tmpPath) }()
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmpPath, filepath.Join(f.root, stateFileName)); err != nil {
		return fmt.Errorf("commit capsule fixture state: %w", err)
	}
	if err := os.WriteFile(filepath.Join(f.root, markerFileName), []byte("fixture-v1\n"), 0o600); err != nil {
		return fmt.Errorf("write capsule fixture marker: %w", err)
	}
	return nil
}
