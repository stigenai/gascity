package api

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/session"
)

func TestOmnigentCapsuleStateInspectPreviewAndPurge(t *testing.T) {
	t.Parallel()

	state := newFakeState(t)
	store := beads.NewMemStore()
	provider := runtime.NewFake()
	state.sessionsBeadStore = store
	state.cityBeadStore = store
	state.sp = provider

	created, err := store.Create(beads.Bead{
		Title:  "capsule",
		Type:   session.BeadType,
		Labels: []string{session.LabelSession},
		Metadata: map[string]string{
			"state":        string(session.StateArchived),
			"session_name": "remote-capsule-place",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	key, err := runtime.NewCapsuleKey(state.CityName(), created.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.SetMetadataBatch(created.ID, session.CapsuleStateMetadata(key)); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(created.ID); err != nil {
		t.Fatal(err)
	}
	if _, _, err := provider.EnsureCapsuleState(context.Background(), key); err != nil {
		t.Fatal(err)
	}

	h := New(state)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, cityURL(state, "/omnigent/capsule-state"), nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("inspect status = %d body=%s", rec.Code, rec.Body.String())
	}
	var inspected OmnigentCapsuleStateReportBody
	if err := json.NewDecoder(rec.Body).Decode(&inspected); err != nil {
		t.Fatal(err)
	}
	if inspected.DryRun != true || len(inspected.Items) != 1 || inspected.Items[0].SessionID != created.ID || inspected.Items[0].Action != string(session.CapsuleStateRetained) {
		t.Fatalf("inspect report = %#v", inspected)
	}

	previewBody := bytes.NewBufferString(`{"dry_run":true}`)
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, newPostRequest(cityURL(state, "/omnigent/capsule-state/"+created.ID+"/purge"), previewBody))
	if rec.Code != http.StatusOK {
		t.Fatalf("preview status = %d body=%s", rec.Code, rec.Body.String())
	}
	var preview OmnigentCapsuleStateReportBody
	if err := json.NewDecoder(rec.Body).Decode(&preview); err != nil {
		t.Fatal(err)
	}
	if !preview.DryRun || len(preview.Items) != 1 || preview.Items[0].Action != string(session.CapsuleStateWouldPurge) {
		t.Fatalf("preview report = %#v", preview)
	}
	if _, ok, err := provider.OpenCapsuleState(context.Background(), key); err != nil || !ok {
		t.Fatalf("preview state exists=%t err=%v", ok, err)
	}

	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, newPostRequest(cityURL(state, "/omnigent/capsule-state/"+created.ID+"/purge"), bytes.NewBufferString(`{"dry_run":false}`)))
	if rec.Code != http.StatusOK {
		t.Fatalf("purge status = %d body=%s", rec.Code, rec.Body.String())
	}
	var purged OmnigentCapsuleStateReportBody
	if err := json.NewDecoder(rec.Body).Decode(&purged); err != nil {
		t.Fatal(err)
	}
	if purged.DryRun || len(purged.Items) != 1 || purged.Items[0].Action != string(session.CapsuleStatePurged) {
		t.Fatalf("purge report = %#v", purged)
	}
	if _, ok, err := provider.OpenCapsuleState(context.Background(), key); err != nil || ok {
		t.Fatalf("purged state exists=%t err=%v", ok, err)
	}
}

func TestOmnigentCapsuleStatePurgeRejectsOpenSession(t *testing.T) {
	t.Parallel()

	state := newFakeState(t)
	store := beads.NewMemStore()
	provider := runtime.NewFake()
	state.sessionsBeadStore = store
	state.cityBeadStore = store
	state.sp = provider
	created, err := store.Create(beads.Bead{
		Title: "capsule", Type: session.BeadType, Labels: []string{session.LabelSession},
		Metadata: map[string]string{"state": string(session.StateArchived), "session_name": "remote-capsule-place"},
	})
	if err != nil {
		t.Fatal(err)
	}
	key, err := runtime.NewCapsuleKey(state.CityName(), created.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.SetMetadataBatch(created.ID, session.CapsuleStateMetadata(key)); err != nil {
		t.Fatal(err)
	}
	if _, _, err := provider.EnsureCapsuleState(context.Background(), key); err != nil {
		t.Fatal(err)
	}

	rec := httptest.NewRecorder()
	New(state).ServeHTTP(rec, newPostRequest(
		cityURL(state, "/omnigent/capsule-state/"+created.ID+"/purge"),
		bytes.NewBufferString(`{"dry_run":false}`),
	))
	if rec.Code != http.StatusConflict {
		t.Fatalf("open-session purge status = %d body=%s", rec.Code, rec.Body.String())
	}
}

func TestOmnigentCapsuleStateClientRoundTrip(t *testing.T) {
	t.Parallel()

	state := newFakeState(t)
	store := beads.NewMemStore()
	provider := runtime.NewFake()
	state.sessionsBeadStore = store
	state.cityBeadStore = store
	state.sp = provider
	created, err := store.Create(beads.Bead{
		Title: "capsule", Type: session.BeadType, Labels: []string{session.LabelSession},
		Metadata: map[string]string{"state": string(session.StateArchived), "session_name": "remote-capsule-place"},
	})
	if err != nil {
		t.Fatal(err)
	}
	key, err := runtime.NewCapsuleKey(state.CityName(), created.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.SetMetadataBatch(created.ID, session.CapsuleStateMetadata(key)); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(created.ID); err != nil {
		t.Fatal(err)
	}
	if _, _, err := provider.EnsureCapsuleState(context.Background(), key); err != nil {
		t.Fatal(err)
	}

	srv := newLoopbackTestServer(t, New(state))
	client := NewCityScopedClient(srv.URL, state.CityName())
	inspected, err := client.InspectOmnigentCapsuleState()
	if err != nil {
		t.Fatalf("InspectOmnigentCapsuleState: %v", err)
	}
	if len(inspected.Items) != 1 || inspected.Items[0].SessionID != created.ID {
		t.Fatalf("inspect = %#v", inspected)
	}
	preview, err := client.PurgeOmnigentCapsuleState(created.ID, true)
	if err != nil {
		t.Fatalf("PurgeOmnigentCapsuleState(dry-run): %v", err)
	}
	if !preview.DryRun || len(preview.Items) != 1 || preview.Items[0].Action != string(session.CapsuleStateWouldPurge) {
		t.Fatalf("preview = %#v", preview)
	}
}
