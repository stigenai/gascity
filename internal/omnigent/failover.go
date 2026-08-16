package omnigent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	failoverStateVersion = "1"

	failoverVersionLabel      = "gascity.omnigent.failover.version"
	failoverChainLabel        = "gascity.omnigent.profile.chain"
	failoverActiveLabel       = "gascity.omnigent.profile.active"
	failoverLastSequenceLabel = "gascity.omnigent.failover.last_sequence"
	failoverLastFromLabel     = "gascity.omnigent.failover.last_from"
	failoverLastToLabel       = "gascity.omnigent.failover.last_to"
	failoverLastReasonLabel   = "gascity.omnigent.failover.last_reason"
	failoverLastAtLabel       = "gascity.omnigent.failover.last_at"
	failoverExhaustedLabel    = "gascity.omnigent.failover.exhausted"
)

// FailoverReason is a stable, non-secret reason category for a profile
// transition. It intentionally excludes provider messages and account data.
type FailoverReason string

const (
	// FailoverAuthentication means the configured backend rejected or expired
	// its authentication.
	FailoverAuthentication FailoverReason = "authentication"
	// FailoverRateLimit means the configured backend exhausted its allowed
	// request capacity.
	FailoverRateLimit FailoverReason = "rate_limit"
	// FailoverBackendUnavailable means the configured backend could not serve
	// the request after Omnigent exhausted its own retry policy.
	FailoverBackendUnavailable FailoverReason = "backend_unavailable"
)

// FailoverSignal is a verified terminal Omnigent failure that is eligible to
// advance one configured profile position.
type FailoverSignal struct {
	Sequence int64
	Reason   FailoverReason
}

// BoundProfile pairs non-secret catalog metadata with the exact built-in
// Omnigent agent registered for that profile.
type BoundProfile struct {
	ID      string
	AgentID string
	Blurb   string
	Backend string
	Harness string
}

// FailoverState is the reconciled sticky state of one Omnigent conversation.
type FailoverState struct {
	ConversationID  string
	Chain           []BoundProfile
	ActiveIndex     int
	ActiveProfileID string
	LastSequence    int64
}

// ProfileTransition is the non-secret status record for one completed
// in-place switch-agent operation.
type ProfileTransition struct {
	FromProfileID string         `json:"from_profile_id"`
	FromBlurb     string         `json:"from_blurb"`
	ToProfileID   string         `json:"to_profile_id"`
	ToBlurb       string         `json:"to_blurb"`
	Reason        FailoverReason `json:"reason"`
	At            time.Time      `json:"at"`
}

// FailoverObservation is the bounded machine-only subset of one stream event
// forwarded to the local sidecar. Provider prose and credentials are omitted.
type FailoverObservation struct {
	ConversationID string `json:"conversation_id"`
	ExpectedActive int    `json:"expected_active"`
	Type           string `json:"type"`
	Source         string `json:"source"`
	Sequence       *int64 `json:"sequence,omitempty"`
	ErrorCode      string `json:"error_code,omitempty"`
	StatusCode     int    `json:"status_code,omitempty"`
}

// FailoverObservationResult is the non-secret local sidecar response needed by
// an attached pane to display sticky profile state without choosing a profile.
type FailoverObservationResult struct {
	ActiveProfileID string             `json:"active_profile_id,omitempty"`
	ActiveIndex     int                `json:"active_index"`
	Transition      *ProfileTransition `json:"transition,omitempty"`
	Ignored         bool               `json:"ignored"`
	Exhausted       bool               `json:"exhausted"`
}

