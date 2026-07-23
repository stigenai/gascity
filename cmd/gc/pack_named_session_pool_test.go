package main

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/agentutil"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/clock"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/fsys"
	"github.com/gastownhall/gascity/internal/runtime"
)

func TestPackNamedSessionPoolPatchWarmsFromBaseSessionToMin(t *testing.T) {
	for _, tc := range []struct {
		name  string
		rig   string
		agent string
	}{
		{name: "gascity builder", rig: "gascity", agent: "builder"},
		{name: "cairn reviewer", rig: "cairn", agent: "reviewer"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fixture := newPackNamedPoolFixture(t, tc.rig, tc.agent, 2, 3)
			store := beads.NewMemStore()
			provider := runtime.NewFake()
			clk := &clock.Fake{Time: time.Date(2026, 7, 23, 12, 0, 0, 0, time.UTC)}

			base := createPackNamedBaseSessionBead(t, store, fixture.cfg, fixture.identity)
			if err := provider.Start(context.Background(), base.Metadata["session_name"], runtime.Config{}); err != nil {
				t.Fatalf("starting base named session: %v", err)
			}
			createAssignedWorkBead(t, store, "resume work", base.ID, fixture.identity)

			var stdout, stderr bytes.Buffer
			_, sessions, _ := reconcileConfiguredSessionsOnce(t, fixture.cityPath, store, fixture.cfg, provider, clk, &stdout, &stderr)

			if got := countOpenTemplateSessions(sessions, fixture.identity); got != 2 {
				t.Fatalf("open sessions for %s = %d, want 2 (base named session plus one pool instance)", fixture.identity, got)
			}
			instances := poolInstancesByAgentName(sessions, fixture.identity)
			instance, ok := instances[fixture.identity+"-1"]
			if !ok {
				t.Fatalf("expected materialized pool instance %s-1; got instances %v", fixture.identity, keysOf(instances))
			}
			if _, ok := instances[fixture.identity+"-2"]; ok {
				t.Fatalf("unexpected %s-2; base named session must count toward min_active_sessions", fixture.identity)
			}
			if got := instance.Metadata["pool_slot"]; got != "1" {
				t.Fatalf("pool slot for %s = %q, want 1", instance.ID, got)
			}

			resolvedID, err := resolveSessionIDWithConfig(fixture.cityPath, fixture.cfg, store, fixture.identity+"-1")
			if err != nil {
				t.Fatalf("resolving materialized instance %s-1 for wake: %v", fixture.identity, err)
			}
			if resolvedID != instance.ID {
				t.Fatalf("resolved %s-1 to %s, want %s", fixture.identity, resolvedID, instance.ID)
			}

			resolvedAgent, ok := resolveAgentIdentity(fixture.cfg, fixture.identity+"-1", "")
			if !ok {
				t.Fatalf("resolving materialized instance %s-1 for sling failed", fixture.identity)
			}
			if got := resolvedAgent.QualifiedName(); got != fixture.identity+"-1" {
				t.Fatalf("resolved sling target = %q, want %q", got, fixture.identity+"-1")
			}
			if got := agentutil.NormalizePoolRouteTarget(fixture.cfg, fixture.identity+"-1"); got != fixture.identity {
				t.Fatalf("normalized sling route = %q, want %q", got, fixture.identity)
			}
		})
	}
}

