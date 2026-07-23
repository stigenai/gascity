package main

import (
	"context"
	"encoding/json"
	"io"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/runtime"
)

const routedDemandStrandedEventType = "routed_demand.stranded"

func TestBuildDesiredStateDeadAssigneeCountsAsPoolDemand(t *testing.T) {
	store := beads.NewMemStore()
	cfg := deadAssigneeDemandConfig(1, 0)
	template := cfg.Agents[0].QualifiedName()
	closed := deadAssigneeSessionBead("session-dead", "worker-dead", template, "closed")
	if _, err := store.Create(closed); err != nil {
		t.Fatalf("create closed session bead: %v", err)
	}
	if _, err := store.Create(beads.Bead{
		ID:       "ga-dead-assignee",
		Title:    "ready work held by a closed worker",
		Type:     "task",
		Status:   "open",
		Assignee: "worker-dead",
		Metadata: map[string]string{"gc.routed_to": template},
	}); err != nil {
		t.Fatalf("create assigned routed work: %v", err)
	}

	result := buildDesiredStateWithSessionBeads(
		"test-city", t.TempDir(), time.Now().UTC(), cfg, runtime.NewFake(),
		store, nil, newSessionBeadSnapshot(nil), nil, io.Discard,
	)

	if got := result.ScaleCheckCounts[template]; got != 1 {
		t.Fatalf("ScaleCheckCounts[%q] = %d, want 1 for ready work assigned to a confirmed-dead session", template, got)
	}
	if len(result.State) != 1 {
		t.Fatalf("desired session count = %d, want 1 replacement for confirmed-dead assignee; state=%v", len(result.State), result.State)
	}
}

func TestBuildDesiredStateDoesNotTreatOpenAssigneesAsDeadDemand(t *testing.T) {
	for _, state := range []string{"active", "idle", "asleep", ""} {
		t.Run("state_"+state, func(t *testing.T) {
			store := beads.NewMemStore()
			cfg := deadAssigneeDemandConfig(2, 0)
			template := cfg.Agents[0].QualifiedName()
			session := deadAssigneeSessionBead("session-open", "worker-open", template, "open")
			session.Metadata["state"] = state
			if _, err := store.Create(session); err != nil {
				t.Fatalf("create open session bead: %v", err)
			}
			if _, err := store.Create(beads.Bead{
				ID:       "ga-open-assignee",
				Title:    "ready work held by an open worker",
				Type:     "task",
				Status:   "open",
				Assignee: "worker-open",
				Metadata: map[string]string{"gc.routed_to": template},
			}); err != nil {
				t.Fatalf("create assigned routed work: %v", err)
			}

			result := buildDesiredStateWithSessionBeads(
				"test-city", t.TempDir(), time.Now().UTC(), cfg, runtime.NewFake(),
				store, nil, newSessionBeadSnapshot([]beads.Bead{session}), nil, io.Discard,
			)

			if got := result.ScaleCheckCounts[template]; got != 0 {
				t.Fatalf("ScaleCheckCounts[%q] = %d, want 0: open session state %q is not confirmed-dead fallback demand", template, got, state)
			}
			requests := PoolDesiredCounts(ComputePoolDesiredStates(cfg, result.AssignedWorkBeads, []beads.Bead{session}, result.ScaleCheckCounts))
			if got := requests[template]; got != 1 {
				t.Fatalf("pool desired for open session state %q = %d, want 1 resume demand for the existing session", state, got)
			}
		})
	}
}

func TestBuildDesiredStateDoesNotTreatUncertainAssigneeAsDeadDemand(t *testing.T) {
	store := beads.NewMemStore()
	cfg := deadAssigneeDemandConfig(1, 0)
	template := cfg.Agents[0].QualifiedName()
	if _, err := store.Create(beads.Bead{
		ID:       "ga-unknown-assignee",
		Title:    "ready work held by an unknown assignee",
		Type:     "task",
		Status:   "open",
		Assignee: "worker-never-seen",
		Metadata: map[string]string{"gc.routed_to": template},
	}); err != nil {
		t.Fatalf("create assigned routed work: %v", err)
	}

	result := buildDesiredStateWithSessionBeads(
		"test-city", t.TempDir(), time.Now().UTC(), cfg, runtime.NewFake(),
		store, nil, newSessionBeadSnapshot(nil), nil, io.Discard,
	)

	if got := result.ScaleCheckCounts[template]; got != 0 {
		t.Fatalf("ScaleCheckCounts[%q] = %d, want 0 for unresolvable/uncertain assignee", template, got)
	}
	if len(result.State) != 0 {
		t.Fatalf("desired session count = %d, want 0 for unresolvable/uncertain assignee; state=%v", len(result.State), result.State)
	}
}