// ObserveFailover forwards only stable classifier fields to the city-local
// sidecar. The sidecar owns classification, chain advancement, and stickiness;
// this client merely observes and displays the returned facts.
func (c *APIClient) ObserveFailover(ctx context.Context, conversationID string, expectedActive int, event StreamEvent) (FailoverObservationResult, error) {
	if err := validateOpaqueID("conversation", conversationID); err != nil {
		return FailoverObservationResult{}, err
	}
	if expectedActive < 0 {
		return FailoverObservationResult{}, errors.New("omnigent failover expected active index must not be negative")
	}
	observation := FailoverObservation{
		ConversationID: conversationID,
		ExpectedActive: expectedActive,
		Type:           boundedText(strings.TrimSpace(event.Type), 80),
		Source:         boundedText(strings.TrimSpace(event.Source), 80),
		Sequence:       event.SequenceNumber,
	}
	if event.Error != nil {
		observation.ErrorCode = boundedText(strings.TrimSpace(event.Error.Code), 80)
		if event.Error.Detail != nil {
			observation.StatusCode = event.Error.Detail.StatusCode
		}
	}
	var result FailoverObservationResult
	if err := c.doJSON(ctx, http.MethodPost, "/gascity/v1/failover", observation, &result); err != nil {
		return FailoverObservationResult{}, err
	}
	if result.Ignored {
		return result, nil
	}
	if result.ActiveIndex < 0 || !profileIDPattern.MatchString(result.ActiveProfileID) {
		return FailoverObservationResult{}, errors.New("omnigent failover response has invalid active profile state")
	}
	if result.Transition != nil {
		transition := result.Transition
		if !profileIDPattern.MatchString(transition.FromProfileID) || !profileIDPattern.MatchString(transition.ToProfileID) || !validFailoverReason(transition.Reason) || transition.At.IsZero() {
			return FailoverObservationResult{}, errors.New("omnigent failover response has invalid transition")
		}
	}
	return result, nil
}

func validFailoverReason(reason FailoverReason) bool {
	switch reason {
	case FailoverAuthentication, FailoverRateLimit, FailoverBackendUnavailable:
		return true
	default:
		return false
	}
}

// FailoverResult reports an advance, a stale/duplicate signal, or exhaustion.
type FailoverResult struct {
	State      FailoverState
	Transition *ProfileTransition
	Ignored    bool
	Exhausted  bool
}

// FailoverController serializes profile transitions per conversation and
// reconciles interrupted transitions from Omnigent's durable agent binding.
type FailoverController struct {
	client  *APIClient
	catalog *Catalog
	now     func() time.Time
	locks   conversationLocks
}

type conversationLocks struct {
	mu    sync.Mutex
	locks map[string]*conversationLock
}

type conversationLock struct {
	mu   sync.Mutex
	refs int
}

// NewFailoverController constructs a controller over one local Omnigent API
// and one immutable validated catalog.
func NewFailoverController(client *APIClient, catalog *Catalog, now func() time.Time) (*FailoverController, error) {
	if client == nil {
		return nil, errors.New("omnigent API client is required for failover")
	}
	if catalog == nil {
		return nil, errors.New("omnigent profile catalog is required for failover")
	}
	if now == nil {
		return nil, errors.New("clock is required for omnigent failover")
	}
	return &FailoverController{
		client: client, catalog: catalog, now: now,
		locks: conversationLocks{locks: make(map[string]*conversationLock)},
	}, nil
}

// ClassifyFailoverEvent accepts only a terminal LLM error with a positive
// sequence number and an explicit machine classifier. It never parses prose.
func ClassifyFailoverEvent(event StreamEvent) (FailoverSignal, bool) {
	if event.Type != "response.error" || event.Source != "llm" || event.Error == nil || event.SequenceNumber == nil || *event.SequenceNumber <= 0 {
		return FailoverSignal{}, false
	}
	code := strings.ToLower(strings.TrimSpace(event.Error.Code))
	statusCode := 0
	if event.Error.Detail != nil {
		statusCode = event.Error.Detail.StatusCode
	}
	reason, ok := classifyFailoverCode(code, statusCode)
	if !ok {
		return FailoverSignal{}, false
	}
	return FailoverSignal{Sequence: *event.SequenceNumber, Reason: reason}, true
}

func classifyFailoverCode(code string, statusCode int) (FailoverReason, bool) {
	if statusCode != 0 {
		code = strconv.Itoa(statusCode)
	}
	switch code {
	case "401", "403", "authentication_error", "auth_error", "unauthorized", "invalid_api_key", "not_authenticated", "expired_token":
		return FailoverAuthentication, true
	case "429", "rate_limit", "rate_limit_exceeded":
		return FailoverRateLimit, true
	case "500", "502", "503", "504", "backend_unavailable", "provider_unavailable", "service_unavailable", "connection_error", "server_error", "timeout":
		return FailoverBackendUnavailable, true
	default:
		return "", false
	}
}

