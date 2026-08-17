package omnigent

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/session"
)

const (
	// SessionStatusMetadataKey is the single versioned, non-secret Omnigent
	// status document stored on a Gas City session bead.
	SessionStatusMetadataKey = "gascity.omnigent.status"
	statusSnapshotVersion    = 1
)

// StatusDegradation is the bounded public reason why an Omnigent attachment is
// operating below its configured profile or is unavailable.
type StatusDegradation string

const (
	// DegradationNone means no degradation has been observed.
	DegradationNone StatusDegradation = "none"
	// DegradationAuthentication means the selected backend rejected authentication.
	DegradationAuthentication StatusDegradation = "authentication"
	// DegradationRateLimit means the selected backend exhausted request capacity.
	DegradationRateLimit StatusDegradation = "rate_limit"
	// DegradationBackendUnavailable means the selected backend was unavailable.
	DegradationBackendUnavailable StatusDegradation = "backend_unavailable"
	// DegradationExhausted means every configured fallback was exhausted.
	DegradationExhausted StatusDegradation = "exhausted"
	// DegradationServiceUnavailable means the owning local service is unavailable.
	DegradationServiceUnavailable StatusDegradation = "service_unavailable"
	// DegradationCatalogUnavailable means public profile metadata cannot be loaded.
	DegradationCatalogUnavailable StatusDegradation = "catalog_unavailable"
	// DegradationUnknownProfile means a stored profile is absent from the current catalog.
	DegradationUnknownProfile StatusDegradation = "unknown_profile"
	// DegradationStale means the Gas City session is closed or lost its conversation binding.
	DegradationStale StatusDegradation = "stale"
)

// SessionStatusSnapshot is the durable, non-secret status written by an
// attached Omnigent frontend. It intentionally contains no conversation key,
// path, credential reference, environment name, transcript, or provider text.
type SessionStatusSnapshot struct {
	Version             int                `json:"version"`
	Location            AttachmentLocation `json:"location"`
	ConfiguredProfileID string             `json:"configured_profile_id"`
	ActiveProfileID     string             `json:"active_profile_id"`
	ActiveIndex         int                `json:"active_index"`
	Degradation         StatusDegradation  `json:"degradation"`
	Exhausted           bool               `json:"exhausted"`
	ObservedAt          time.Time          `json:"observed_at"`
}

// SessionStatusRecord correlates one non-secret Omnigent snapshot with its Gas
// City session. ConversationPresent is derived from the opaque session key;
// the key itself never leaves this boundary.
type SessionStatusRecord struct {
	SessionID           string                `json:"session_id"`
	Alias               string                `json:"alias,omitempty"`
	Template            string                `json:"template,omitempty"`
	Provider            string                `json:"provider,omitempty"`
	Transport           string                `json:"transport,omitempty"`
	State               string                `json:"state,omitempty"`
	Closed              bool                  `json:"closed,omitempty"`
	CreatedAt           time.Time             `json:"created_at"`
	ConversationPresent bool                  `json:"conversation_present"`
	Snapshot            SessionStatusSnapshot `json:"snapshot"`
}

// SessionStatusStore confines serialization of Omnigent status metadata to a
// provider-owned adapter over the session-class Beads store.
type SessionStatusStore struct {
	store beads.SessionStore
}

// NewSessionStatusStore creates a typed Omnigent status adapter.
func NewSessionStatusStore(store beads.SessionStore) *SessionStatusStore {
	return &SessionStatusStore{store: store}
}

// NewSessionStatusSnapshot returns a valid initial snapshot for one attachment.
func NewSessionStatusSnapshot(location AttachmentLocation, configuredProfileID, activeProfileID string, activeIndex int, observedAt time.Time) SessionStatusSnapshot {
	return SessionStatusSnapshot{
		Version: statusSnapshotVersion, Location: location,
		ConfiguredProfileID: strings.TrimSpace(configuredProfileID),
		ActiveProfileID:     strings.TrimSpace(activeProfileID), ActiveIndex: activeIndex,
		Degradation: DegradationNone, ObservedAt: observedAt.UTC(),
	}
}

// Record validates and atomically replaces one session's public snapshot.
func (s *SessionStatusStore) Record(sessionID string, snapshot SessionStatusSnapshot) error {
	if s == nil || s.store.Store == nil {
		return errors.New("omnigent session status store is unavailable")
	}
	if err := snapshot.Validate(); err != nil {
		return err
	}
	bead, err := s.store.Get(strings.TrimSpace(sessionID))
	if err != nil {
		return fmt.Errorf("load Omnigent status session: %w", err)
	}
	if !session.IsSessionBeadOrRepairable(bead) {
		return errors.New("omnigent status target is not a session")
	}
	encoded, err := json.Marshal(snapshot)
	if err != nil {
		return fmt.Errorf("encode Omnigent session status: %w", err)
	}
	if err := s.store.SetMetadata(bead.ID, SessionStatusMetadataKey, string(encoded)); err != nil {
		return fmt.Errorf("record Omnigent session status: %w", err)
	}
	return nil
}

