package omnigent

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"time"
)

// AttachmentOpenInput identifies either a fresh profile conversation or an
// exact durable conversation to resume in the Gas City-owned workspace.
type AttachmentOpenInput struct {
	ProfileID      string          `json:"profile_id"`
	ConversationID string          `json:"conversation_id,omitempty"`
	Workspace      string          `json:"workspace"`
	Title          string          `json:"title,omitempty"`
	Identity       GasCityIdentity `json:"identity,omitempty"`
}

// GasCityIdentity is the explicit, non-secret worker context bound to one
// Omnigent conversation. Arbitrary process environment is never accepted.
type GasCityIdentity struct {
	SessionID string `json:"session_id"`
	Agent     string `json:"agent"`
	Rig       string `json:"rig,omitempty"`
	City      string `json:"city,omitempty"`
}

const (
	identitySessionLabel = "gascity.session.id"
	identityAgentLabel   = "gascity.agent"
	identityRigLabel     = "gascity.rig"
	identityCityLabel    = "gascity.city"
)

// AttachmentDescriptor is the bounded, non-secret result returned by the
// sidecar after it resolves a fresh or exact-resume attachment with its
// private catalog.
type AttachmentDescriptor struct {
	ConversationID string `json:"conversation_id"`
	ProfileID      string `json:"profile_id"`
	Fresh          bool   `json:"fresh"`
	ActiveProfile  string `json:"active_profile"`
	ActiveIndex    int    `json:"active_index"`
}

// Attachment is a subscribed interactive frontend over one durable Omnigent
// conversation. Closing it detaches only this frontend.
type Attachment struct {
	ConversationID string
	Fresh          bool
	Snapshot       Session
	State          FailoverState
	Stream         *EventStream
}

// ResolveAttachment asks the local compatibility sidecar to bind a profile
// using its private catalog. It never accepts an endpoint or credentials from
// the request and exact resume failures never create replacement sessions.
func (c *APIClient) ResolveAttachment(ctx context.Context, input AttachmentOpenInput) (AttachmentDescriptor, error) {
	if conversationID := strings.TrimSpace(input.ConversationID); conversationID != "" {
		if err := validateOpaqueID("conversation", conversationID); err != nil {
			return AttachmentDescriptor{}, err
		}
	}
	var descriptor AttachmentDescriptor
	if err := c.doJSON(ctx, "POST", "/gascity/v1/attachments", input, &descriptor); err != nil {
		return AttachmentDescriptor{}, err
	}
	if err := validateOpaqueID("conversation", descriptor.ConversationID); err != nil {
		return AttachmentDescriptor{}, err
	}
	if descriptor.ProfileID != strings.TrimSpace(input.ProfileID) {
		return AttachmentDescriptor{}, fmt.Errorf("omnigent attachment profile %q does not match requested profile %q", descriptor.ProfileID, input.ProfileID)
	}
	if input.ConversationID != "" && descriptor.ConversationID != strings.TrimSpace(input.ConversationID) {
		return AttachmentDescriptor{}, fmt.Errorf("omnigent attachment changed conversation id from %q to %q", input.ConversationID, descriptor.ConversationID)
	}
	if !profileIDPattern.MatchString(descriptor.ActiveProfile) || descriptor.ActiveIndex < 0 {
		return AttachmentDescriptor{}, errors.New("omnigent attachment returned invalid active profile state")
	}
	return descriptor, nil
}

// OpenResolvedAttachment establishes a live stream before reloading the
// authoritative snapshot returned through the public Omnigent API. The
// sidecar has already performed private-catalog binding; this edge revalidates
// exact conversation, profile, and workspace identity without catalog access.
func (c *APIClient) OpenResolvedAttachment(ctx context.Context, descriptor AttachmentDescriptor, input AttachmentOpenInput) (*Attachment, error) {
	if err := validateOpaqueID("conversation", descriptor.ConversationID); err != nil {
		return nil, err
	}
	workspace := filepath.Clean(strings.TrimSpace(input.Workspace))
	if !filepath.IsAbs(workspace) {
		return nil, errors.New("omnigent attachment workspace must be absolute")
	}
	identityLabels, err := gasCityIdentityLabels(input.Identity)
	if err != nil {
		return nil, err
	}
	stream, err := c.Subscribe(ctx, descriptor.ConversationID)
	if err != nil {
		return nil, err
	}
	closeOnError := func(err error) (*Attachment, error) {
		_ = stream.Close()
		return nil, err
	}
	snapshot, err := c.GetSession(ctx, descriptor.ConversationID)
	if err != nil {
		return closeOnError(redactedClientError("load subscribed omnigent conversation", err))
	}
	if snapshot.Archived {
		return closeOnError(fmt.Errorf("omnigent conversation %q is archived", descriptor.ConversationID))
	}
	if filepath.Clean(snapshot.Workspace) != workspace {
		return closeOnError(fmt.Errorf("omnigent conversation %q workspace %q does not match Gas City assignment %q", descriptor.ConversationID, snapshot.Workspace, workspace))
	}
	for key, expected := range identityLabels {
		if snapshot.Labels[key] != expected {
			return closeOnError(fmt.Errorf("omnigent conversation %q identity label %q changed while attaching", descriptor.ConversationID, key))
		}
	}
	chainIDs, active, lastSequence, err := parseFailoverLabels(snapshot.Labels)
	if err != nil {
		return closeOnError(fmt.Errorf("omnigent conversation %q has invalid profile state: %w", descriptor.ConversationID, err))
	}
	profileID := strings.TrimSpace(input.ProfileID)
	if chainIDs[0] != profileID || active != descriptor.ActiveIndex || chainIDs[active] != descriptor.ActiveProfile {
		return closeOnError(fmt.Errorf("omnigent conversation %q profile state changed while attaching", descriptor.ConversationID))
	}
	return &Attachment{
		ConversationID: descriptor.ConversationID,
		Fresh:          descriptor.Fresh,
		Snapshot:       snapshot,
		State: FailoverState{
			ConversationID: descriptor.ConversationID, ActiveIndex: active,
			ActiveProfileID: chainIDs[active], LastSequence: lastSequence,
		},
		Stream: stream,
	}, nil
}