// BindProfileChain resolves every catalog profile to exactly one Omnigent
// built-in agent. Missing or ambiguous registrations fail closed.
func BindProfileChain(catalog *Catalog, rootID string, agents []Agent) ([]BoundProfile, error) {
	if catalog == nil {
		return nil, errors.New("omnigent profile catalog is required")
	}
	profiles, err := catalog.Chain(rootID)
	if err != nil {
		return nil, err
	}
	return bindProfiles(profiles, agents)
}

// BindStoredProfileChain resolves an immutable conversation-owned profile
// chain. Catalog fallback edits affect only newly created conversations; every
// referenced profile must still exist and retain its harness contract.
func BindStoredProfileChain(catalog *Catalog, ids []string, agents []Agent) ([]BoundProfile, error) {
	if catalog == nil {
		return nil, errors.New("omnigent profile catalog is required")
	}
	profiles := make([]ResolvedProfile, 0, len(ids))
	seen := make(map[string]bool, len(ids))
	for _, id := range ids {
		if seen[id] {
			return nil, fmt.Errorf("stored omnigent profile chain contains duplicate %q", id)
		}
		seen[id] = true
		profile, ok := catalog.Profile(id)
		if !ok {
			return nil, fmt.Errorf("stored omnigent profile %q is unavailable in the current catalog", id)
		}
		profiles = append(profiles, profile)
	}
	return bindProfiles(profiles, agents)
}

func bindProfiles(profiles []ResolvedProfile, agents []Agent) ([]BoundProfile, error) {
	byName := make(map[string][]Agent, len(agents))
	for _, agent := range agents {
		if err := validateOpaqueID("agent", agent.ID); err != nil {
			return nil, err
		}
		name := strings.TrimSpace(agent.Name)
		if name == "" {
			return nil, fmt.Errorf("omnigent agent %q has no name", agent.ID)
		}
		byName[name] = append(byName[name], agent)
	}
	bound := make([]BoundProfile, 0, len(profiles))
	for _, profile := range profiles {
		matches := byName[profile.AgentName]
		if len(matches) != 1 {
			return nil, fmt.Errorf("omnigent profile %q requires exactly one registered agent named %q, found %d", profile.ID, profile.AgentName, len(matches))
		}
		agentHarness := canonicalHarness(matches[0].Harness)
		if agentHarness == "" || agentHarness != canonicalHarness(profile.Harness) {
			return nil, fmt.Errorf("omnigent profile %q agent harness %q does not match catalog harness %q", profile.ID, matches[0].Harness, profile.Harness)
		}
		bound = append(bound, BoundProfile{
			ID: profile.ID, AgentID: matches[0].ID, Blurb: profile.Blurb,
			Backend: profile.Backend, Harness: profile.Harness,
		})
	}
	return bound, nil
}

func canonicalHarness(value string) string {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "claude-sdk", "claude_sdk":
		return "claude-sdk"
	case "codex":
		return "codex"
	default:
		return ""
	}
}

// InitialFailoverLabels returns the immutable chain and initial active index
// to include atomically in Omnigent session creation.
func InitialFailoverLabels(catalog *Catalog, rootID string) (map[string]string, error) {
	if catalog == nil {
		return nil, errors.New("omnigent profile catalog is required")
	}
	chain, err := catalog.Chain(rootID)
	if err != nil {
		return nil, err
	}
	ids := make([]string, 0, len(chain))
	for _, profile := range chain {
		ids = append(ids, profile.ID)
	}
	encoded, err := json.Marshal(ids)
	if err != nil {
		return nil, fmt.Errorf("encode omnigent profile chain: %w", err)
	}
	return map[string]string{
		failoverVersionLabel:   failoverStateVersion,
		failoverChainLabel:     string(encoded),
		failoverActiveLabel:    "0",
		failoverExhaustedLabel: "false",
	}, nil
}