// List returns every session carrying a valid Omnigent snapshot, newest first.
func (s *SessionStatusStore) List() ([]SessionStatusRecord, error) {
	if s == nil || s.store.Store == nil {
		return nil, errors.New("omnigent session status store is unavailable")
	}
	all, err := s.store.List(beads.ListQuery{
		Label: session.LabelSession, Sort: beads.SortCreatedDesc, IncludeClosed: true,
	})
	if err != nil {
		return nil, fmt.Errorf("list Omnigent status sessions: %w", err)
	}
	records := make([]SessionStatusRecord, 0)
	for _, bead := range all {
		raw := strings.TrimSpace(bead.Metadata[SessionStatusMetadataKey])
		if raw == "" || !session.IsSessionBeadOrRepairable(bead) {
			continue
		}
		snapshot, err := decodeSessionStatusSnapshot(raw)
		if err != nil {
			return nil, fmt.Errorf("decode Omnigent status for session %q: %w", bead.ID, err)
		}
		records = append(records, SessionStatusRecord{
			SessionID: bead.ID, Alias: strings.TrimSpace(bead.Metadata["alias"]),
			Template: strings.TrimSpace(bead.Metadata["template"]), Provider: strings.TrimSpace(bead.Metadata["provider"]),
			Transport: strings.TrimSpace(bead.Metadata["transport"]), State: strings.TrimSpace(bead.Metadata["state"]),
			Closed: bead.Status == "closed", CreatedAt: bead.CreatedAt,
			ConversationPresent: strings.TrimSpace(bead.Metadata["session_key"]) != "", Snapshot: snapshot,
		})
	}
	return records, nil
}

// Validate checks the complete versioned snapshot contract.
func (s SessionStatusSnapshot) Validate() error {
	if s.Version != statusSnapshotVersion {
		return fmt.Errorf("unsupported Omnigent session status version %d", s.Version)
	}
	if s.Location != AttachmentLocationController && s.Location != AttachmentLocationCapsule {
		return errors.New("invalid Omnigent session status location")
	}
	if !profileIDPattern.MatchString(strings.TrimSpace(s.ConfiguredProfileID)) || !profileIDPattern.MatchString(strings.TrimSpace(s.ActiveProfileID)) {
		return errors.New("invalid Omnigent session status profile")
	}
	if s.ActiveIndex < 0 {
		return errors.New("invalid Omnigent session status active index")
	}
	if !validStatusDegradation(s.Degradation) {
		return errors.New("invalid Omnigent session status degradation")
	}
	if s.ObservedAt.IsZero() {
		return errors.New("invalid Omnigent session status observation time")
	}
	if s.Exhausted && s.Degradation != DegradationExhausted {
		return errors.New("exhausted Omnigent session status requires exhausted degradation")
	}
	return nil
}

func validStatusDegradation(value StatusDegradation) bool {
	switch value {
	case "", DegradationNone, DegradationAuthentication, DegradationRateLimit, DegradationBackendUnavailable, DegradationExhausted,
		DegradationServiceUnavailable, DegradationCatalogUnavailable, DegradationUnknownProfile, DegradationStale:
		return true
	default:
		return false
	}
}

func decodeSessionStatusSnapshot(raw string) (SessionStatusSnapshot, error) {
	decoder := json.NewDecoder(bytes.NewBufferString(raw))
	decoder.DisallowUnknownFields()
	var snapshot SessionStatusSnapshot
	if err := decoder.Decode(&snapshot); err != nil {
		return SessionStatusSnapshot{}, err
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			return SessionStatusSnapshot{}, errors.New("multiple Omnigent status documents")
		}
		return SessionStatusSnapshot{}, errors.New("invalid trailing Omnigent status data")
	}
	if err := snapshot.Validate(); err != nil {
		return SessionStatusSnapshot{}, err
	}
	return snapshot, nil
}

// DegradationFromFailoverReason maps the private failover machinery onto the
// bounded status vocabulary.
func DegradationFromFailoverReason(reason FailoverReason) StatusDegradation {
	switch reason {
	case FailoverAuthentication:
		return DegradationAuthentication
	case FailoverRateLimit:
		return DegradationRateLimit
	case FailoverBackendUnavailable:
		return DegradationBackendUnavailable
	default:
		return DegradationNone
	}
}