func TestFilterAssignedWorkBeadsForPoolDemandSkipsAmbiguousDeadAssignee(t *testing.T) {
	cfg := &config.City{Agents: []config.Agent{{Name: "worker"}}}
	sessions := []beads.Bead{
		deadAssigneeSessionBead("session-dead-a", "shared-dead", "worker", "closed"),
		deadAssigneeSessionBead("session-dead-b", "shared-dead", "worker", "closed"),
	}
	work := []beads.Bead{{
		ID:       "ga-ambiguous-dead-assignee",
		Type:     "task",
		Status:   "open",
		Assignee: "shared-dead",
	}}

	got := filterAssignedWorkBeadsForPoolDemand(cfg, "", sessions, work, []string{""})

	if len(got) != 0 {
		t.Fatalf("filtered work = %#v, want empty because ambiguous dead-session identity is uncertain, not confirmed-dead demand", got)
	}
}

func TestFilterAssignedWorkBeadsForPoolDemandUsesClosedSessionTemplateFallback(t *testing.T) {
	cfg := &config.City{Agents: []config.Agent{{Name: "primary"}, {Name: "fallback"}}}
	closed := deadAssigneeSessionBead("session-dead", "fallback-dead", "fallback", "closed")
	work := []beads.Bead{{
		ID:       "ga-assignee-only",
		Type:     "task",
		Status:   "open",
		Assignee: "fallback-dead",
		Metadata: map[string]string{},
	}}

	got := filterAssignedWorkBeadsForPoolDemand(cfg, "", []beads.Bead{closed}, work, []string{""})

	if len(got) != 1 || got[0].ID != "ga-assignee-only" {
		t.Fatalf("filtered work = %#v, want assignee-only work mapped through the confirmed-dead session template", got)
	}
}

func TestDeadAssigneeDemandMapsAssigneeTemplateBeforeClosedSessionTemplate(t *testing.T) {
	cfg := &config.City{Agents: []config.Agent{{Name: "worker"}, {Name: "fallback"}}}
	closed := deadAssigneeSessionBead("session-dead", "worker", "fallback", "closed")
	work := []beads.Bead{{
		ID:       "ga-assignee-template-wins",
		Type:     "task",
		Status:   "open",
		Assignee: "worker",
	}}

	states := ComputePoolDesiredStates(cfg, filterAssignedWorkBeadsForPoolDemand(cfg, "", []beads.Bead{closed}, work, []string{""}), []beads.Bead{closed}, nil)
	counts := PoolDesiredCounts(states)

	if got := counts["worker"]; got != 1 {
		t.Fatalf("worker pool desired = %d, want 1 because assignee identity maps to configured template before closed-session template fallback", got)
	}
	if got := counts["fallback"]; got != 0 {
		t.Fatalf("fallback pool desired = %d, want 0 because assignee-template mapping must outrank closed-session template fallback", got)
	}
}

func TestDeadAssigneeDemandPreservesRouteTemplatePrecedence(t *testing.T) {
	cfg := &config.City{Agents: []config.Agent{{Name: "preferred"}, {Name: "fallback"}}}
	closed := deadAssigneeSessionBead("session-dead", "fallback-dead", "fallback", "closed")
	work := []beads.Bead{{
		ID:       "ga-route-wins",
		Type:     "task",
		Status:   "open",
		Assignee: "fallback-dead",
		Metadata: map[string]string{"gc.routed_to": "preferred"},
	}}

	states := ComputePoolDesiredStates(cfg, filterAssignedWorkBeadsForPoolDemand(cfg, "", []beads.Bead{closed}, work, []string{""}), []beads.Bead{closed}, nil)
	counts := PoolDesiredCounts(states)

	if got := counts["preferred"]; got != 1 {
		t.Fatalf("preferred pool desired = %d, want 1 because gc.routed_to outranks the dead session's template", got)
	}
	if got := counts["fallback"]; got != 0 {
		t.Fatalf("fallback pool desired = %d, want 0 because gc.routed_to must win over closed-session template fallback", got)
	}
}

