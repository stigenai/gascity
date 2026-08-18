package session

import (
	"context"
	"errors"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/runtime"
)

func TestManagerStartPersistsCapsuleStateIdentity(t *testing.T) {
	t.Parallel()

	store := beads.NewMemStore()
	created, err := store.Create(beads.Bead{
		ID:     "session-capsule",
		Title:  "capsule",
		Type:   BeadType,
		Labels: []string{LabelSession},
		Metadata: map[string]string{
			"state":        string(StateAsleep),
			"template":     "worker",
			"provider":     "fake",
			"command":      "agent",
			"work_dir":     t.TempDir(),
			"session_name": "capsule-place",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	key, err := runtime.NewCapsuleKey("capsule-city", created.ID)
	if err != nil {
		t.Fatal(err)
	}

	mgr := NewManagerWithOptions(store, runtime.NewFake())
	if err := mgr.Start(context.Background(), created.ID, "agent", runtime.Config{
		Capsule: &runtime.CapsuleLaunchConfig{Key: key},
	}); err != nil {
		t.Fatalf("Start: %v", err)
	}

	got, err := store.Get(created.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Metadata[CapsuleStateCityScopeMetadataKey] != key.CityScope ||
		got.Metadata[CapsuleStateDigestMetadataKey] != key.Digest {
		t.Fatalf("capsule state metadata = %#v, want scope %q digest %q", got.Metadata, key.CityScope, key.Digest)
	}
}

func TestManagerStartRejectsCapsuleStateIdentityChangeBeforeProviderStart(t *testing.T) {
	t.Parallel()

	store := beads.NewMemStore()
	created, err := store.Create(beads.Bead{
		Title: "capsule", Type: BeadType, Labels: []string{LabelSession},
		Metadata: map[string]string{
			"state": string(StateAsleep), "template": "worker", "provider": "fake",
			"command": "agent", "work_dir": t.TempDir(), "session_name": "capsule-place",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	original, err := runtime.NewCapsuleKey("capsule-city", created.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.SetMetadataBatch(created.ID, CapsuleStateMetadata(original)); err != nil {
		t.Fatal(err)
	}
	changed, err := runtime.NewCapsuleKey("different-city", created.ID)
	if err != nil {
		t.Fatal(err)
	}
	provider := runtime.NewFake()
	err = NewManagerWithOptions(store, provider).Start(context.Background(), created.ID, "agent", runtime.Config{
		Capsule: &runtime.CapsuleLaunchConfig{Key: changed},
	})
	if !errors.Is(err, runtime.ErrCapsuleStateConflict) {
		t.Fatalf("Start error = %v, want ErrCapsuleStateConflict", err)
	}
	if provider.CountCalls("Start", "capsule-place") != 0 {
		t.Fatal("provider started after capsule identity conflict")
	}
}

func TestCapsuleStateControlInspectPreviewAndPurge(t *testing.T) {
	t.Parallel()

	store := beads.NewMemStore()
	provider := runtime.NewFake()
	closedKey := createCapsuleStateControlSession(t, store, provider, "closed", true, false)
	liveKey := createCapsuleStateControlSession(t, store, provider, "live", false, true)
	ordinary, err := store.Create(beads.Bead{
		ID:       "ordinary-closed",
		Title:    "ordinary",
		Type:     BeadType,
		Labels:   []string{LabelSession},
		Metadata: map[string]string{"state": string(StateArchived), "session_name": "ordinary-place"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Close(ordinary.ID); err != nil {
		t.Fatal(err)
	}

	control := NewCapsuleStateControl(beads.SessionStore{Store: store}, provider)
	report, err := control.Inspect(context.Background(), "capsule-city")
	if err != nil {
		t.Fatalf("Inspect: %v", err)
	}
	if got := capsuleStateActions(report); len(got) != 2 || got[closedKey.SessionID] != CapsuleStateRetained || got[liveKey.SessionID] != CapsuleStateRetained {
		t.Fatalf("inspect actions = %#v, want only marked closed/live sessions", got)
	}

	preview, err := control.Purge(context.Background(), "capsule-city", closedKey.SessionID, true)
	if err != nil {
		t.Fatalf("Purge(dry-run): %v", err)
	}
	if got := capsuleStateActions(preview)[closedKey.SessionID]; got != CapsuleStateWouldPurge {
		t.Fatalf("dry-run action = %q, want %q", got, CapsuleStateWouldPurge)
	}
	if _, ok, err := provider.OpenCapsuleState(context.Background(), closedKey); err != nil || !ok {
		t.Fatalf("dry-run state exists = %t, err=%v", ok, err)
	}
	closed, err := store.Get(closedKey.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	if closed.Metadata[CapsuleStatePurgeAuthorizedMetadataKey] != "" || closed.Metadata[CapsuleStatePurgeCompletedMetadataKey] != "" {
		t.Fatalf("dry-run mutated durable authorization: %#v", closed.Metadata)
	}

	report, err = control.Purge(context.Background(), "capsule-city", closedKey.SessionID, false)
	if err != nil {
		t.Fatalf("Purge: %v", err)
	}
	if got := capsuleStateActions(report)[closedKey.SessionID]; got != CapsuleStatePurged {
		t.Fatalf("purge action = %q, want %q", got, CapsuleStatePurged)
	}
	if _, ok, err := provider.OpenCapsuleState(context.Background(), closedKey); err != nil || ok {
		t.Fatalf("purged state exists = %t, err=%v", ok, err)
	}
	closed, err = store.Get(closedKey.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	if closed.Metadata[CapsuleStatePurgeAuthorizedMetadataKey] != closedKey.Digest ||
		closed.Metadata[CapsuleStatePurgeCompletedMetadataKey] != closedKey.Digest {
		t.Fatalf("purge audit metadata = %#v, want digest authorization and completion", closed.Metadata)
	}
}

func TestCapsuleStateControlRejectsNonterminalOrLivePurge(t *testing.T) {
	t.Parallel()

	store := beads.NewMemStore()
	provider := runtime.NewFake()
	openKey := createCapsuleStateControlSession(t, store, provider, "open", false, false)
	liveKey := createCapsuleStateControlSession(t, store, provider, "closed-live", true, true)
	control := NewCapsuleStateControl(beads.SessionStore{Store: store}, provider)

	if _, err := control.Purge(context.Background(), "capsule-city", openKey.SessionID, false); !errors.Is(err, ErrCapsuleStatePurgeNotTerminal) {
		t.Fatalf("open purge error = %v, want ErrCapsuleStatePurgeNotTerminal", err)
	}
	if _, err := control.Purge(context.Background(), "capsule-city", liveKey.SessionID, false); !errors.Is(err, ErrCapsuleStatePurgeLive) {
		t.Fatalf("live purge error = %v, want ErrCapsuleStatePurgeLive", err)
	}

	for _, key := range []runtime.CapsuleKey{openKey, liveKey} {
		if _, ok, err := provider.OpenCapsuleState(context.Background(), key); err != nil || !ok {
			t.Fatalf("rejected purge removed %q: exists=%t err=%v", key.SessionID, ok, err)
		}
		b, err := store.Get(key.SessionID)
		if err != nil {
			t.Fatal(err)
		}
		if b.Metadata[CapsuleStatePurgeAuthorizedMetadataKey] != "" {
			t.Fatalf("rejected purge authorized %q: %#v", key.SessionID, b.Metadata)
		}
	}
}

func TestCapsuleStateControlPersistsAuthorizationAcrossProviderFailure(t *testing.T) {
	t.Parallel()

	store := beads.NewMemStore()
	provider := runtime.NewFake()
	key := createCapsuleStateControlSession(t, store, provider, "retry", true, false)
	provider.CapsuleStateErrors[key.Digest] = errors.New("provider endpoint /private/path unavailable")
	control := NewCapsuleStateControl(beads.SessionStore{Store: store}, provider)

	if _, err := control.Purge(context.Background(), "capsule-city", key.SessionID, false); err == nil {
		t.Fatal("Purge unexpectedly succeeded while provider failed")
	}
	b, err := store.Get(key.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	if b.Metadata[CapsuleStatePurgeAuthorizedMetadataKey] != key.Digest || b.Metadata[CapsuleStatePurgeCompletedMetadataKey] != "" {
		t.Fatalf("failed purge metadata = %#v, want durable authorization only", b.Metadata)
	}
	delete(provider.CapsuleStateErrors, key.Digest)
	report, err := control.Purge(context.Background(), "capsule-city", key.SessionID, false)
	if err != nil {
		t.Fatalf("retry Purge: %v", err)
	}
	if got := capsuleStateActions(report)[key.SessionID]; got != CapsuleStatePurged {
		t.Fatalf("retry action = %q, want %q", got, CapsuleStatePurged)
	}
}

func createCapsuleStateControlSession(t *testing.T, store *beads.MemStore, provider *runtime.Fake, id string, closed, live bool) runtime.CapsuleKey {
	t.Helper()

	metadata := map[string]string{
		"state":        string(StateArchived),
		"session_name": id + "-place",
	}
	created, err := store.Create(beads.Bead{
		ID: id, Title: id, Type: BeadType, Labels: []string{LabelSession}, Metadata: metadata,
	})
	if err != nil {
		t.Fatal(err)
	}
	key, err := runtime.NewCapsuleKey("capsule-city", created.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.SetMetadataBatch(created.ID, CapsuleStateMetadata(key)); err != nil {
		t.Fatal(err)
	}
	if closed {
		if err := store.Close(created.ID); err != nil {
			t.Fatal(err)
		}
	}
	if _, _, err := provider.EnsureCapsuleState(context.Background(), key); err != nil {
		t.Fatal(err)
	}
	if live {
		if err := provider.Start(context.Background(), id+"-place", runtime.Config{}); err != nil {
			t.Fatal(err)
		}
	}
	return key
}
