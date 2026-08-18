package session

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/runtime"
)

const (
	// CapsuleStateCityScopeMetadataKey records the provider-owned scope used
	// to derive a session's durable capsule allocation identity.
	CapsuleStateCityScopeMetadataKey = "capsule_state_city_scope"
	// CapsuleStateDigestMetadataKey records the canonical allocation digest.
	// It is an ownership proof, not a secret or provider resource path.
	CapsuleStateDigestMetadataKey = "capsule_state_digest"
	// CapsuleStatePurgeAuthorizedMetadataKey records explicit operator intent.
	// Its value is the allocation digest the operator authorized.
	CapsuleStatePurgeAuthorizedMetadataKey = "capsule_state_purge_authorized"
	// CapsuleStatePurgeCompletedMetadataKey records successful provider purge.
	// Its value is the allocation digest that was deleted or confirmed absent.
	CapsuleStatePurgeCompletedMetadataKey = "capsule_state_purge_completed"
)

var (
	// ErrCapsuleStateControlUnsupported reports that the selected runtime does
	// not expose provider-owned capsule-state inventory and purge operations.
	ErrCapsuleStateControlUnsupported = errors.New("capsule state control is unsupported by the selected runtime")
	// ErrCapsuleStateNotTracked reports that a session never launched with a
	// durable capsule identity and therefore cannot authorize capsule purge.
	ErrCapsuleStateNotTracked = errors.New("session has no tracked capsule state")
	// ErrCapsuleStatePurgeNotTerminal reports that purge was requested before
	// the session bead was irreversibly closed.
	ErrCapsuleStatePurgeNotTerminal = errors.New("capsule state purge requires a closed session")
	// ErrCapsuleStatePurgeLive reports that provider ground truth still shows a
	// live runtime for the closed session.
	ErrCapsuleStatePurgeLive = errors.New("capsule state purge is blocked by a live runtime")
)

// CapsuleStateMetadata returns the non-secret durable ownership fields for a
// canonical capsule key. Callers must validate the key before persisting it.
func CapsuleStateMetadata(key runtime.CapsuleKey) map[string]string {
	return map[string]string{
		CapsuleStateCityScopeMetadataKey: key.CityScope,
		CapsuleStateDigestMetadataKey:    key.Digest,
	}
}

// CapsuleStateControl is the session-domain operator surface for inspecting
// provider inventory and explicitly purging one closed session allocation.
// Session beads remain the durable authorization and audit substrate.
type CapsuleStateControl struct {
	store    beads.SessionStore
	provider runtime.Provider
	state    runtime.CapsuleStateRuntime
}

// NewCapsuleStateControl binds session persistence to the selected runtime.
// Unsupported runtimes are represented by a usable control whose operations
// return ErrCapsuleStateControlUnsupported.
func NewCapsuleStateControl(store beads.SessionStore, provider runtime.Provider) *CapsuleStateControl {
	state, _ := provider.(runtime.CapsuleStateRuntime)
	return &CapsuleStateControl{store: store, provider: provider, state: state}
}

// Inspect returns a non-mutating reconciliation report for one city scope.
func (c *CapsuleStateControl) Inspect(ctx context.Context, cityScope string) (CapsuleStateReconcileReport, error) {
	reconciler, err := c.reconciler("")
	if err != nil {
		return CapsuleStateReconcileReport{}, err
	}
	return reconciler.Reconcile(ctx, cityScope, true)
}

