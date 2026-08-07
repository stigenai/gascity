package main

import (
	"reflect"
	"sort"
	"testing"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
)

// TestClaimQueuedBehindHead covers the demand suppressor added after the
// 2026-08-01 spawn storms, where 1,447 creates aborted across five beads that
// route-claim-watch had already marked undeliverable. gc had no reader for
// claim_state at all, so it kept generating pool demand for beads that were
// simply waiting their turn behind another bead at their target's queue head.
//
// The asymmetry between "queued" and the other failing states is the whole
// point of the function and is asserted below: "queued" clears itself when the
// head is worked, so suppressing demand is safe. "overdue"/"escalated" do not
// clear themselves, so suppressing on those would strand the bead forever —
// nothing would spawn, so nothing could claim, so the state could never change.
// stringSet returns the given slice as a sorted slice of its unique values,
// for order-independent comparison of ID collections whose order follows store
// iteration and is not part of the contract.
func stringSet(in []string) []string {
	seen := make(map[string]struct{}, len(in))
	out := make([]string, 0, len(in))
	for _, s := range in {
		if _, dup := seen[s]; dup {
			continue
		}
		seen[s] = struct{}{}
		out = append(out, s)
	}
	sort.Strings(out)
	return out
}

// TestDefaultScaleCheckCountsAndDemandReportsClaimQueuedSuppression locks in
// the observability added after the 2026-08-05 incident, where ~40 ready beads
// sat unrouted for ~10h with route-claim-watch wedged and nothing in gc
// distinguished "no work" from "demand suppressed by a dead watcher". The
// claim_state=queued beads must still NOT count as pool demand (that is what
// PR #50 fixed), but the suppression must now be reported so a stuck queue is
// visible instead of silent.
func TestDefaultScaleCheckCountsAndDemandReportsClaimQueuedSuppression(t *testing.T) {
	const template = "rig-basic/coder"
	store := beads.NewMemStore()

	// Ordinary ready demand: must still be counted normally.
	live, err := store.Create(beads.Bead{
		Title:  "live demand",
		Type:   "task",
		Status: "open",
		Metadata: map[string]string{
			beadmeta.RoutedToMetadataKey: template,
		},
	})
	if err != nil {
		t.Fatalf("create live bead: %v", err)
	}

	// Two beads queued behind their target's head by route-claim-watch. They
	// are unclaimable by construction, so they must not spawn demand — but they
	// must be surfaced as suppressed so a stalled watcher is observable.
	queuedA, err := store.Create(beads.Bead{
		Title:  "queued behind head A",
		Type:   "task",
		Status: "open",
		Metadata: map[string]string{
			beadmeta.RoutedToMetadataKey:         template,
			beadmeta.ClaimStateMetadataKey:       beadmeta.ClaimStateQueued,
			beadmeta.ClaimQueueReasonMetadataKey: "behind_head",
		},
	})
	if err != nil {
		t.Fatalf("create queued bead A: %v", err)
	}
	queuedB, err := store.Create(beads.Bead{
		Title:  "queued target busy B",
		Type:   "task",
		Status: "open",
		Metadata: map[string]string{
			beadmeta.RoutedToMetadataKey:         template,
			beadmeta.ClaimStateMetadataKey:       beadmeta.ClaimStateQueued,
			beadmeta.ClaimQueueReasonMetadataKey: "target_busy",
		},
	})
	if err != nil {
		t.Fatalf("create queued bead B: %v", err)
	}

	counts, _, _, suppression, errs := defaultScaleCheckCountsAndDemand(nil, []defaultScaleCheckTarget{{
		template: template,
		storeKey: "rig:rig-basic",
		store:    store,
	}})
	if len(errs) != 0 {
		t.Fatalf("defaultScaleCheckCountsAndDemand errs = %v", errs)
	}
	if got := counts[template]; got != 1 {
		t.Fatalf("counts[%q] = %d, want 1 (only the live bead counts as demand; queued beads stay suppressed)", template, got)
	}
	if suppression.Count != 2 {
		t.Fatalf("suppression.Count = %d, want 2 (both queued beads must be reported as suppressed)", suppression.Count)
	}
	if got := stringSet(suppression.BeadIDs); !reflect.DeepEqual(got, stringSet([]string{queuedA.ID, queuedB.ID})) {
		t.Fatalf("suppression.BeadIDs = %v, want [%s %s]", suppression.BeadIDs, queuedA.ID, queuedB.ID)
	}
	if live.ID == queuedA.ID || live.ID == queuedB.ID {
		t.Fatalf("test setup collision: live bead ID matches a queued bead ID")
	}
}

// TestDefaultScaleCheckCountsAndDemandSilentWhenNothingQueued asserts the
// suppression diagnostic stays quiet on a healthy tick: with no queued beads
// there is nothing to report, so Count is zero and no bead IDs are collected.
// This guards against the diagnostic becoming noisy every tick.
func TestDefaultScaleCheckCountsAndDemandSilentWhenNothingQueued(t *testing.T) {
	const template = "rig-basic/coder"
	store := beads.NewMemStore()
	if _, err := store.Create(beads.Bead{
		Title:  "ordinary demand",
		Type:   "task",
		Status: "open",
		Metadata: map[string]string{
			beadmeta.RoutedToMetadataKey: template,
		},
	}); err != nil {
		t.Fatalf("create bead: %v", err)
	}

	_, _, _, suppression, errs := defaultScaleCheckCountsAndDemand(nil, []defaultScaleCheckTarget{{
		template: template,
		storeKey: "rig:rig-basic",
		store:    store,
	}})
	if len(errs) != 0 {
		t.Fatalf("defaultScaleCheckCountsAndDemand errs = %v", errs)
	}
	if suppression.Count != 0 {
		t.Fatalf("suppression.Count = %d, want 0 on a healthy tick (no queued beads)", suppression.Count)
	}
	if len(suppression.BeadIDs) != 0 {
		t.Fatalf("suppression.BeadIDs = %v, want empty on a healthy tick", suppression.BeadIDs)
	}
}