func TestComputePoolDesiredStatesPoolShapedNamedSessionCountsBaseResumeTowardMax(t *testing.T) {
	fixture := newPackNamedPoolFixture(t, "gascity", "builder", 2, 3)
	base := packNamedBaseSessionBead(fixture.cfg, fixture.identity, "base-session")
	slot := packNamedPoolSessionBead(fixture.identity, 1, "slot-1")
	work := []beads.Bead{
		{
			ID:       "base-work",
			Title:    "base assigned work",
			Type:     "task",
			Status:   "in_progress",
			Assignee: base.ID,
			Metadata: map[string]string{"gc.routed_to": fixture.identity},
		},
		{
			ID:       "slot-work",
			Title:    "slot assigned work",
			Type:     "task",
			Status:   "in_progress",
			Assignee: slot.ID,
			Metadata: map[string]string{"gc.routed_to": fixture.identity},
		},
	}

	states := ComputePoolDesiredStates(
		fixture.cfg,
		work,
		[]beads.Bead{base, slot},
		map[string]int{fixture.identity: 3},
	)
	state, ok := findPoolDesiredState(states, fixture.identity)
	if !ok {
		t.Fatalf("desired state for %s not found in %#v", fixture.identity, states)
	}
	requests := state.Requests

	if got := len(requests); got != 3 {
		t.Fatalf("desired requests = %d, want max_active_sessions ceiling 3", got)
	}
	if !hasResumeRequestForSession(requests, base.ID) {
		t.Fatalf("expected base named session %s to count as a resume request toward max_active_sessions", base.ID)
	}
	if !hasResumeRequestForSession(requests, slot.ID) {
		t.Fatalf("expected pool instance %s to count as a resume request toward max_active_sessions", slot.ID)
	}
	if got := countAnonymousNewRequests(requests); got != 1 {
		t.Fatalf("anonymous new-spawn requests = %d, want 1 after counting base and slot resumes toward max", got)
	}
}

type packNamedPoolFixture struct {
	cityPath string
	cfg      *config.City
	identity string
}

func newPackNamedPoolFixture(t *testing.T, rigName, agentName string, minActive, maxActive int) packNamedPoolFixture {
	t.Helper()

	root := t.TempDir()
	rigPath := filepath.Join(root, "rigs", rigName)
	packPath := filepath.Join(rigPath, "packs", "workflow")
	mustMkdirAllPackNamed(t, packPath)

	writeFixtureFile(t, filepath.Join(root, "city.toml"), fmt.Sprintf(`
[workspace]
name = "test-city"
session_template = "{{.City}}--{{.Agent}}"

[[rigs]]
name = %q
path = %q
includes = ["packs/workflow"]

[[rigs.patches]]
agent = %q
min_active_sessions = %d
max_active_sessions = %d
`, rigName, rigPath, agentName, minActive, maxActive))

	writeFixtureFile(t, filepath.Join(packPath, "pack.toml"), fmt.Sprintf(`
[pack]
name = "workflow"
schema = 1

[[agent]]
name = %q
scope = "rig"
start_command = "true"
min_active_sessions = 0
max_active_sessions = 1

[[named_session]]
template = %q
scope = "rig"
mode = "on_demand"
`, agentName, agentName))

	cityPath := filepath.Join(root, "city.toml")
	cfg, _, err := config.LoadWithIncludes(fsys.OSFS{}, cityPath)
	if err != nil {
		t.Fatalf("loading fixture city config: %v", err)
	}

	identity := rigName + "/" + agentName
	agent := config.FindAgent(cfg, identity)
	if agent == nil {
		t.Fatalf("fixture agent %s not found", identity)
	}
	if agent.MinActiveSessions == nil || *agent.MinActiveSessions != minActive {
		t.Fatalf("fixture min_active_sessions = %v, want %d", agent.MinActiveSessions, minActive)
	}
	if agent.MaxActiveSessions == nil || *agent.MaxActiveSessions != maxActive {
		t.Fatalf("fixture max_active_sessions = %v, want %d", agent.MaxActiveSessions, maxActive)
	}
	if config.FindNamedSession(cfg, identity) == nil {
		t.Fatalf("fixture named session %s not found", identity)
	}

	return packNamedPoolFixture{cityPath: cityPath, cfg: cfg, identity: identity}
}

func createPackNamedBaseSessionBead(t *testing.T, store beads.Store, cfg *config.City, identity string) beads.Bead {
	t.Helper()

	created, err := store.Create(packNamedBaseSessionBead(cfg, identity, ""))
	if err != nil {
		t.Fatalf("creating base named session bead: %v", err)
	}
	return created
}

