package session

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/gastownhall/gascity/internal/runtime"
)

// CapsuleStateSessionFact is the durable session-side authority used when
// reconciling provider-owned Omnigent state. PurgeAuthorized must be committed
// by an explicit operator operation; Terminal alone never authorizes deletion.
type CapsuleStateSessionFact struct {
	Key             runtime.CapsuleKey
	Terminal        bool
	Live            bool
	PurgeAuthorized bool
	PurgeCompleted  bool
}

// CapsuleStateReconcileAction is a factual reconciliation result, not retry or
// retention policy.
type CapsuleStateReconcileAction string

const (
	// CapsuleStateRetained means an authoritative session still requires continuity.
	CapsuleStateRetained CapsuleStateReconcileAction = "retained"
	// CapsuleStateRetainedOrphan means verified state has no authoritative session and was preserved.
	CapsuleStateRetainedOrphan CapsuleStateReconcileAction = "retained_orphan"
	// CapsuleStateWouldPurge means dry-run validated an authorized purge without mutating state.
	CapsuleStateWouldPurge CapsuleStateReconcileAction = "would_purge"
	// CapsuleStatePurged means the exact allocation was deleted and completion recorded.
	CapsuleStatePurged CapsuleStateReconcileAction = "purged"
	// CapsuleStatePurgeRecorded means an already-absent authorized allocation was recorded complete.
	CapsuleStatePurgeRecorded CapsuleStateReconcileAction = "purge_recorded"
	// CapsuleStateConflict means ownership or a fresh safety observation was ambiguous.
	CapsuleStateConflict CapsuleStateReconcileAction = "conflict"
	// CapsuleStateMissing means expected retained state was absent without purge authority.
	CapsuleStateMissing CapsuleStateReconcileAction = "missing"
)

// CapsuleStateReconcileItem is one non-secret diagnostic for a session in the
// requested city scope.
type CapsuleStateReconcileItem struct {
	SessionID string
	Action    CapsuleStateReconcileAction
	Reason    string
}

// CapsuleStateReconcileReport is a stable dry-run or mutation report. Foreign
// allocations are counted but their identities are not disclosed.
type CapsuleStateReconcileReport struct {
	Items          []CapsuleStateReconcileItem
	IgnoredForeign int
}

// CapsuleStateReconciler joins provider inventory to durable session facts.
// Function fields keep Beads ownership outside the runtime layer and provide a
// narrow seam for a fresh pre-mutation lookup and idempotent completion record.
type CapsuleStateReconciler struct {
	Runtime      runtime.CapsuleStateRuntime
	ListSessions func(context.Context, string) ([]CapsuleStateSessionFact, error)
	Lookup       func(context.Context, runtime.CapsuleKey) (CapsuleStateSessionFact, bool, error)
	MarkPurged   func(context.Context, runtime.CapsuleKey) error
}

