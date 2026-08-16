package omnigent

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"
)

// LocalPinStatus is the non-secret executable identity exposed by the running
// city-local sidecar.
type LocalPinStatus struct {
	Executable     string `json:"executable"`
	ResolvedPath   string `json:"resolved_path,omitempty"`
	Commit         string `json:"commit"`
	PackageVersion string `json:"package_version"`
	SHA256         string `json:"sha256"`
}

// ProfileDiagnostic augments public profile discovery with missing local
// environment variable names. Values are never exposed.
type ProfileDiagnostic struct {
	PublicProfile
	MissingEnvironment []string `json:"missing_environment,omitempty"`
}

// ConversationStatus is the validated non-secret attachment projection owned
// by the local sidecar.
type ConversationStatus struct {
	ID              string              `json:"id"`
	Status          string              `json:"status"`
	Outcome         OutcomeKind         `json:"outcome"`
	Archived        bool                `json:"archived"`
	Workspace       string              `json:"workspace"`
	ActiveProfileID string              `json:"active_profile_id"`
	ActiveIndex     int                 `json:"active_index"`
	Chain           []ProfileDiagnostic `json:"chain"`
	LastSequence    int64               `json:"last_sequence,omitempty"`
	LastTransition  *ProfileTransition  `json:"last_transition,omitempty"`
	Exhausted       bool                `json:"exhausted"`
	Policy          *PolicyStatus       `json:"policy,omitempty"`
}

// PolicyStatus is the non-secret durable interaction fact exposed by status.
type PolicyStatus struct {
	RequestID string `json:"request_id"`
	Kind      string `json:"kind"`
	State     string `json:"state"`
	Pending   bool   `json:"pending"`
	MailBound bool   `json:"mail_bound"`
}

// LocalStatus is the typed status and explain surface of one running local
// Omnigent compatibility sidecar.
type LocalStatus struct {
	Mode         string              `json:"mode"`
	Ready        bool                `json:"ready"`
	Pin          LocalPinStatus      `json:"pin"`
	Profiles     []ProfileDiagnostic `json:"profiles"`
	Conversation *ConversationStatus `json:"conversation,omitempty"`
}

// LocalStatus returns the sidecar-owned local configuration and optional exact
// conversation status. An empty conversation ID returns configuration only.
func (c *APIClient) LocalStatus(ctx context.Context, conversationID string) (LocalStatus, error) {
	path := "/gascity/v1/status"
	if conversationID = strings.TrimSpace(conversationID); conversationID != "" {
		if err := validateOpaqueID("conversation", conversationID); err != nil {
			return LocalStatus{}, err
		}
		path += "?conversation_id=" + url.QueryEscape(conversationID)
	}
	var status LocalStatus
	if err := c.doJSON(ctx, http.MethodGet, path, nil, &status); err != nil {
		return LocalStatus{}, err
	}
	if status.Mode != "local" {
		return LocalStatus{}, fmt.Errorf("omnigent sidecar reported forbidden mode %q; only local mode is supported", status.Mode)
	}
	return status, nil
}

func localStatus(catalog *Catalog, verified VerifiedExecutable, lookup func(string) (string, bool)) LocalStatus {
	if lookup == nil {
		lookup = func(string) (string, bool) { return "", false }
	}
	public := catalog.PublicProfiles()
	profiles := make([]ProfileDiagnostic, 0, len(public))
	for _, profile := range public {
		resolved, _ := catalog.Profile(profile.ID)
		missing := make([]string, 0)
		for _, name := range resolved.Environment {
			if value, ok := lookup(name); !ok || strings.TrimSpace(value) == "" {
				missing = append(missing, name)
			}
		}
		sort.Strings(missing)
		profiles = append(profiles, ProfileDiagnostic{PublicProfile: profile, MissingEnvironment: missing})
	}
	return LocalStatus{
		Mode: "local", Ready: true,
		Pin: LocalPinStatus{
			Executable: redactSensitiveText(catalog.Pin.Executable), ResolvedPath: redactSensitiveText(verified.Path),
			Commit: catalog.Pin.Commit, PackageVersion: redactSensitiveText(catalog.Pin.PackageVersion),
			SHA256: catalog.Pin.SHA256,
		},
		Profiles: profiles,
	}
}