func TestDeadAssigneeDemandHonorsReadyExcludeTypesAndBlockingDependencies(t *testing.T) {
	for _, tc := range []struct {
		name  string
		setup func(t *testing.T, store *beads.MemStore, template string)
	}{
		{
			name: "graph step excluded by readyExcludeTypes",
			setup: func(t *testing.T, store *beads.MemStore, template string) {
				t.Helper()
				if _, err := store.Create(beads.Bead{
					ID:       "ga-graph-step",
					Title:    "graph.v2 drain step",
					Type:     "step",
					Status:   "open",
					Assignee: "worker-dead",
					Metadata: map[string]string{
						"gc.routed_to":          template,
						"gc.kind":               "workflow",
						"gc.formula_contract":   "graph.v2",
						"gc.workflow_member_id": "drain-unit-member",
					},
				}); err != nil {
					t.Fatalf("create graph step: %v", err)
				}
			},
		},
		{
			name: "blocking dependency excludes ready task",
			setup: func(t *testing.T, store *beads.MemStore, template string) {
				t.Helper()
				blocker, err := store.Create(beads.Bead{ID: "ga-blocker", Title: "blocker", Type: "task", Status: "open"})
				if err != nil {
					t.Fatalf("create blocker: %v", err)
				}
				blocked, err := store.Create(beads.Bead{
					ID:       "ga-blocked",
					Title:    "blocked routed work",
					Type:     "task",
					Status:   "open",
					Assignee: "worker-dead",
					Metadata: map[string]string{"gc.routed_to": template},
				})
				if err != nil {
					t.Fatalf("create blocked work: %v", err)
				}
				if err := store.DepAdd(blocked.ID, blocker.ID, "blocks"); err != nil {
					t.Fatalf("add blocking dependency: %v", err)
				}
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := beads.NewMemStore()
			cfg := deadAssigneeDemandConfig(1, 0)
			template := cfg.Agents[0].QualifiedName()
			if _, err := store.Create(deadAssigneeSessionBead("session-dead", "worker-dead", template, "closed")); err != nil {
				t.Fatalf("create closed session bead: %v", err)
			}
			tc.setup(t, store, template)

			result := buildDesiredStateWithSessionBeads(
				"test-city", t.TempDir(), time.Now().UTC(), cfg, runtime.NewFake(),
				store, nil, newSessionBeadSnapshot(nil), nil, io.Discard,
			)

			if got := result.ScaleCheckCounts[template]; got != 0 {
				t.Fatalf("ScaleCheckCounts[%q] = %d, want 0 for non-actionable dead-assignee work", template, got)
			}
			if len(result.State) != 0 {
				t.Fatalf("desired session count = %d, want 0 for non-actionable dead-assignee work; state=%v", len(result.State), result.State)
			}
		})
	}
}

func TestCityRuntimeBeadReconcileTickStrandedRoutedDemandAutoDefaultWarnsForAllWriters(t *testing.T) {
	for _, tc := range []struct {
		name string
		bead beads.Bead
	}{
		{
			name: "sling style",
			bead: strandedRoutedWorkBead("ga-sling", "worker"),
		},
		{
			name: "direct metadata",
			bead: strandedRoutedWorkBead("ga-direct", "worker"),
		},
		{
			name: "order dispatch pool-demand wisp",
			bead: strandedOrderPoolDemandWisp("mol-worker-order", "worker", "graph-drain"),
		},
		{
			name: "gh-3872 incident-5 graph.v2 drain-unit member",
			bead: strandedGraphV2DrainUnitMember("ga-f4tu7c-incident-5", "worker"),
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := beads.NewMemStore()
			bead := tc.bead
			bead.CreatedAt = time.Now().UTC().Add(-2 * time.Minute)
			if _, err := store.Create(bead); err != nil {
				t.Fatalf("create stranded work: %v", err)
			}
			rec := events.NewFake()
			cr := strandedDemandRuntime(t, store, rec, deadAssigneeDemandConfig(1, 0))

			cr.beadReconcileTick(context.Background(), strandedDemandResult("worker"), newSessionBeadSnapshot(nil), nil)

			ev := requireOneRoutedDemandStrandedEvent(t, rec)
			payload := requireEventPayload(t, ev)
			requirePayloadString(t, payload, "severity", "warning")
			requirePayloadString(t, payload, "template", "worker")
			requirePayloadIncludesBeadID(t, payload, bead.ID)
			if hasEventType(rec, events.OrderFailed) {
				t.Fatalf("Auto policy emitted %s, want warning-only routed-demand event", events.OrderFailed)
			}
			assertBeadStillReadyAndUngated(t, store, bead.ID)
		})
	}
}