// Reconcile returns the conversation's actual active profile. If a crash
// occurred after switch-agent but before labels were patched, it repairs the
// index from Omnigent's durable agent binding without switching again.
func (c *FailoverController) Reconcile(ctx context.Context, conversationID string) (FailoverState, error) {
	if err := validateOpaqueID("conversation", conversationID); err != nil {
		return FailoverState{}, err
	}
	release := c.locks.acquire(conversationID)
	defer release()
	return c.reconcileLocked(ctx, conversationID)
}

// Advance advances exactly one ordered profile for a verified failure. The
// expected index provides optimistic compare-and-set semantics to concurrent
// stream consumers; stale and duplicate observations are ignored.
func (c *FailoverController) Advance(ctx context.Context, conversationID string, expectedActive int, event StreamEvent) (FailoverResult, error) {
	if err := validateOpaqueID("conversation", conversationID); err != nil {
		return FailoverResult{}, err
	}
	signal, ok := ClassifyFailoverEvent(event)
	if !ok {
		return FailoverResult{Ignored: true}, nil
	}
	release := c.locks.acquire(conversationID)
	defer release()

	state, err := c.reconcileLocked(ctx, conversationID)
	if err != nil {
		return FailoverResult{}, err
	}
	if state.ActiveIndex != expectedActive || signal.Sequence <= state.LastSequence {
		return FailoverResult{State: state, Ignored: true}, nil
	}
	if state.ActiveIndex+1 >= len(state.Chain) {
		labels := transitionLabels(state.ActiveIndex, state.ActiveIndex, signal, c.now(), true, state.Chain)
		if _, err := c.client.UpdateLabels(ctx, conversationID, labels); err != nil {
			return FailoverResult{}, redactedClientError("record omnigent failover exhaustion", err)
		}
		state.LastSequence = signal.Sequence
		return FailoverResult{State: state, Exhausted: true}, nil
	}

	if stateSession, err := c.client.GetSession(ctx, conversationID); err != nil {
		return FailoverResult{}, redactedClientError("verify omnigent conversation before failover", err)
	} else if stateSession.Status != "idle" && stateSession.Status != "failed" {
		return FailoverResult{}, fmt.Errorf("omnigent conversation %q is %q; profile failover requires an idle or failed conversation", conversationID, stateSession.Status)
	}

	fromIndex := state.ActiveIndex
	toIndex := fromIndex + 1
	target := state.Chain[toIndex]
	switched, err := c.client.SwitchAgent(ctx, conversationID, target.AgentID)
	if err != nil {
		return FailoverResult{}, redactedClientError("switch omnigent profile", err)
	}
	if switched.AgentID != target.AgentID {
		return FailoverResult{}, fmt.Errorf("omnigent switch-agent bound unexpected agent %q", switched.AgentID)
	}
	at := c.now().UTC()
	labels := transitionLabels(fromIndex, toIndex, signal, at, false, state.Chain)
	if _, err := c.client.UpdateLabels(ctx, conversationID, labels); err != nil {
		return FailoverResult{}, redactedClientError("record switched omnigent profile", err)
	}
	transition := &ProfileTransition{
		FromProfileID: state.Chain[fromIndex].ID,
		FromBlurb:     state.Chain[fromIndex].Blurb,
		ToProfileID:   target.ID,
		ToBlurb:       target.Blurb,
		Reason:        signal.Reason,
		At:            at,
	}
	state.ActiveIndex = toIndex
	state.ActiveProfileID = target.ID
	state.LastSequence = signal.Sequence
	return FailoverResult{State: state, Transition: transition}, nil
}