// Purge previews or executes an explicit purge for one closed, non-live
// session. A preview performs all fresh safety reads but writes no metadata and
// deletes no provider state. Execution durably records authorization before
// invoking the idempotent reconciler, so an interrupted purge is retryable.
func (c *CapsuleStateControl) Purge(ctx context.Context, cityScope, sessionID string, dryRun bool) (CapsuleStateReconcileReport, error) {
	if c == nil || c.store.Store == nil || c.provider == nil || c.state == nil {
		return CapsuleStateReconcileReport{}, ErrCapsuleStateControlUnsupported
	}
	cityScope = strings.TrimSpace(cityScope)
	sessionID = strings.TrimSpace(sessionID)
	if cityScope == "" || sessionID == "" {
		return CapsuleStateReconcileReport{}, errors.New("capsule state purge requires city scope and session id")
	}

	var report CapsuleStateReconcileReport
	err := WithSessionMutationLock(sessionID, func() error {
		fact, exists, err := c.lookupFact(ctx, runtime.CapsuleKey{SessionID: sessionID}, cityScope)
		if err != nil {
			return err
		}
		if !exists {
			return fmt.Errorf("%w: %s", ErrCapsuleStateNotTracked, sessionID)
		}
		if !fact.Terminal {
			return fmt.Errorf("%w: %s", ErrCapsuleStatePurgeNotTerminal, sessionID)
		}
		if fact.Live {
			return fmt.Errorf("%w: %s", ErrCapsuleStatePurgeLive, sessionID)
		}

		authorizedDigest := fact.Key.Digest
		if !dryRun {
			if err := c.store.SetMetadata(sessionID, CapsuleStatePurgeAuthorizedMetadataKey, authorizedDigest); err != nil {
				return fmt.Errorf("record capsule state purge authorization for %q: %w", sessionID, err)
			}
		}
		reconciler, err := c.reconciler(authorizedDigest)
		if err != nil {
			return err
		}
		report, err = reconciler.Reconcile(ctx, cityScope, dryRun)
		return err
	})
	return report, err
}

func (c *CapsuleStateControl) reconciler(authorizedDigest string) (CapsuleStateReconciler, error) {
	if c == nil || c.store.Store == nil || c.provider == nil || c.state == nil {
		return CapsuleStateReconciler{}, ErrCapsuleStateControlUnsupported
	}
	return CapsuleStateReconciler{
		Runtime: c.state,
		ListSessions: func(ctx context.Context, scope string) ([]CapsuleStateSessionFact, error) {
			facts, err := c.listFacts(ctx, scope)
			if err != nil {
				return nil, err
			}
			for i := range facts {
				if facts[i].Key.Digest == authorizedDigest {
					facts[i].PurgeAuthorized = true
				}
			}
			return facts, nil
		},
		Lookup: func(ctx context.Context, key runtime.CapsuleKey) (CapsuleStateSessionFact, bool, error) {
			fact, ok, err := c.lookupFact(ctx, key, key.CityScope)
			if ok && fact.Key.Digest == authorizedDigest {
				fact.PurgeAuthorized = true
			}
			return fact, ok, err
		},
		MarkPurged: func(_ context.Context, key runtime.CapsuleKey) error {
			return c.store.SetMetadata(key.SessionID, CapsuleStatePurgeCompletedMetadataKey, key.Digest)
		},
	}, nil
}

func (c *CapsuleStateControl) listFacts(_ context.Context, cityScope string) ([]CapsuleStateSessionFact, error) {
	rows, err := c.store.List(beads.ListQuery{Label: LabelSession, IncludeClosed: true})
	if err != nil {
		return nil, fmt.Errorf("list capsule state session records: %w", err)
	}
	facts := make([]CapsuleStateSessionFact, 0, len(rows))
	for _, row := range rows {
		fact, tracked, err := c.factFromBead(row, cityScope)
		if err != nil {
			return nil, err
		}
		if tracked {
			facts = append(facts, fact)
		}
	}
	return facts, nil
}

