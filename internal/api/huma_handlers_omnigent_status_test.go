package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/api/genclient"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/omnigent"
	"github.com/gastownhall/gascity/internal/workspacesvc"
)

func TestOmnigentStatusProjectsProfilesFailoverAndRedactsConversation(t *testing.T) {
	state := newFakeState(t)
	state.cityPath = writeOmnigentStatusCatalog(t)
	store := beads.NewMemStore()
	state.sessionsBeadStore = store
	state.services = &fakeServiceRegistry{items: []workspacesvc.Status{{
		ServiceName: "omnigent", State: "ready", LocalState: "ready", Visibility: "private",
	}}}
	created, err := store.Create(beads.Bead{
		Type: "session", Labels: []string{"gc:session"},
		Metadata: map[string]string{
			"state": "active", "alias": "reviewer", "provider": "omnigent", "template": "reviewer",
			"transport": "tmux", "session_key": "conv-private-canary",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	statusStore := omnigent.NewSessionStatusStore(beads.SessionStore{Store: store})
	if err := statusStore.Record(created.ID, omnigent.SessionStatusSnapshot{
		Version: 1, Location: omnigent.AttachmentLocationController,
		ConfiguredProfileID: "claude-primary", ActiveProfileID: "claude-secondary", ActiveIndex: 1,
		Degradation: omnigent.DegradationRateLimit, ObservedAt: time.Date(2026, 8, 17, 12, 0, 0, 0, time.UTC),
	}); err != nil {
		t.Fatal(err)
	}

	rec := httptest.NewRecorder()
	newTestCityHandler(t, state).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, cityURL(state, "/omnigent/status"), nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	var body ListBody[omnigent.RemoteSessionStatus]
	if err := json.NewDecoder(rec.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	if body.Total != 1 || len(body.Items) != 1 {
		t.Fatalf("body = %#v", body)
	}
	got := body.Items[0]
	if got.SessionID != created.ID || got.Alias != "reviewer" || got.ServiceState != omnigent.ServiceStateReady || !got.ConversationPresent {
		t.Fatalf("correlation = %#v", got)
	}
	if got.ConfiguredProfile == nil || got.ConfiguredProfile.Blurb != "Primary public blurb." || got.ActiveProfile == nil || got.ActiveProfile.Blurb != "Secondary public blurb." {
		t.Fatalf("profiles = configured %#v active %#v", got.ConfiguredProfile, got.ActiveProfile)
	}
	if got.ActiveIndex != 1 || got.Degradation != omnigent.DegradationRateLimit || got.Stale {
		t.Fatalf("state = %#v", got)
	}
	for _, canary := range []string{"conv-private-canary", "session_key", "SECRET_TOKEN", "/private/workspace", "provider raw error"} {
		if strings.Contains(rec.Body.String(), canary) {
			t.Fatalf("response leaked %q: %s", canary, rec.Body.String())
		}
	}
}

func TestOmnigentStatusReportsUnavailableUnknownAndStaleWithoutRawErrors(t *testing.T) {
	state := newFakeState(t)
	state.cityPath = writeOmnigentStatusCatalog(t)
	store := beads.NewMemStore()
	state.sessionsBeadStore = store
	created, _ := store.Create(beads.Bead{
		Type: "session", Labels: []string{"gc:session"},
		Metadata: map[string]string{"state": "active", "provider": "omnigent", "session_key": "secret"},
	})
	if err := omnigent.NewSessionStatusStore(beads.SessionStore{Store: store}).Record(created.ID, omnigent.SessionStatusSnapshot{
		Version: 1, Location: omnigent.AttachmentLocationController,
		ConfiguredProfileID: "removed-profile", ActiveProfileID: "removed-profile", ObservedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(created.ID); err != nil {
		t.Fatal(err)
	}

	rec := httptest.NewRecorder()
	newTestCityHandler(t, state).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, cityURL(state, "/omnigent/status"), nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	var body ListBody[omnigent.RemoteSessionStatus]
	if err := json.NewDecoder(rec.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	got := body.Items[0]
	if got.ServiceState != omnigent.ServiceStateUnavailable || !got.Stale || got.Degradation != omnigent.DegradationStale {
		t.Fatalf("degraded status = %#v", got)
	}
	if got.ConfiguredProfile != nil || got.ActiveProfile != nil || got.ConfiguredProfileID != "removed-profile" {
		t.Fatalf("unknown profile projection = %#v", got)
	}
}

func TestOmnigentStatusUsesKeysetPagination(t *testing.T) {
	state := newFakeState(t)
	state.cityPath = writeOmnigentStatusCatalog(t)
	store := beads.NewMemStore()
	state.sessionsBeadStore = store
	statusStore := omnigent.NewSessionStatusStore(beads.SessionStore{Store: store})
	for i := 0; i < 2; i++ {
		created, _ := store.Create(beads.Bead{Type: "session", Labels: []string{"gc:session"}, Metadata: map[string]string{"state": "active"}})
		if err := statusStore.Record(created.ID, omnigent.NewSessionStatusSnapshot(omnigent.AttachmentLocationCapsule, "claude-primary", "claude-primary", 0, time.Now())); err != nil {
			t.Fatal(err)
		}
	}
	h := newTestCityHandler(t, state)
	first := httptest.NewRecorder()
	h.ServeHTTP(first, httptest.NewRequest(http.MethodGet, cityURL(state, "/omnigent/status?limit=1"), nil))
	var page1 ListBody[omnigent.RemoteSessionStatus]
	if err := json.NewDecoder(first.Body).Decode(&page1); err != nil {
		t.Fatal(err)
	}
	if first.Code != http.StatusOK || len(page1.Items) != 1 || page1.Total != 2 || page1.NextCursor == "" {
		t.Fatalf("first page status=%d body=%#v", first.Code, page1)
	}
	second := httptest.NewRecorder()
	h.ServeHTTP(second, httptest.NewRequest(http.MethodGet, cityURL(state, "/omnigent/status?limit=1&cursor="+page1.NextCursor), nil))
	var page2 ListBody[omnigent.RemoteSessionStatus]
	if err := json.NewDecoder(second.Body).Decode(&page2); err != nil {
		t.Fatal(err)
	}
	if second.Code != http.StatusOK || len(page2.Items) != 1 || page2.Items[0].SessionID == page1.Items[0].SessionID || page2.NextCursor != "" {
		t.Fatalf("second page status=%d body=%#v", second.Code, page2)
	}
}

func TestOmnigentStatusRequiresAndAcceptsRemoteReadGrant(t *testing.T) {
	state := newFakeState(t)
	state.cityPath = writeOmnigentStatusCatalog(t)
	state.sessionsBeadStore = beads.NewMemStore()
	now := time.Unix(1_787_000_000, 0)
	pub, priv := mustKeypair(t)
	sm := NewSupervisorMux(&stateCityResolver{state: state}, nil, false, "test", "", now).
		WithAllowedHosts([]string{"example.com"}).
		WithReadAuth(newTestReadVerifier(t, pub, now))
	path := cityURL(state, "/omnigent/status")

	missing := httptest.NewRecorder()
	sm.Handler().ServeHTTP(missing, httptest.NewRequest(http.MethodGet, path, nil))
	if missing.Code != http.StatusUnauthorized {
		t.Fatalf("missing grant status = %d, want 401", missing.Code)
	}

	token := mintToken(t, priv, readGrant(now, state.CityName(), http.MethodGet, path, "", "omnigent-status-read"))
	req := httptest.NewRequest(http.MethodGet, path, nil)
	req.Header.Set(readAuthHeader, token)
	accepted := httptest.NewRecorder()
	sm.Handler().ServeHTTP(accepted, req)
	if accepted.Code != http.StatusOK {
		t.Fatalf("valid grant status = %d body=%s", accepted.Code, accepted.Body.String())
	}
}

func TestOmnigentStatusCatalogUnavailableIsTypedPartialWithoutPathLeak(t *testing.T) {
	state := newFakeState(t)
	state.cityPath = filepath.Join(t.TempDir(), "private-token-canary")
	store := beads.NewMemStore()
	state.sessionsBeadStore = store
	created, _ := store.Create(beads.Bead{Type: "session", Labels: []string{"gc:session"}, Metadata: map[string]string{"state": "active", "session_key": "secret"}})
	if err := omnigent.NewSessionStatusStore(beads.SessionStore{Store: store}).Record(created.ID,
		omnigent.NewSessionStatusSnapshot(omnigent.AttachmentLocationCapsule, "claude-primary", "claude-primary", 0, time.Now())); err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	newTestCityHandler(t, state).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, cityURL(state, "/omnigent/status"), nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	var body ListBody[omnigent.RemoteSessionStatus]
	if err := json.NewDecoder(rec.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	if !body.Partial || len(body.PartialErrors) != 1 || body.Items[0].Degradation != omnigent.DegradationCatalogUnavailable {
		t.Fatalf("body = %#v", body)
	}
	if strings.Contains(rec.Body.String(), "private-token-canary") || strings.Contains(rec.Body.String(), "profiles.yaml") {
		t.Fatalf("catalog path leaked: %s", rec.Body.String())
	}
}

func TestOmnigentStatusGeneratedClientConversion(t *testing.T) {
	alias := "reviewer"
	got := omnigentStatusFromGen(genclient.RemoteSessionStatus{
		SessionId: "gc-1", Alias: &alias, Location: "capsule", ServiceState: "ready",
		ConfiguredProfileId: "claude-primary", ActiveProfileId: "claude-secondary", ActiveIndex: 1,
		ConversationPresent: true, Degradation: "rate_limit", ObservedAt: time.Now().UTC(),
		ConfiguredProfile: &genclient.StatusProfile{Id: "claude-primary", Blurb: "Primary"},
	})
	if got.SessionID != "gc-1" || got.Alias != alias || got.Location != omnigent.AttachmentLocationCapsule || got.ConfiguredProfile == nil || got.ConfiguredProfile.Blurb != "Primary" {
		t.Fatalf("got = %#v", got)
	}
}

func writeOmnigentStatusCatalog(t *testing.T) string {
	t.Helper()
	cityPath := t.TempDir()
	root := filepath.Join(cityPath, ".gc", "services", "omnigent", "config")
	if err := os.MkdirAll(filepath.Join(root, "agents"), 0o755); err != nil {
		t.Fatal(err)
	}
	agent := "name: fixture\ndescription: fixture\nexecutor:\n  harness: claude-sdk\n  model: fixture/model\n  auth:\n    type: provider\n    name: fixture\nprompt: work\n"
	for _, name := range []string{"primary.yaml", "secondary.yaml"} {
		if err := os.WriteFile(filepath.Join(root, "agents", name), []byte(agent), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	catalog := `version: 1
omnigent:
  commit: 0123456789abcdef0123456789abcdef01234567
  package_version: 0.10.0
  executable: omnigent
  sha256: sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef
profiles:
  claude-primary:
    display_name: Claude primary
    blurb: Primary public blurb.
    harness: claude-sdk
    backend: compatible-primary
    network: external-model
    agent: agents/primary.yaml
    fallbacks: [claude-secondary]
  claude-secondary:
    display_name: Claude secondary
    blurb: Secondary public blurb.
    harness: claude-sdk
    backend: compatible-secondary
    network: external-model
    agent: agents/secondary.yaml
`
	if err := os.WriteFile(filepath.Join(root, "profiles.yaml"), []byte(catalog), 0o600); err != nil {
		t.Fatal(err)
	}
	return cityPath
}