// TestDefaultScaleCheckCountsAndDemandDedupsSuppressionAcrossAliasedStores
// mirrors the countedBeads dedup: when a rig store aliases the city store as a
// distinct Store object, the same ready bead is iterated in two store groups.
// A queued bead must be reported as suppressed exactly once per tick, not once
// per group, so the count an operator reads is the number of physical beads.
func TestDefaultScaleCheckCountsAndDemandDedupsSuppressionAcrossAliasedStores(t *testing.T) {
	const template = "rig-basic/coder"
	store := beads.NewMemStore()
	queued, err := store.Create(beads.Bead{
		Title:  "queued behind head",
		Type:   "task",
		Status: "open",
		Metadata: map[string]string{
			beadmeta.RoutedToMetadataKey:         template,
			beadmeta.ClaimStateMetadataKey:       beadmeta.ClaimStateQueued,
			beadmeta.ClaimQueueReasonMetadataKey: "behind_head",
		},
	})
	if err != nil {
		t.Fatalf("create queued bead: %v", err)
	}

	// Same store surfaced under two storeKeys, as a rig-aliasing city layout
	// would produce. The queued bead is iterated once per group.
	_, _, _, suppression, errs := defaultScaleCheckCountsAndDemand(nil, []defaultScaleCheckTarget{
		{template: template, storeKey: "city", store: store},
		{template: template, storeKey: "rig:rig-basic", store: store},
	})
	if len(errs) != 0 {
		t.Fatalf("defaultScaleCheckCountsAndDemand errs = %v", errs)
	}
	if suppression.Count != 1 {
		t.Fatalf("suppression.Count = %d, want 1 (a bead aliased across two store groups must be suppressed once)", suppression.Count)
	}
	if got := stringSet(suppression.BeadIDs); !reflect.DeepEqual(got, []string{queued.ID}) {
		t.Fatalf("suppression.BeadIDs = %v, want [%s]", suppression.BeadIDs, queued.ID)
	}
}

func TestClaimQueuedBehindHead(t *testing.T) {
	for _, tc := range []struct {
		name string
		meta map[string]string
		want bool
	}{
		{
			// ib-izxdw's actual shape on 2026-08-01: queued behind head from
			// 06:38:45, retried 167 times until it was acked at 10:49:05.
			name: "queued behind head is suppressed",
			meta: map[string]string{
				beadmeta.ClaimStateMetadataKey:       "queued",
				beadmeta.ClaimQueueReasonMetadataKey: "behind_head",
				beadmeta.RoutedToMetadataKey:         "infra-blocks/ib-ops.review-pre",
			},
			want: true,
		},
		{
			name: "queued because the target is busy is also suppressed",
			meta: map[string]string{
				beadmeta.ClaimStateMetadataKey:       "queued",
				beadmeta.ClaimQueueReasonMetadataKey: "target_busy",
			},
			want: true,
		},
		{
			// st-pvpfs: escalated at 16:16:32, then retried for six more hours.
			// That is a real bug, but suppressing here would deadlock the bead.
			// It needs an attempt budget that parks it visibly instead.
			name: "escalated is NOT suppressed, it would strand the bead",
			meta: map[string]string{beadmeta.ClaimStateMetadataKey: "escalated"},
			want: false,
		},
		{
			name: "overdue is NOT suppressed, same stranding risk",
			meta: map[string]string{beadmeta.ClaimStateMetadataKey: "overdue"},
			want: false,
		},
		{
			name: "an already-claimed bead is not suppressed by this rule",
			meta: map[string]string{beadmeta.ClaimStateMetadataKey: "claimed"},
			want: false,
		},
		{
			// The overwhelmingly common case: route-claim-watch has never
			// touched this bead. It must remain ordinary demand.
			name: "no claim metadata at all is normal demand",
			meta: map[string]string{beadmeta.RoutedToMetadataKey: "infra-blocks/ib-ops.e2e"},
			want: false,
		},
		{
			name: "nil metadata is normal demand",
			meta: nil,
			want: false,
		},
		{
			// route-claim-watch writes through bd/JSON round-trips; a padded
			// value must not silently disable the suppressor.
			name: "surrounding whitespace still matches",
			meta: map[string]string{beadmeta.ClaimStateMetadataKey: "  queued\n"},
			want: true,
		},
		{
			name: "a queue reason without a queued state does not suppress",
			meta: map[string]string{beadmeta.ClaimQueueReasonMetadataKey: "behind_head"},
			want: false,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := claimQueuedBehindHead(beads.Bead{ID: "b-1", Metadata: tc.meta})
			if got != tc.want {
				t.Fatalf("claimQueuedBehindHead(%v) = %v, want %v", tc.meta, got, tc.want)
			}
		})
	}
}