func (c *CapsuleStateControl) lookupFact(_ context.Context, key runtime.CapsuleKey, cityScope string) (CapsuleStateSessionFact, bool, error) {
	row, err := c.store.Get(strings.TrimSpace(key.SessionID))
	if err != nil {
		if errors.Is(err, beads.ErrNotFound) {
			return CapsuleStateSessionFact{}, false, nil
		}
		return CapsuleStateSessionFact{}, false, fmt.Errorf("load capsule state session %q: %w", key.SessionID, err)
	}
	fact, tracked, err := c.factFromBead(row, cityScope)
	if err != nil || !tracked {
		return fact, tracked, err
	}
	if key.Digest != "" && fact.Key != key {
		return CapsuleStateSessionFact{}, false, fmt.Errorf("%w: session %q capsule identity changed", runtime.ErrCapsuleStateConflict, key.SessionID)
	}
	return fact, true, nil
}

func (c *CapsuleStateControl) factFromBead(row beads.Bead, cityScope string) (CapsuleStateSessionFact, bool, error) {
	if !IsSessionBeadOrRepairable(row) {
		return CapsuleStateSessionFact{}, false, nil
	}
	scope := strings.TrimSpace(row.Metadata[CapsuleStateCityScopeMetadataKey])
	digest := strings.TrimSpace(row.Metadata[CapsuleStateDigestMetadataKey])
	if scope == "" && digest == "" {
		return CapsuleStateSessionFact{}, false, nil
	}
	if scope == "" || digest == "" {
		return CapsuleStateSessionFact{}, false, fmt.Errorf("%w: session %q has incomplete capsule identity", runtime.ErrCapsuleStateConflict, row.ID)
	}
	if strings.TrimSpace(cityScope) != "" && scope != strings.TrimSpace(cityScope) {
		return CapsuleStateSessionFact{}, false, nil
	}
	key, err := runtime.NewCapsuleKey(scope, row.ID)
	if err != nil || key.Digest != digest {
		return CapsuleStateSessionFact{}, false, fmt.Errorf("%w: session %q has invalid capsule identity", runtime.ErrCapsuleStateConflict, row.ID)
	}
	sessionName := strings.TrimSpace(row.Metadata["session_name"])
	if sessionName == "" {
		sessionName = sessionNameFor(row.ID)
	}
	return CapsuleStateSessionFact{
		Key:             key,
		Terminal:        row.Status == "closed",
		Live:            c.provider.IsRunning(sessionName),
		PurgeAuthorized: strings.TrimSpace(row.Metadata[CapsuleStatePurgeAuthorizedMetadataKey]) == key.Digest,
		PurgeCompleted:  strings.TrimSpace(row.Metadata[CapsuleStatePurgeCompletedMetadataKey]) == key.Digest,
	}, true, nil
}

func (m *Manager) persistCapsuleStateIdentity(id string, b *beads.Bead, capsule *runtime.CapsuleLaunchConfig) error {
	if capsule == nil {
		return nil
	}
	key := capsule.Key
	if err := key.Validate(); err != nil {
		return fmt.Errorf("validate capsule state identity: %w", err)
	}
	if key.SessionID != id {
		return fmt.Errorf("%w: capsule session %q does not match bead %q", runtime.ErrCapsuleStateConflict, key.SessionID, id)
	}
	if b == nil {
		return errors.New("persist capsule state identity requires session bead")
	}
	if b.Metadata == nil {
		b.Metadata = make(map[string]string)
	}
	want := CapsuleStateMetadata(key)
	existingScope := strings.TrimSpace(b.Metadata[CapsuleStateCityScopeMetadataKey])
	existingDigest := strings.TrimSpace(b.Metadata[CapsuleStateDigestMetadataKey])
	if existingScope != "" || existingDigest != "" {
		if existingScope != key.CityScope || existingDigest != key.Digest {
			return fmt.Errorf("%w: session %q capsule state identity changed", runtime.ErrCapsuleStateConflict, id)
		}
		return nil
	}
	if err := m.store.SetMetadataBatch(id, want); err != nil {
		return fmt.Errorf("persist capsule state identity for %q: %w", id, err)
	}
	for k, v := range want {
		b.Metadata[k] = v
	}
	return nil
}