func TestCityRuntimeBeadReconcileTickStrandedRoutedDemandPolicyModes(t *testing.T) {
	for _, tc := range []struct {
		name            string
		mode            string
		wantStranded    bool
		wantSeverity    string
		wantOrderFailed bool
	}{
		{name: "off kill switch is silent", mode: "off"},
		{name: "auto warns only", mode: "auto", wantStranded: true, wantSeverity: "warning"},
		{name: "require fails and mirrors order failure", mode: "require", wantStranded: true, wantSeverity: "failure", wantOrderFailed: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := beads.NewMemStore()
			bead := strandedOrderPoolDemandWisp("mol-worker-order", "worker", "graph-drain")
			bead.CreatedAt = time.Now().UTC().Add(-2 * time.Minute)
			if _, err := store.Create(bead); err != nil {
				t.Fatalf("create stranded order work: %v", err)
			}
			cfg := deadAssigneeDemandConfig(1, 0)
			setDemandConfigString(t, cfg, "StrandedRoutePolicy", tc.mode)
			rec := events.NewFake()
			cr := strandedDemandRuntime(t, store, rec, cfg)

			cr.beadReconcileTick(context.Background(), strandedDemandResult("worker"), newSessionBeadSnapshot(nil), nil)

			gotEvents := routedDemandStrandedEvents(rec)
			if !tc.wantStranded {
				if len(gotEvents) != 0 {
					t.Fatalf("stranded events = %d, want 0 when policy=%s", len(gotEvents), tc.mode)
				}
				if hasEventType(rec, events.OrderFailed) {
					t.Fatalf("unexpected %s when policy=%s", events.OrderFailed, tc.mode)
				}
				assertBeadStillReadyAndUngated(t, store, bead.ID)
				return
			}
			if len(gotEvents) != 1 {
				t.Fatalf("stranded events = %d, want 1 when policy=%s; events=%+v", len(gotEvents), tc.mode, rec.Events)
			}
			payload := requireEventPayload(t, gotEvents[0])
			requirePayloadString(t, payload, "severity", tc.wantSeverity)
			requirePayloadIncludesBeadID(t, payload, bead.ID)
			if has := hasEventType(rec, events.OrderFailed); has != tc.wantOrderFailed {
				t.Fatalf("has %s = %v, want %v for policy=%s; events=%+v", events.OrderFailed, has, tc.wantOrderFailed, tc.mode, rec.Events)
			}
			assertBeadStillReadyAndUngated(t, store, bead.ID)
		})
	}
}

