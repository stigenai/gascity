package main

import (
	"io"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/runtime"
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

// TestBuildDesiredStateSurfacesClaimQueuedSuppressionDiagnostic drives a
// claim_state=queued bead through buildDesiredStateWithSessionBeads, unlike
// the three tests above which all call defaultScaleCheckCountsAndDemand
// directly. That tally is correctly implemented and covered, but the stderr
// line and demand_snapshot.default_scale_demand trace field it feeds — the
// literal fix for gcy-kcy's "no log line ... nothing in gc status" complaint
// — are only ever emitted from buildDesiredStateWithSessionBeads
// (cmd/gc/build_desired_state.go), so a regression there would leave the
// tests above green while silently reproducing gcy-kcy.
func TestBuildDesiredStateSurfacesClaimQueuedSuppressionDiagnostic(t *testing.T) {
	const template = "coder"
	cityDir := t.TempDir()
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

	// A single agent with no custom scale_check is enough to populate
	// defaultScaleTargets (see defaultScaleCheckTargetForAgent) and drive the
	// diagnostic in buildDesiredStateWithSessionBeads.
	cfg := &config.City{
		Workspace: config.Workspace{Name: "test-city"},
		Agents: []config.Agent{{
			Name:         template,
			StartCommand: "true",
		}},
	}

	tracer := newSessionReconcilerTracer(cityDir, "trace-town", io.Discard)
	if !tracer.Enabled() {
		t.Fatal("tracer should be enabled")
	}
	cycle := tracer.BeginCycle(TraceTickTriggerPatrol, "", time.Now().UTC(), cfg)
	if cycle == nil {
		t.Fatal("BeginCycle returned nil")
	}

	sessionSnapshot, err := loadSessionBeadSnapshot(store)
	if err != nil {
		t.Fatalf("load session snapshot: %v", err)
	}
	var stderr strings.Builder
	buildDesiredStateWithSessionBeads(
		"trace-town", cityDir, time.Now().UTC(), cfg, runtime.NewFake(),
		store, nil, sessionSnapshot, cycle, &stderr,
	)

	if !strings.Contains(stderr.String(), "scaleCheck: 1 ready bead(s) suppressed from pool demand") {
		t.Fatalf("stderr = %q, want claim-queued suppression diagnostic", stderr.String())
	}
	if !strings.Contains(stderr.String(), queued.ID) {
		t.Fatalf("stderr = %q, want suppressed bead ID %s", stderr.String(), queued.ID)
	}

	if err := cycle.End(TraceCompletionCompleted, map[string]any{}); err != nil {
		t.Fatalf("End: %v", err)
	}
	if err := tracer.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	records, err := ReadTraceRecords(traceCityRuntimeDir(cityDir), TraceFilter{})
	if err != nil {
		t.Fatalf("ReadTraceRecords: %v", err)
	}
	var found bool
	for i := range records {
		r := &records[i]
		if r.RecordType != TraceRecordOperation || r.SiteCode != TraceSiteDemandSnapshot {
			continue
		}
		if name, _ := r.Fields["operation_name"].(string); name != "demand_snapshot.default_scale_demand" {
			continue
		}
		found = true
		if got := traceFieldInt(r.Fields["claim_queued_suppressed"]); got != 1 {
			t.Fatalf("claim_queued_suppressed = %#v, want 1", r.Fields["claim_queued_suppressed"])
		}
	}
	if !found {
		t.Fatal("missing demand_snapshot.default_scale_demand operation record")
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

// --- gcy-gbgd: the claim-queued stderr diagnostic above must stay bounded.
// Review of PR #53 found it unbounded in two dimensions: an unbounded ID
// list (formatClaimQueuedSuppressedIDs' job below) and an unbounded re-print
// rate (claimQueuedSuppressionLastEmitted's job) — in the wedged
// route-claim-watch scenario the diagnostic exists to surface (~40 beads,
// ~10h on 2026-08-05) the original version would have written ~36,000
// identical lines carrying ~40 IDs each.

// TestFormatClaimQueuedSuppressedIDs covers the ID-list cap, mirroring
// formatStrandedMessage's strandedWorkIDListLimit handling
// (session_reconciler.go): under the limit prints every ID, over it
// truncates with a "+N more" suffix rather than growing unbounded.
func TestFormatClaimQueuedSuppressedIDs(t *testing.T) {
	tests := []struct {
		name string
		ids  []string
		want string
	}{
		{name: "empty", ids: nil, want: ""},
		{name: "under limit", ids: []string{"a", "b", "c"}, want: "a,b,c"},
		{
			name: "exactly at limit prints all, no suffix",
			ids:  []string{"a", "b", "c", "d", "e", "f", "g", "h", "i", "j"},
			want: "a,b,c,d,e,f,g,h,i,j",
		},
		{
			name: "over limit truncates with a +N more suffix",
			ids:  []string{"a", "b", "c", "d", "e", "f", "g", "h", "i", "j", "k", "l"},
			want: "a,b,c,d,e,f,g,h,i,j (+2 more)",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := formatClaimQueuedSuppressedIDs(tc.ids); got != tc.want {
				t.Fatalf("formatClaimQueuedSuppressedIDs(%v) = %q, want %q", tc.ids, got, tc.want)
			}
		})
	}
}

// TestBuildDesiredStateClaimQueuedSuppressionStderrDedupsAcrossUnchangedTicks
// locks in the re-print fix: PR #53's original diagnostic re-emitted the
// full suppressed-bead-ID list on every demand snapshot — up to once/second
// via scaleCheckDemandMinInterval, with nothing to distinguish an unchanged
// tick from a new one. The fix must still print the FIRST time a suppressed
// set is observed, go SILENT on a following tick with that same set
// unchanged, and — critically — re-announce if the exact same bead becomes
// suppressed again after an intervening healthy tick. That last case is the
// one a naive "remember every ID ever printed" cache would get wrong: it
// would permanently silence a bead that clears and later re-wedges.
func TestBuildDesiredStateClaimQueuedSuppressionStderrDedupsAcrossUnchangedTicks(t *testing.T) {
	const template = "coder"
	cityDir := t.TempDir()
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

	cfg := &config.City{
		Workspace: config.Workspace{Name: "test-city"},
		Agents: []config.Agent{{
			Name:         template,
			StartCommand: "true",
		}},
	}

	tick := func() string {
		sessionSnapshot, err := loadSessionBeadSnapshot(store)
		if err != nil {
			t.Fatalf("load session snapshot: %v", err)
		}
		var stderr strings.Builder
		buildDesiredStateWithSessionBeads(
			"trace-town", cityDir, time.Now().UTC(), cfg, runtime.NewFake(),
			store, nil, sessionSnapshot, nil, &stderr,
		)
		return stderr.String()
	}

	if out := tick(); !strings.Contains(out, "scaleCheck:") || !strings.Contains(out, queued.ID) {
		t.Fatalf("tick 1 stderr = %q, want the suppression diagnostic naming %s", out, queued.ID)
	}

	if out := tick(); strings.Contains(out, "scaleCheck:") {
		t.Fatalf("tick 2 stderr = %q, want silence (unchanged suppressed set)", out)
	}

	// Un-suppress the bead (a healthy tick) — must clear the dedup cache,
	// not just stay silent because Count happens to be 0.
	if err := store.SetMetadataBatch(queued.ID, map[string]string{
		beadmeta.ClaimStateMetadataKey: "claimed",
	}); err != nil {
		t.Fatalf("SetMetadataBatch (un-suppress): %v", err)
	}
	if out := tick(); strings.Contains(out, "scaleCheck:") {
		t.Fatalf("tick 3 stderr = %q, want silence (nothing suppressed)", out)
	}

	// The exact same bead ID becomes suppressed again. If the healthy tick
	// above had not cleared the dedup cache, this would wrongly stay silent
	// forever since the fingerprint matches tick 1's.
	if err := store.SetMetadataBatch(queued.ID, map[string]string{
		beadmeta.ClaimStateMetadataKey:       beadmeta.ClaimStateQueued,
		beadmeta.ClaimQueueReasonMetadataKey: "behind_head",
	}); err != nil {
		t.Fatalf("SetMetadataBatch (re-suppress): %v", err)
	}
	if out := tick(); !strings.Contains(out, "scaleCheck:") || !strings.Contains(out, queued.ID) {
		t.Fatalf("tick 4 stderr = %q, want the suppression diagnostic to re-announce %s", out, queued.ID)
	}
}