// Reconcile inventories one city scope. It retains live, closed, archived, and
// orphan state; only a terminal, non-live, explicitly authorized purge mutates
// the provider. Dry-run performs all safety reads but no mutation.
func (r CapsuleStateReconciler) Reconcile(ctx context.Context, cityScope string, dryRun bool) (CapsuleStateReconcileReport, error) {
	if r.Runtime == nil || r.ListSessions == nil || r.Lookup == nil || r.MarkPurged == nil {
		return CapsuleStateReconcileReport{}, errors.New("capsule state reconciler dependencies are required")
	}
	cityScope = strings.TrimSpace(cityScope)
	if cityScope == "" {
		return CapsuleStateReconcileReport{}, errors.New("capsule state reconciler city scope is required")
	}
	sessions, err := r.ListSessions(ctx, cityScope)
	if err != nil {
		return CapsuleStateReconcileReport{}, fmt.Errorf("list capsule state sessions: %w", err)
	}
	providerRefs, err := r.Runtime.ListCapsuleStates(ctx)
	if err != nil {
		return CapsuleStateReconcileReport{}, fmt.Errorf("list capsule state provider inventory: %w", err)
	}

	report := CapsuleStateReconcileReport{}
	providerByDigest := make(map[string]runtime.CapsuleStateReference)
	ambiguous := make(map[string]bool)
	for _, ref := range providerRefs {
		if err := ref.Key.Validate(); err != nil || ref.Provider == "" || ref.ResourceID == "" {
			if ref.Key.CityScope == cityScope {
				report.Items = append(report.Items, CapsuleStateReconcileItem{SessionID: ref.Key.SessionID, Action: CapsuleStateConflict, Reason: "invalid_provider_identity"})
			}
			continue
		}
		if ref.Key.CityScope != cityScope {
			report.IgnoredForeign++
			continue
		}
		if _, exists := providerByDigest[ref.Key.Digest]; exists {
			ambiguous[ref.Key.Digest] = true
			continue
		}
		providerByDigest[ref.Key.Digest] = ref
	}

	sessionByDigest := make(map[string]CapsuleStateSessionFact, len(sessions))
	var reconcileErrs []error
	for _, fact := range sessions {
		if err := fact.Key.Validate(); err != nil || fact.Key.CityScope != cityScope {
			reconcileErrs = append(reconcileErrs, fmt.Errorf("session %q has invalid capsule state identity", fact.Key.SessionID))
			continue
		}
		if _, duplicate := sessionByDigest[fact.Key.Digest]; duplicate {
			report.Items = append(report.Items, CapsuleStateReconcileItem{SessionID: fact.Key.SessionID, Action: CapsuleStateConflict, Reason: "duplicate_session_identity"})
			continue
		}
		sessionByDigest[fact.Key.Digest] = fact
		ref, hasState := providerByDigest[fact.Key.Digest]
		if ambiguous[fact.Key.Digest] {
			report.Items = append(report.Items, CapsuleStateReconcileItem{SessionID: fact.Key.SessionID, Action: CapsuleStateConflict, Reason: "ambiguous_provider_state"})
			continue
		}
		if !hasState {
			if fact.PurgeAuthorized && fact.Terminal && !fact.Live {
				if dryRun {
					report.Items = append(report.Items, CapsuleStateReconcileItem{SessionID: fact.Key.SessionID, Action: CapsuleStateWouldPurge, Reason: "authorized_state_already_absent"})
					continue
				}
				if err := r.Runtime.PurgeCapsuleState(ctx, fact.Key); err != nil {
					reconcileErrs = append(reconcileErrs, fmt.Errorf("confirm absent capsule state for session %q: %w", fact.Key.SessionID, err))
					continue
				}
				if err := r.MarkPurged(ctx, fact.Key); err != nil {
					reconcileErrs = append(reconcileErrs, fmt.Errorf("record purged capsule state for session %q: %w", fact.Key.SessionID, err))
					continue
				}
				report.Items = append(report.Items, CapsuleStateReconcileItem{SessionID: fact.Key.SessionID, Action: CapsuleStatePurgeRecorded, Reason: "authorized_state_already_absent"})
				continue
			}
			report.Items = append(report.Items, CapsuleStateReconcileItem{SessionID: fact.Key.SessionID, Action: CapsuleStateMissing, Reason: "expected_state_absent"})
			continue
		}
		if !fact.PurgeAuthorized || !fact.Terminal || fact.Live {
			reason := "retention_required"
			if fact.PurgeAuthorized && fact.Live {
				reason = "live_session_blocks_purge"
			} else if fact.PurgeAuthorized && !fact.Terminal {
				reason = "nonterminal_session_blocks_purge"
			}
			report.Items = append(report.Items, CapsuleStateReconcileItem{SessionID: fact.Key.SessionID, Action: CapsuleStateRetained, Reason: reason})
			continue
		}
		item, itemErr := r.purgeOne(ctx, fact, ref, dryRun)
		report.Items = append(report.Items, item)
		if itemErr != nil {
			reconcileErrs = append(reconcileErrs, itemErr)
		}
	}
	for digest, ref := range providerByDigest {
		if _, exists := sessionByDigest[digest]; exists || ambiguous[digest] {
			continue
		}
		report.Items = append(report.Items, CapsuleStateReconcileItem{SessionID: ref.Key.SessionID, Action: CapsuleStateRetainedOrphan, Reason: "no_authoritative_session"})
	}
	sort.Slice(report.Items, func(i, j int) bool {
		if report.Items[i].SessionID == report.Items[j].SessionID {
			return report.Items[i].Action < report.Items[j].Action
		}
		return report.Items[i].SessionID < report.Items[j].SessionID
	})
	return report, errors.Join(reconcileErrs...)
}

func (r CapsuleStateReconciler) purgeOne(ctx context.Context, observed CapsuleStateSessionFact, listed runtime.CapsuleStateReference, dryRun bool) (CapsuleStateReconcileItem, error) {
	item := CapsuleStateReconcileItem{SessionID: observed.Key.SessionID, Action: CapsuleStateConflict, Reason: "pre_purge_check_failed"}
	opened, ok, err := r.Runtime.OpenCapsuleState(ctx, observed.Key)
	if err != nil {
		return item, fmt.Errorf("reopen capsule state for session %q: %w", observed.Key.SessionID, err)
	}
	if ok && opened != listed {
		item.Reason = "stale_provider_observation"
		return item, fmt.Errorf("%w: capsule state changed before purge for session %q", runtime.ErrCapsuleStateConflict, observed.Key.SessionID)
	}
	fresh, exists, err := r.Lookup(ctx, observed.Key)
	if err != nil {
		return item, fmt.Errorf("refresh capsule state session %q: %w", observed.Key.SessionID, err)
	}
	if !exists {
		item.Action = CapsuleStateRetainedOrphan
		item.Reason = "session_deleted_during_reconcile"
		return item, nil
	}
	if fresh.Key != observed.Key || !fresh.PurgeAuthorized || !fresh.Terminal || fresh.Live {
		item.Action = CapsuleStateRetained
		item.Reason = "purge_authority_changed"
		return item, nil
	}
	if dryRun {
		item.Action = CapsuleStateWouldPurge
		item.Reason = "explicit_terminal_purge"
		return item, nil
	}
	if err := r.Runtime.PurgeCapsuleState(ctx, observed.Key); err != nil {
		item.Reason = "provider_purge_failed"
		return item, fmt.Errorf("purge capsule state for session %q: %w", observed.Key.SessionID, err)
	}
	if err := r.MarkPurged(ctx, observed.Key); err != nil {
		item.Reason = "record_purge_failed"
		return item, fmt.Errorf("record purged capsule state for session %q: %w", observed.Key.SessionID, err)
	}
	item.Action = CapsuleStatePurged
	item.Reason = "explicit_terminal_purge"
	return item, nil
}