func conversationStatus(ctx context.Context, client *APIClient, controller *FailoverController, base LocalStatus, conversationID string) (*ConversationStatus, error) {
	snapshot, err := client.GetSession(ctx, conversationID)
	if err != nil {
		return nil, err
	}
	outcome := ClassifySessionStatus(snapshot.Status)
	if outcome == OutcomeUnknown {
		return nil, errors.New("omnigent conversation returned unsupported lifecycle status")
	}
	state, err := controller.Reconcile(ctx, conversationID)
	if err != nil {
		return nil, err
	}
	byID := make(map[string]ProfileDiagnostic, len(base.Profiles))
	for _, profile := range base.Profiles {
		byID[profile.ID] = profile
	}
	chain := make([]ProfileDiagnostic, 0, len(state.Chain))
	for _, bound := range state.Chain {
		profile, ok := byID[bound.ID]
		if !ok {
			return nil, fmt.Errorf("omnigent conversation %q references unavailable profile %q", conversationID, bound.ID)
		}
		chain = append(chain, profile)
	}
	exhausted, err := parseOptionalBool(snapshot.Labels[failoverExhaustedLabel])
	if err != nil {
		return nil, fmt.Errorf("omnigent conversation %q has invalid exhausted state", conversationID)
	}
	transition, err := statusTransition(snapshot.Labels, byID)
	if err != nil {
		return nil, fmt.Errorf("omnigent conversation %q has invalid transition status: %w", conversationID, err)
	}
	policy, err := policyStatusFromLabels(snapshot.Labels)
	if err != nil {
		return nil, fmt.Errorf("omnigent conversation %q has invalid policy status: %w", conversationID, err)
	}
	return &ConversationStatus{
		ID: conversationID, Status: strings.ToLower(strings.TrimSpace(snapshot.Status)), Outcome: outcome, Archived: snapshot.Archived,
		Workspace: redactSensitiveText(snapshot.Workspace), ActiveProfileID: state.ActiveProfileID,
		ActiveIndex: state.ActiveIndex, Chain: chain, LastSequence: state.LastSequence,
		LastTransition: transition, Exhausted: exhausted, Policy: policy,
	}, nil
}

func policyStatusFromLabels(labels map[string]string) (*PolicyStatus, error) {
	version := strings.TrimSpace(labels[policyVersionLabel])
	if version == "" {
		return nil, nil
	}
	if version != policyStateVersion {
		return nil, errors.New("unsupported policy state version")
	}
	requestID := strings.TrimSpace(labels[policyRequestIDLabel])
	if err := validateOpaqueID("policy request", requestID); err != nil {
		return nil, err
	}
	kind := strings.TrimSpace(labels[policyKindLabel])
	if !policyKindPattern.MatchString(kind) {
		return nil, errors.New("invalid policy kind")
	}
	state := strings.TrimSpace(labels[policyStatusLabel])
	switch state {
	case "pending", "delivering", "delivered", "canceled":
	default:
		return nil, errors.New("invalid policy state")
	}
	return &PolicyStatus{
		RequestID: requestID, Kind: kind, State: state,
		Pending:   state == "pending" || state == "delivering",
		MailBound: strings.TrimSpace(labels[policyMailIDLabel]) != "",
	}, nil
}

func statusTransition(labels map[string]string, profiles map[string]ProfileDiagnostic) (*ProfileTransition, error) {
	from := strings.TrimSpace(labels[failoverLastFromLabel])
	to := strings.TrimSpace(labels[failoverLastToLabel])
	reason := FailoverReason(strings.TrimSpace(labels[failoverLastReasonLabel]))
	atRaw := strings.TrimSpace(labels[failoverLastAtLabel])
	if from == "" && to == "" && reason == "" && atRaw == "" {
		return nil, nil
	}
	fromProfile, fromOK := profiles[from]
	toProfile, toOK := profiles[to]
	if !fromOK || !toOK || !validFailoverReason(reason) {
		return nil, errors.New("profile or reason is invalid")
	}
	at, err := time.Parse(time.RFC3339Nano, atRaw)
	if err != nil {
		return nil, errors.New("timestamp is invalid")
	}
	return &ProfileTransition{
		FromProfileID: from, FromBlurb: fromProfile.Blurb,
		ToProfileID: to, ToBlurb: toProfile.Blurb, Reason: reason, At: at,
	}, nil
}

func parseOptionalBool(value string) (bool, error) {
	switch strings.TrimSpace(value) {
	case "", "false":
		return false, nil
	case "true":
		return true, nil
	default:
		return false, errors.New("invalid boolean")
	}
}