func packNamedBaseSessionBead(cfg *config.City, identity, id string) beads.Bead {
	sessionName := config.NamedSessionRuntimeName(cfg.EffectiveCityName(), cfg.Workspace, identity)
	return beads.Bead{
		ID:     id,
		Title:  identity,
		Type:   sessionBeadType,
		Status: "open",
		Labels: []string{sessionBeadLabel, "agent:" + identity},
		Metadata: map[string]string{
			"agent_name":                 identity,
			"alias":                      identity,
			"continuation_epoch":         "1",
			"generation":                 "1",
			"instance_token":             "base-token",
			"session_name":               sessionName,
			"state":                      "active",
			"template":                   identity,
			namedSessionIdentityMetadata: identity,
			namedSessionMetadataKey:      "true",
			namedSessionModeMetadata:     "on_demand",
		},
	}
}

func packNamedPoolSessionBead(identity string, slot int, id string) beads.Bead {
	instance := fmt.Sprintf("%s-%d", identity, slot)
	return beads.Bead{
		ID:     id,
		Title:  instance,
		Type:   sessionBeadType,
		Status: "open",
		Labels: []string{sessionBeadLabel, "agent:" + identity},
		Metadata: map[string]string{
			"agent_name":         instance,
			"alias":              instance,
			"continuation_epoch": "1",
			"generation":         "1",
			"instance_token":     fmt.Sprintf("slot-%d-token", slot),
			"pool_managed":       "true",
			"pool_slot":          fmt.Sprintf("%d", slot),
			"session_name":       "session-" + instance,
			"state":              "active",
			"template":           identity,
		},
	}
}

func createAssignedWorkBead(t *testing.T, store beads.Store, title, assignee, routedTo string) {
	t.Helper()

	if _, err := store.Create(beads.Bead{
		Title:    title,
		Type:     "task",
		Status:   "in_progress",
		Assignee: assignee,
		Metadata: map[string]string{"gc.routed_to": routedTo},
	}); err != nil {
		t.Fatalf("creating assigned work bead: %v", err)
	}
}

func countOpenTemplateSessions(sessions []beads.Bead, template string) int {
	count := 0
	for _, session := range sessions {
		if session.Status == "open" && session.Metadata["template"] == template {
			count++
		}
	}
	return count
}

func poolInstancesByAgentName(sessions []beads.Bead, template string) map[string]beads.Bead {
	instances := make(map[string]beads.Bead)
	for _, session := range sessions {
		if session.Status != "open" || session.Metadata["template"] != template {
			continue
		}
		if session.Metadata["pool_managed"] != "true" {
			continue
		}
		agentName := session.Metadata["agent_name"]
		if agentName == "" {
			agentName = session.Metadata["alias"]
		}
		if agentName != "" {
			instances[agentName] = session
		}
	}
	return instances
}

func findPoolDesiredState(states []PoolDesiredState, template string) (PoolDesiredState, bool) {
	for _, state := range states {
		if state.Template == template {
			return state, true
		}
	}
	return PoolDesiredState{}, false
}

func hasResumeRequestForSession(requests []SessionRequest, sessionID string) bool {
	for _, request := range requests {
		if request.Tier == "resume" && request.SessionBeadID == sessionID {
			return true
		}
	}
	return false
}

func countAnonymousNewRequests(requests []SessionRequest) int {
	count := 0
	for _, request := range requests {
		if request.Tier == "new" && request.SessionBeadID == "" {
			count++
		}
	}
	return count
}

func keysOf[K comparable, V any](values map[K]V) []K {
	keys := make([]K, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	return keys
}

func mustMkdirAllPackNamed(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(path, 0o755); err != nil {
		t.Fatalf("creating fixture dir %s: %v", path, err)
	}
}

func writeFixtureFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("writing fixture file %s: %v", path, err)
	}
}