func (c *FailoverController) reconcileLocked(ctx context.Context, conversationID string) (FailoverState, error) {
	session, err := c.client.GetSession(ctx, conversationID)
	if err != nil {
		return FailoverState{}, redactedClientError("load omnigent conversation for failover", err)
	}
	chainIDs, labeledIndex, lastSequence, err := parseFailoverLabels(session.Labels)
	if err != nil {
		return FailoverState{}, fmt.Errorf("omnigent conversation %q has invalid failover state: %w", conversationID, err)
	}
	agents, err := c.client.ListAgents(ctx)
	if err != nil {
		return FailoverState{}, redactedClientError("list omnigent agents for failover", err)
	}
	bound, err := BindStoredProfileChain(c.catalog, chainIDs, agents)
	if err != nil {
		return FailoverState{}, err
	}
	actualIndex := -1
	for i, profile := range bound {
		if profile.AgentID == session.AgentID {
			actualIndex = i
			break
		}
	}
	if actualIndex < 0 {
		return FailoverState{}, fmt.Errorf("omnigent conversation %q is bound to an agent outside its immutable profile chain", conversationID)
	}
	if labeledIndex != actualIndex {
		if _, err := c.client.UpdateLabels(ctx, conversationID, map[string]string{failoverActiveLabel: strconv.Itoa(actualIndex)}); err != nil {
			return FailoverState{}, redactedClientError("repair omnigent failover state", err)
		}
	}
	return FailoverState{
		ConversationID:  conversationID,
		Chain:           bound,
		ActiveIndex:     actualIndex,
		ActiveProfileID: bound[actualIndex].ID,
		LastSequence:    lastSequence,
	}, nil
}

func parseFailoverLabels(labels map[string]string) ([]string, int, int64, error) {
	if labels[failoverVersionLabel] != failoverStateVersion {
		return nil, 0, 0, fmt.Errorf("version must be %q", failoverStateVersion)
	}
	var chain []string
	if err := json.Unmarshal([]byte(labels[failoverChainLabel]), &chain); err != nil {
		return nil, 0, 0, errors.New("profile chain is not valid JSON")
	}
	if len(chain) == 0 {
		return nil, 0, 0, errors.New("profile chain is empty")
	}
	seen := make(map[string]bool, len(chain))
	for _, id := range chain {
		if !profileIDPattern.MatchString(id) || seen[id] {
			return nil, 0, 0, errors.New("profile chain contains an invalid or duplicate id")
		}
		seen[id] = true
	}
	active, err := strconv.Atoi(labels[failoverActiveLabel])
	if err != nil || active < 0 || active >= len(chain) {
		return nil, 0, 0, errors.New("active profile index is invalid")
	}
	lastSequence := int64(0)
	if raw := labels[failoverLastSequenceLabel]; raw != "" {
		lastSequence, err = strconv.ParseInt(raw, 10, 64)
		if err != nil || lastSequence <= 0 {
			return nil, 0, 0, errors.New("last failover sequence is invalid")
		}
	}
	return chain, active, lastSequence, nil
}

func transitionLabels(fromIndex, toIndex int, signal FailoverSignal, at time.Time, exhausted bool, chain []BoundProfile) map[string]string {
	return map[string]string{
		failoverActiveLabel:       strconv.Itoa(toIndex),
		failoverLastSequenceLabel: strconv.FormatInt(signal.Sequence, 10),
		failoverLastFromLabel:     chain[fromIndex].ID,
		failoverLastToLabel:       chain[toIndex].ID,
		failoverLastReasonLabel:   string(signal.Reason),
		failoverLastAtLabel:       at.UTC().Format(time.RFC3339Nano),
		failoverExhaustedLabel:    strconv.FormatBool(exhausted),
	}
}

func redactedClientError(operation string, err error) error {
	var apiErr *APIError
	if errors.As(err, &apiErr) {
		code := strings.TrimSpace(apiErr.Code)
		if code == "" {
			code = http.StatusText(apiErr.StatusCode)
		}
		message := fmt.Sprintf("%s: omnigent API %s (HTTP %d)", operation, boundedText(redactSensitiveText(code), 80), apiErr.StatusCode)
		return &remoteRedactedError{message: message, cause: err}
	}
	return &remoteRedactedError{message: operation + ": omnigent transport failed", cause: err}
}

func (l *conversationLocks) acquire(id string) func() {
	l.mu.Lock()
	lock := l.locks[id]
	if lock == nil {
		lock = &conversationLock{}
		l.locks[id] = lock
	}
	lock.refs++
	l.mu.Unlock()
	lock.mu.Lock()
	return func() {
		lock.mu.Unlock()
		l.mu.Lock()
		lock.refs--
		if lock.refs == 0 {
			delete(l.locks, id)
		}
		l.mu.Unlock()
	}
}
