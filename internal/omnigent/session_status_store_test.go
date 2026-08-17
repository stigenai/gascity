package omnigent

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
)

func TestSessionStatusStoreRoundTripAndRedaction(t *testing.T) {
	store := beads.NewMemStore()
	created, err := store.Create(beads.Bead{
		Type: "session", Labels: []string{"gc:session"},
		Metadata: map[string]string{
			"state": "active", "alias": "worker", "provider": "omnigent",
			"template": "worker", "transport": "tmux", "session_key": "conv-secret-canary",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 8, 17, 12, 0, 0, 0, time.UTC)
	statusStore := NewSessionStatusStore(beads.SessionStore{Store: store})
	want := SessionStatusSnapshot{
		Version: 1, Location: AttachmentLocationCapsule,
		ConfiguredProfileID: "claude-primary", ActiveProfileID: "claude-secondary",
		ActiveIndex: 1, Degradation: DegradationRateLimit, ObservedAt: now,
	}
	if err := statusStore.Record(created.ID, want); err != nil {
		t.Fatalf("Record: %v", err)
	}

	records, err := statusStore.List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(records) != 1 {
		t.Fatalf("records = %#v, want one", records)
	}
	got := records[0]
	if got.SessionID != created.ID || got.Alias != "worker" || got.Provider != "omnigent" || got.Template != "worker" || got.Transport != "tmux" || got.State != "active" {
		t.Fatalf("record correlation = %#v", got)
	}
	if !got.ConversationPresent || got.Snapshot != want {
		t.Fatalf("record = %#v, want conversation presence and %#v", got, want)
	}

	encoded, err := json.Marshal(got)
	if err != nil {
		t.Fatal(err)
	}
	for _, canary := range []string{"conv-secret-canary", "session_key", "/var/lib", "TOKEN"} {
		if strings.Contains(string(encoded), canary) {
			t.Fatalf("public record leaked %q: %s", canary, encoded)
		}
	}
}

func TestSessionStatusStoreRejectsMalformedAndUnknownVersions(t *testing.T) {
	store := beads.NewMemStore()
	created, err := store.Create(beads.Bead{
		Type: "session", Labels: []string{"gc:session"},
		Metadata: map[string]string{"state": "active"},
	})
	if err != nil {
		t.Fatal(err)
	}
	statusStore := NewSessionStatusStore(beads.SessionStore{Store: store})
	bad := []SessionStatusSnapshot{
		{},
		{Version: 2, Location: AttachmentLocationController, ConfiguredProfileID: "profile", ActiveProfileID: "profile", ObservedAt: time.Now()},
		{Version: 1, Location: "remote-host", ConfiguredProfileID: "profile", ActiveProfileID: "profile", ObservedAt: time.Now()},
		{Version: 1, Location: AttachmentLocationController, ConfiguredProfileID: "../../secret", ActiveProfileID: "profile", ObservedAt: time.Now()},
		{Version: 1, Location: AttachmentLocationController, ConfiguredProfileID: "profile", ActiveProfileID: "profile", ActiveIndex: -1, ObservedAt: time.Now()},
		{Version: 1, Location: AttachmentLocationController, ConfiguredProfileID: "profile", ActiveProfileID: "profile", Degradation: "raw provider said token=canary", ObservedAt: time.Now()},
	}
	for _, snapshot := range bad {
		if err := statusStore.Record(created.ID, snapshot); err == nil {
			t.Errorf("Record(%#v) succeeded", snapshot)
		}
	}

	if err := store.SetMetadata(created.ID, SessionStatusMetadataKey, `{"version":9}`); err != nil {
		t.Fatal(err)
	}
	if _, err := statusStore.List(); err == nil || !strings.Contains(err.Error(), "unsupported") {
		t.Fatalf("List malformed status error = %v", err)
	}
}

func TestSessionStatusStoreListsOnlyOmnigentSnapshotsNewestFirst(t *testing.T) {
	store := beads.NewMemStore()
	first, _ := store.Create(beads.Bead{Type: "session", Labels: []string{"gc:session"}, Metadata: map[string]string{"state": "closed"}})
	_, _ = store.Create(beads.Bead{Type: "session", Labels: []string{"gc:session"}, Metadata: map[string]string{"state": "active"}})
	second, _ := store.Create(beads.Bead{Type: "session", Labels: []string{"gc:session"}, Metadata: map[string]string{"state": "awake"}})
	statusStore := NewSessionStatusStore(beads.SessionStore{Store: store})
	for _, id := range []string{first.ID, second.ID} {
		if err := statusStore.Record(id, SessionStatusSnapshot{
			Version: 1, Location: AttachmentLocationController,
			ConfiguredProfileID: "profile", ActiveProfileID: "profile", ObservedAt: time.Now().UTC(),
		}); err != nil {
			t.Fatal(err)
		}
	}
	records, err := statusStore.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != 2 || records[0].SessionID != second.ID || records[1].SessionID != first.ID {
		t.Fatalf("records = %#v, want newest snapshot session first", records)
	}
}