// OpenAttachment creates or resumes one conversation, establishes its stream
// before loading the authoritative snapshot, and validates placement and
// immutable profile identity. Resume never falls back to fresh creation.
func OpenAttachment(ctx context.Context, client *APIClient, catalog *Catalog, input AttachmentOpenInput) (*Attachment, error) {
	if client == nil {
		return nil, errors.New("omnigent API client is required for attachment")
	}
	if catalog == nil {
		return nil, errors.New("omnigent profile catalog is required for attachment")
	}
	profileID := strings.TrimSpace(input.ProfileID)
	if !profileIDPattern.MatchString(profileID) {
		return nil, fmt.Errorf("invalid omnigent profile id %q", input.ProfileID)
	}
	workspace := filepath.Clean(strings.TrimSpace(input.Workspace))
	if !filepath.IsAbs(workspace) {
		return nil, errors.New("omnigent attachment workspace must be absolute")
	}
	identityLabels, err := gasCityIdentityLabels(input.Identity)
	if err != nil {
		return nil, err
	}

	agents, err := client.ListAgents(ctx)
	if err != nil {
		return nil, redactedClientError("list omnigent agents for attachment", err)
	}
	conversationID := strings.TrimSpace(input.ConversationID)
	fresh := conversationID == ""
	if fresh {
		chain, err := BindProfileChain(catalog, profileID, agents)
		if err != nil {
			return nil, err
		}
		labels, err := InitialFailoverLabels(catalog, profileID)
		if err != nil {
			return nil, err
		}
		for key, value := range identityLabels {
			labels[key] = value
		}
		created, err := client.CreateSession(ctx, CreateSessionInput{
			AgentID: chain[0].AgentID, Workspace: workspace,
			Title: strings.TrimSpace(input.Title), Labels: labels,
		})
		if err != nil {
			return nil, redactedClientError("create omnigent conversation", err)
		}
		conversationID = created.ID
	} else if err := validateOpaqueID("conversation", conversationID); err != nil {
		return nil, err
	}

	stream, err := client.Subscribe(ctx, conversationID)
	if err != nil {
		// Preserve typed 404 evidence so resume callers can report the exact
		// missing-conversation failure without creating a replacement.
		return nil, err
	}
	closeOnError := func(err error) (*Attachment, error) {
		_ = stream.Close()
		return nil, err
	}
	snapshot, err := client.GetSession(ctx, conversationID)
	if err != nil {
		return closeOnError(redactedClientError("load subscribed omnigent conversation", err))
	}
	if snapshot.Archived {
		return closeOnError(fmt.Errorf("omnigent conversation %q is archived", conversationID))
	}
	if filepath.Clean(snapshot.Workspace) != workspace {
		return closeOnError(fmt.Errorf("omnigent conversation %q workspace %q does not match Gas City assignment %q", conversationID, snapshot.Workspace, workspace))
	}
	for key, expected := range identityLabels {
		if snapshot.Labels[key] != expected {
			return closeOnError(fmt.Errorf("omnigent conversation %q identity label %q does not match Gas City assignment", conversationID, key))
		}
	}
	chainIDs, _, _, err := parseFailoverLabels(snapshot.Labels)
	if err != nil {
		return closeOnError(fmt.Errorf("omnigent conversation %q has invalid profile state: %w", conversationID, err))
	}
	if chainIDs[0] != profileID {
		return closeOnError(fmt.Errorf("omnigent conversation %q belongs to profile %q, not requested profile %q", conversationID, chainIDs[0], profileID))
	}
	controller, err := NewFailoverController(client, catalog, time.Now)
	if err != nil {
		return closeOnError(err)
	}
	state, err := controller.Reconcile(ctx, conversationID)
	if err != nil {
		return closeOnError(err)
	}
	return &Attachment{
		ConversationID: conversationID,
		Fresh:          fresh,
		Snapshot:       snapshot,
		State:          state,
		Stream:         stream,
	}, nil
}

func gasCityIdentityLabels(identity GasCityIdentity) (map[string]string, error) {
	fields := []struct {
		label string
		value string
	}{
		{identitySessionLabel, identity.SessionID},
		{identityAgentLabel, identity.Agent},
		{identityRigLabel, identity.Rig},
		{identityCityLabel, identity.City},
	}
	labels := make(map[string]string, len(fields))
	for _, field := range fields {
		value := strings.TrimSpace(field.value)
		if value == "" {
			continue
		}
		if len(value) > 256 {
			return nil, fmt.Errorf("gas city identity %q exceeds 256 bytes", field.label)
		}
		for _, char := range value {
			if char < 0x20 || char == 0x7f {
				return nil, fmt.Errorf("gas city identity %q contains control characters", field.label)
			}
		}
		labels[field.label] = value
	}
	return labels, nil
}

// Close detaches the interactive frontend without sending stop, interrupt, or
// delete operations to the Omnigent conversation.
func (a *Attachment) Close() error {
	if a == nil || a.Stream == nil {
		return nil
	}
	stream := a.Stream
	a.Stream = nil
	return stream.Close()
}
