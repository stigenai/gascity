package main

import (
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/gastownhall/gascity/internal/api"
	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/orders"
	"github.com/gastownhall/gascity/internal/rollout"
)

// routedDemandStrandedEventType is the wire event type emitted when a
// gc.routed_to demand target is detected stranded (FR-4).
const routedDemandStrandedEventType = events.RoutedDemandStranded

// poolDemandWispMetadataKey marks a molecule/wisp bead as carrying
// order-dispatch pool demand (FR-5), the one narrow additive exception to
// readyExcludeTypes for stranded-demand detection purposes.
const poolDemandWispMetadataKey = "gc.pool_demand_wisp"

func poolDemandMetadataPair() map[string]string {
	return map[string]string{poolDemandWispMetadataKey: boolMetadata(true)}
}

// detectStrandedRoutedDemand scans for gc.routed_to demand beads whose
// resolved template no session can ever wake for (FR-4/FR-5/FR-6): a
// dead/misspelled route, or an agent whose effective min_active_sessions is 0
// with zero sessions currently open. Candidates are ordinary ready work beads
// (sling-style, direct-metadata, graph.v2 drain-unit members) plus the one
// additive exception readyExcludeTypes normally hides: order-dispatch
// pool-demand molecule/wisp beads carrying poolDemandWispMetadataKey.
// Candidates younger than cfg.Demand.StrandedRouteDebounceOrDefault() are
// skipped so a route that wakes a session moments after dispatch never fires
// a false positive.
//
// Emission is gated by the demand.stranded_route_policy rollout gate: Off is
// a byte-identical no-op, Auto emits one warning-severity event per stranded
// template and never blocks, Require additionally marks each candidate's
// owning order run failed (when it carries one) and mirrors
// events.OrderFailed. No mode ever mutates a candidate bead's status,
// assignee, or hold labels.
func detectStrandedRoutedDemand(store beads.Store, cfg *config.City, sessionBeads *sessionBeadSnapshot, rec events.Recorder, stderr io.Writer, now time.Time) error {
	if store == nil || cfg == nil || rec == nil {
		return nil
	}
	flags, err := rollout.Resolve(cfg, rollout.ResolveOptions{})
	if err != nil {
		return fmt.Errorf("resolving demand.stranded_route_policy gate: %w", err)
	}
	mode := flags.StrandedRoutedDemand()
	if mode == rollout.Off {
		return nil
	}

	candidates, err := strandedRoutedDemandCandidates(store)
	if err != nil {
		return fmt.Errorf("scanning routed demand candidates: %w", err)
	}

	debounce := cfg.Demand.StrandedRouteDebounceOrDefault()
	byTemplate := map[string][]beads.Bead{}
	var templateOrder []string
	for _, b := range candidates {
		template := strings.TrimSpace(b.Metadata[beadmeta.RoutedToMetadataKey])
		if template == "" || now.Sub(b.CreatedAt) < debounce {
			continue
		}
		if _, seen := byTemplate[template]; !seen {
			templateOrder = append(templateOrder, template)
		}
		byTemplate[template] = append(byTemplate[template], b)
	}

	severity := "warning"
	if mode == rollout.Require {
		severity = "failure"
	}

	for _, template := range templateOrder {
		if !routedTemplateIsUnwakeable(cfg, sessionBeads, template) {
			continue
		}
		group := byTemplate[template]
		beadIDs := make([]string, 0, len(group))
		for _, b := range group {
			beadIDs = append(beadIDs, b.ID)
		}
		rec.Record(events.Event{
			Type:    routedDemandStrandedEventType,
			Ts:      now.UTC(),
			Actor:   "controller",
			Subject: template,
			Message: formatRoutedDemandStrandedMessage(severity, template, beadIDs),
			Payload: api.RoutedDemandStrandedPayloadJSON(severity, template, beadIDs),
		})
		if mode == rollout.Require {
			failStrandedOrderRuns(store, rec, stderr, template, group)
		}
	}
	return nil
}

// strandedRoutedDemandCandidates returns every open, unblocked bead carrying
// gc.routed_to metadata: the normal ready-work set (Ready already excludes
// blocked beads and molecule/wisp types) plus molecule/wisp beads explicitly
// marked as order-dispatch pool demand via poolDemandWispMetadataKey — the
// one narrow additive exception to Ready's type exclusion.
func strandedRoutedDemandCandidates(store beads.Store) ([]beads.Bead, error) {
	ready, err := store.Ready()
	if err != nil {
		return nil, err
	}
	var candidates []beads.Bead
	for _, b := range ready {
		if strings.TrimSpace(b.Metadata[beadmeta.RoutedToMetadataKey]) != "" {
			candidates = append(candidates, b)
		}
	}

	wisps, err := store.List(beads.ListQuery{
		Status:    "open",
		Metadata:  poolDemandMetadataPair(),
		TierMode:  beads.TierBoth,
		AllowScan: true,
	})
	if err != nil {
		return nil, err
	}
	for _, b := range wisps {
		if !beads.IsMoleculeType(b.Type) {
			continue
		}
		if strings.TrimSpace(b.Metadata[beadmeta.RoutedToMetadataKey]) != "" {
			candidates = append(candidates, b)
		}
	}
	return candidates, nil
}

// routedTemplateIsUnwakeable reports whether template names an agent no
// session can currently wake for: the template resolves to no configured
// agent (dead/misspelled route), or its effective min_active_sessions is 0
// and no session is presently open for it.
func routedTemplateIsUnwakeable(cfg *config.City, sessionBeads *sessionBeadSnapshot, template string) bool {
	if sessionBeads != nil {
		if _, ok := sessionBeads.FindInfoByTemplate(template); ok {
			return false
		}
	}
	agent, ok := findAgentByName(cfg, template)
	if !ok {
		return true
	}
	return agent.EffectiveMinActiveSessions() == 0
}

// formatRoutedDemandStrandedMessage renders the operator-facing text for a
// routed_demand.stranded event.
func formatRoutedDemandStrandedMessage(severity, template string, beadIDs []string) string {
	verb := "will not be picked up automatically"
	if severity == "failure" {
		verb = "failing the owning order run(s) as well"
	}
	return fmt.Sprintf("routed demand for template %q is stranded (%d bead(s): %s); no session can currently wake for this route, %s",
		template, len(beadIDs), strings.Join(beadIDs, ", "), verb)
}

// failStrandedOrderRuns mirrors a Require-mode stranded detection onto the
// order-dispatch run ledger. Each candidate that carries an order-run label
// gets a matching events.OrderFailed mirror and has its run marked
// orders.RunOutcomeWispFailed — labels only, never the bead's status or
// assignee — following the same event-first, store-write-best-effort pairing
// order_dispatch.go's markTrackingFailure call sites use.
func failStrandedOrderRuns(store beads.Store, rec events.Recorder, stderr io.Writer, template string, group []beads.Bead) {
	front := orders.NewStore(beads.OrdersStore{Store: store})
	for _, b := range group {
		run, ok := orders.RunFromTrackingBead(b)
		if !ok {
			continue
		}
		rec.Record(events.Event{
			Type:    events.OrderFailed,
			Actor:   "controller",
			Subject: run.Scoped,
			Message: fmt.Sprintf("order %s failed: routed demand for template %q is stranded; no session can currently wake for this route", run.Scoped, template),
		})
		if err := front.MarkFailed(run.ID, run.Scoped, orders.RunOutcomeWispFailed, nil); err != nil {
			logDispatchError(stderr, "gc: stranded routed demand: failed to mark order run %s failed: %v", run.Scoped, err)
		}
	}
}