func TestCityRuntimeBeadReconcileTickStrandedRoutedDemandDebounceAndExclusions(t *testing.T) {
	for _, tc := range []struct {
		name      string
		bead      func(now time.Time) beads.Bead
		wantEvent bool
	}{
		{
			name: "just under debounce is silent",
			bead: func(now time.Time) beads.Bead {
				b := strandedRoutedWorkBead("ga-fresh", "worker")
				b.CreatedAt = now.Add(-59 * time.Second)
				return b
			},
		},
		{
			name: "just over debounce emits",
			bead: func(now time.Time) beads.Bead {
				b := strandedRoutedWorkBead("ga-stale", "worker")
				b.CreatedAt = now.Add(-61 * time.Second)
				return b
			},
			wantEvent: true,
		},
		{
			name: "molecule without pool-demand flag remains excluded",
			bead: func(now time.Time) beads.Bead {
				return beads.Bead{
					ID:        "mol-no-demand",
					Title:     "workflow container without pool demand",
					Type:      "molecule",
					Status:    "open",
					CreatedAt: now.Add(-2 * time.Minute),
					Metadata:  map[string]string{"gc.routed_to": "worker"},
				}
			},
		},
		{
			name: "blocked routed task remains excluded",
			bead: func(now time.Time) beads.Bead {
				b := strandedRoutedWorkBead("ga-blocked", "worker")
				b.CreatedAt = now.Add(-2 * time.Minute)
				return b
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			now := time.Now().UTC()
			store := beads.NewMemStore()
			bead := tc.bead(now)
			if _, err := store.Create(bead); err != nil {
				t.Fatalf("create routed work: %v", err)
			}
			if bead.ID == "ga-blocked" {
				blocker, err := store.Create(beads.Bead{ID: "ga-blocker", Title: "blocker", Type: "task", Status: "open"})
				if err != nil {
					t.Fatalf("create blocker: %v", err)
				}
				if err := store.DepAdd(bead.ID, blocker.ID, "blocks"); err != nil {
					t.Fatalf("add blocking dependency: %v", err)
				}
			}
			cfg := deadAssigneeDemandConfig(1, 0)
			setDemandConfigString(t, cfg, "StrandedRoutePolicy", "auto")
			setDemandConfigString(t, cfg, "StrandedRouteDebounce", "1m")
			rec := events.NewFake()
			cr := strandedDemandRuntime(t, store, rec, cfg)

			cr.beadReconcileTick(context.Background(), strandedDemandResult("worker"), newSessionBeadSnapshot(nil), nil)

			got := len(routedDemandStrandedEvents(rec))
			if tc.wantEvent && got != 1 {
				t.Fatalf("stranded events = %d, want 1; events=%+v", got, rec.Events)
			}
			if !tc.wantEvent && got != 0 {
				t.Fatalf("stranded events = %d, want 0; events=%+v", got, rec.Events)
			}
			assertBeadStillReadyAndUngated(t, store, bead.ID)
		})
	}
}

func deadAssigneeDemandConfig(max, min int) *config.City {
	return &config.City{
		Agents: []config.Agent{{
			Name:              "worker",
			MaxActiveSessions: &max,
			MinActiveSessions: &min,
			Provider:          "mock",
			StartCommand:      "true",
		}},
		Providers: map[string]config.ProviderSpec{"mock": {Command: "true"}},
	}
}

func deadAssigneeSessionBead(id, sessionName, template, status string) beads.Bead {
	return beads.Bead{
		ID:     id,
		Title:  template,
		Type:   sessionBeadType,
		Status: status,
		Labels: []string{sessionBeadLabel, "agent:" + template},
		Metadata: map[string]string{
			"session_name":         sessionName,
			"template":             template,
			"agent_name":           template,
			"pool_slot":            "1",
			poolManagedMetadataKey: boolMetadata(true),
			"state":                "active",
		},
	}
}

func strandedRoutedWorkBead(id, template string) beads.Bead {
	return beads.Bead{
		ID:       id,
		Title:    "stranded routed work",
		Type:     "task",
		Status:   "open",
		Metadata: map[string]string{"gc.routed_to": template},
	}
}

func strandedOrderPoolDemandWisp(id, template, order string) beads.Bead {
	return beads.Bead{
		ID:        id,
		Title:     "order-dispatch pool-demand wisp",
		Type:      "molecule",
		Status:    "open",
		Ephemeral: true,
		Labels:    []string{"order-run:" + order},
		Metadata:  strandedPoolDemandMetadata(template),
	}
}

func strandedPoolDemandMetadata(template string) map[string]string {
	metadata := map[string]string{"gc.routed_to": template}
	for k, v := range poolDemandMetadataPair() {
		metadata[k] = v
	}
	return metadata
}

func strandedGraphV2DrainUnitMember(id, template string) beads.Bead {
	return beads.Bead{
		ID:     id,
		Title:  "GH #3872 incident #5 graph.v2 drain-unit member",
		Type:   "task",
		Status: "open",
		Metadata: map[string]string{
			"gc.routed_to":          template,
			"gc.kind":               "workflow",
			"gc.formula_contract":   "graph.v2",
			"gc.workflow_id":        "mol-drain-graph",
			"gc.workflow_member_id": "drain-unit-member",
		},
	}
}

func strandedDemandRuntime(t *testing.T, store beads.Store, rec *events.Fake, cfg *config.City) *CityRuntime {
	t.Helper()
	return &CityRuntime{
		cityPath:            t.TempDir(),
		cityName:            "maintainer-city",
		cfg:                 cfg,
		sp:                  runtime.NewFake(),
		standaloneCityStore: store,
		sessionDrains:       newDrainTracker(),
		rec:                 rec,
		stdout:              io.Discard,
		stderr:              io.Discard,
	}
}

