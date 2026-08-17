package omnigent

import "time"

// ServiceState is the bounded availability of the attachment-local Omnigent
// service. Raw workspace-service reasons never enter this vocabulary.
type ServiceState string

const (
	// ServiceStateReady means the attachment-local sidecar is available.
	ServiceStateReady ServiceState = "ready"
	// ServiceStateUnavailable means the attachment-local sidecar is unavailable.
	ServiceStateUnavailable ServiceState = "unavailable"
)

// StatusProfile is the public subset of one immutable catalog profile used by
// remote status. It deliberately excludes agent paths, environment names,
// secret references, and availability derived from controller credentials.
type StatusProfile struct {
	ID          string `json:"id"`
	DisplayName string `json:"display_name"`
	Blurb       string `json:"blurb"`
	Harness     string `json:"harness"`
	Backend     string `json:"backend"`
	Network     string `json:"network"`
}

// RemoteSessionStatus is the typed, non-secret operator projection of one Gas
// City session backed by Omnigent.
type RemoteSessionStatus struct {
	SessionID           string             `json:"session_id"`
	Alias               string             `json:"alias,omitempty"`
	Template            string             `json:"template,omitempty"`
	Provider            string             `json:"provider,omitempty"`
	Transport           string             `json:"transport,omitempty"`
	SessionState        string             `json:"session_state,omitempty"`
	Location            AttachmentLocation `json:"location"`
	ServiceState        ServiceState       `json:"service_state"`
	ConfiguredProfileID string             `json:"configured_profile_id"`
	ConfiguredProfile   *StatusProfile     `json:"configured_profile,omitempty"`
	ActiveProfileID     string             `json:"active_profile_id"`
	ActiveProfile       *StatusProfile     `json:"active_profile,omitempty"`
	ActiveIndex         int                `json:"active_index"`
	ConversationPresent bool               `json:"conversation_present"`
	Degradation         StatusDegradation  `json:"degradation"`
	Exhausted           bool               `json:"exhausted"`
	Stale               bool               `json:"stale"`
	ObservedAt          time.Time          `json:"observed_at"`
}

// StatusProfile returns one catalog profile's safe remote-status projection.
func (c *Catalog) StatusProfile(id string) (*StatusProfile, bool) {
	profile, ok := c.Profile(id)
	if !ok {
		return nil, false
	}
	return &StatusProfile{
		ID: profile.ID, DisplayName: profile.DisplayName, Blurb: profile.Blurb,
		Harness: profile.Harness, Backend: profile.Backend, Network: profile.Network,
	}, true
}

// ProjectRemoteSessionStatus combines a durable session snapshot with current
// public catalog metadata and bounded service availability.
func ProjectRemoteSessionStatus(record SessionStatusRecord, catalog *Catalog, serviceState ServiceState) RemoteSessionStatus {
	snapshot := record.Snapshot
	status := RemoteSessionStatus{
		SessionID: record.SessionID, Alias: record.Alias, Template: record.Template,
		Provider: record.Provider, Transport: record.Transport, SessionState: record.State,
		Location: snapshot.Location, ServiceState: serviceState,
		ConfiguredProfileID: snapshot.ConfiguredProfileID, ActiveProfileID: snapshot.ActiveProfileID,
		ActiveIndex: snapshot.ActiveIndex, ConversationPresent: record.ConversationPresent,
		Degradation: snapshot.Degradation, Exhausted: snapshot.Exhausted,
		ObservedAt: snapshot.ObservedAt.UTC(),
	}
	if status.Degradation == "" {
		status.Degradation = DegradationNone
	}
	status.Stale = record.Closed || !record.ConversationPresent
	if catalog == nil {
		status.Degradation = DegradationCatalogUnavailable
	} else {
		configured, configuredOK := catalog.StatusProfile(snapshot.ConfiguredProfileID)
		active, activeOK := catalog.StatusProfile(snapshot.ActiveProfileID)
		status.ConfiguredProfile = configured
		status.ActiveProfile = active
		if !configuredOK || !activeOK {
			status.Degradation = DegradationUnknownProfile
		}
	}
	if serviceState != ServiceStateReady {
		status.Degradation = DegradationServiceUnavailable
	}
	if status.Stale {
		status.Degradation = DegradationStale
	}
	return status
}
