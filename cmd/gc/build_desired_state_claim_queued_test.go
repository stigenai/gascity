package main

import (
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