func strandedDemandResult(template string) DesiredStateResult {
	return DesiredStateResult{
		State:             map[string]TemplateParams{},
		ScaleCheckCounts:  map[string]int{template: 0},
		PoolDesiredCounts: map[string]int{template: 0},
	}
}

func routedDemandStrandedEvents(rec *events.Fake) []events.Event {
	var out []events.Event
	for _, ev := range rec.Events {
		if ev.Type == routedDemandStrandedEventType {
			out = append(out, ev)
		}
	}
	return out
}

func requireOneRoutedDemandStrandedEvent(t *testing.T, rec *events.Fake) events.Event {
	t.Helper()
	got := routedDemandStrandedEvents(rec)
	if len(got) != 1 {
		t.Fatalf("stranded events = %d, want 1; events=%+v", len(got), rec.Events)
	}
	return got[0]
}

func requireEventPayload(t *testing.T, ev events.Event) map[string]any {
	t.Helper()
	if len(ev.Payload) == 0 {
		t.Fatalf("%s payload is empty; event=%+v", ev.Type, ev)
	}
	var payload map[string]any
	if err := json.Unmarshal(ev.Payload, &payload); err != nil {
		t.Fatalf("unmarshal %s payload: %v; raw=%s", ev.Type, err, ev.Payload)
	}
	return payload
}

func requirePayloadString(t *testing.T, payload map[string]any, key, want string) {
	t.Helper()
	if got, ok := payload[key].(string); !ok || got != want {
		t.Fatalf("payload[%q] = %#v, want %q; payload=%v", key, payload[key], want, payload)
	}
}

func requirePayloadIncludesBeadID(t *testing.T, payload map[string]any, beadID string) {
	t.Helper()
	if got, ok := payload["bead_id"].(string); ok && got == beadID {
		return
	}
	if values, ok := payload["bead_ids"].([]any); ok {
		for _, value := range values {
			if got, ok := value.(string); ok && got == beadID {
				return
			}
		}
	}
	t.Fatalf("payload does not include bead id %q: %v", beadID, payload)
}

func hasEventType(rec *events.Fake, eventType string) bool {
	for _, ev := range rec.Events {
		if ev.Type == eventType {
			return true
		}
	}
	return false
}

func assertBeadStillReadyAndUngated(t *testing.T, store beads.Store, beadID string) {
	t.Helper()
	got, err := store.Get(beadID)
	if err != nil {
		t.Fatalf("get %s: %v", beadID, err)
	}
	if got.Status != "open" {
		t.Fatalf("bead %s status = %q, want open: %+v", beadID, got.Status, got)
	}
	if strings.TrimSpace(got.Assignee) != "" {
		t.Fatalf("bead %s assignee = %q, want empty", beadID, got.Assignee)
	}
	for _, label := range got.Labels {
		if strings.HasPrefix(label, "hold:") {
			t.Fatalf("bead %s labels = %v, want no fail-loud hold label", beadID, got.Labels)
		}
	}
	ready, err := store.Ready(beads.ReadyQuery{})
	if err != nil {
		t.Fatalf("Ready(): %v", err)
	}
	for _, bead := range ready {
		if bead.ID == beadID {
			return
		}
	}
	t.Fatalf("bead %s was not Ready() after fail-loud detection; ready=%+v", beadID, ready)
}

func setDemandConfigString(t *testing.T, cfg *config.City, fieldName, value string) {
	t.Helper()
	root := reflect.ValueOf(cfg)
	if root.Kind() != reflect.Pointer || root.IsNil() {
		t.Fatalf("config must be a non-nil *config.City")
	}
	demand := root.Elem().FieldByName("Demand")
	if !demand.IsValid() {
		t.Fatalf("config.City missing Demand field; ga-o3ko1j.4.1 requires top-level [demand] config with %s", fieldName)
	}
	field := demand.FieldByName(fieldName)
	if !field.IsValid() {
		t.Fatalf("config.City.Demand missing %s field", fieldName)
	}
	if !field.CanSet() || field.Kind() != reflect.String {
		t.Fatalf("config.City.Demand.%s must be a settable string field, got kind=%s canSet=%v", fieldName, field.Kind(), field.CanSet())
	}
	field.SetString(value)
}
