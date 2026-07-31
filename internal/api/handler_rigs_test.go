package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/runtime"
)

func putExecutableOnPath(t *testing.T, name string) {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatalf("write executable %s: %v", name, err)
	}
	t.Setenv("PATH", dir)
}

func TestRigList(t *testing.T) {
	state := newFakeState(t)
	h := newTestCityHandler(t, state)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", cityURL(state, "/rigs"), nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}

	var resp listResponse
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Total != 1 {
		t.Fatalf("total = %d, want 1", resp.Total)
	}
}

func TestRigListUsesOneRuntimeSnapshot(t *testing.T) {
	state := newFakeState(t)
	state.cfg.Agents = []config.Agent{
		{Name: "worker", Dir: "myrig", MaxActiveSessions: intPtr(-1), ProcessNames: []string{"agent-cli"}},
		{Name: "coder", Dir: "myrig", MaxActiveSessions: intPtr(1), ProcessNames: []string{"agent-cli"}},
	}
	if err := state.sp.Start(context.Background(), "myrig--worker-1", runtime.Config{ProcessNames: []string{"agent-cli"}}); err != nil {
		t.Fatalf("start worker: %v", err)
	}
	if err := state.sp.Start(context.Background(), "myrig--coder", runtime.Config{ProcessNames: []string{"agent-cli"}}); err != nil {
		t.Fatalf("start coder: %v", err)
	}
	provider := &countingIsRunningProvider{Provider: state.sp}
	state.sessionProvider = provider
	h := newTestCityHandler(t, state)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", cityURL(state, "/rigs"), nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	var resp struct {
		Items []rigResponse `json:"items"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(resp.Items) != 1 || resp.Items[0].RunningCount != 2 {
		t.Fatalf("items = %+v, want one rig with two running sessions", resp.Items)
	}
	if provider.listRunningCalls != 1 {
		t.Fatalf("ListRunning calls = %d, want one request snapshot", provider.listRunningCalls)
	}
	if provider.isRunningCalls != 0 {
		t.Fatalf("IsRunning calls = %d, want zero per-agent probes", provider.isRunningCalls)
	}
	if provider.processAliveCalls != 0 {
		t.Fatalf("ProcessAlive calls = %d, want zero per-agent probes", provider.processAliveCalls)
	}
	if provider.getMetaCalls != 2 {
		t.Fatalf("GetMeta calls = %d, want one suspended-state probe per running agent", provider.getMetaCalls)
	}
	if provider.getLastActivityCalls != 2 {
		t.Fatalf("GetLastActivity calls = %d, want one activity probe per running agent", provider.getLastActivityCalls)
	}
	if provider.isAttachedCalls != 0 {
		t.Fatalf("IsAttached calls = %d, want zero unused rig-list probes", provider.isAttachedCalls)
	}
}

func TestRigListFallsBackWhenRuntimeSnapshotFails(t *testing.T) {
	state := newFakeState(t)
	state.cfg.Agents = []config.Agent{
		{Name: "worker", Dir: "myrig", MaxActiveSessions: intPtr(1)},
	}
	if err := state.sp.Start(context.Background(), "myrig--worker", runtime.Config{}); err != nil {
		t.Fatalf("start worker: %v", err)
	}
	counting := &countingIsRunningProvider{Provider: state.sp}
	provider := &failingSessionListProvider{Provider: counting}
	state.sessionProvider = provider
	h := newTestCityHandler(t, state)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", cityURL(state, "/rigs"), nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	var resp struct {
		Items []rigResponse `json:"items"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(resp.Items) != 1 || resp.Items[0].RunningCount != 1 {
		t.Fatalf("items = %+v, want running session from fallback probes", resp.Items)
	}
	if provider.listRunningCalls != 1 {
		t.Fatalf("ListRunning calls = %d, want one failed snapshot attempt", provider.listRunningCalls)
	}
	if counting.isRunningCalls == 0 {
		t.Fatal("IsRunning calls = 0, want per-agent fallback probes")
	}
}

func TestRigGet(t *testing.T) {
	state := newFakeState(t)
	h := newTestCityHandler(t, state)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", cityURL(state, "/rig/myrig"), nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}

	var rig rigResponse
	if err := json.NewDecoder(rec.Body).Decode(&rig); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if rig.Name != "myrig" {
		t.Fatalf("name = %q, want %q", rig.Name, "myrig")
	}
}

func TestRigGetNotFound(t *testing.T) {
	state := newFakeState(t)
	h := newTestCityHandler(t, state)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", cityURL(state, "/rig/nonexistent"), nil))

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
}

func TestRigEnrichment(t *testing.T) {
	state := newFakeState(t)
	state.cfg.Agents = []config.Agent{
		{Name: "worker", Dir: "myrig", MaxActiveSessions: intPtr(1)},
		{Name: "coder", Dir: "myrig", MaxActiveSessions: intPtr(1)},
	}
	state.sp.Start(context.Background(), "myrig--worker", runtime.Config{}) //nolint:errcheck
	h := newTestCityHandler(t, state)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", cityURL(state, "/rig/myrig"), nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}

	var rig rigResponse
	json.NewDecoder(rec.Body).Decode(&rig) //nolint:errcheck
	if rig.AgentCount != 2 {
		t.Errorf("AgentCount = %d, want 2", rig.AgentCount)
	}
	if rig.RunningCount != 1 {
		t.Errorf("RunningCount = %d, want 1", rig.RunningCount)
	}
}

type falseNegativeSessionProvider struct {
	*runtime.Fake
}

func (p *falseNegativeSessionProvider) IsRunning(name string) bool {
	_ = p.Fake.IsRunning(name)
	return false
}

type sessionProviderOverrideState struct {
	*fakeState
	provider runtime.Provider
}

func (s *sessionProviderOverrideState) SessionProvider() runtime.Provider {
	return s.provider
}

func TestRigEnrichmentUsesProcessNamesForRuntimeFalseNegative(t *testing.T) {
	base := newFakeState(t)
	base.cfg.Agents = []config.Agent{
		{Name: "worker", Dir: "myrig", Provider: "test-agent", MaxActiveSessions: intPtr(1), ProcessNames: []string{"agent-cli"}},
	}
	sp := &falseNegativeSessionProvider{Fake: runtime.NewFake()}
	if err := sp.Start(context.Background(), "myrig--worker", runtime.Config{ProcessNames: []string{"agent-cli"}}); err != nil {
		t.Fatalf("Start existing session: %v", err)
	}
	state := &sessionProviderOverrideState{
		fakeState: base,
		provider:  sp,
	}
	h := newTestCityHandler(t, state)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", cityURL(state, "/rig/myrig"), nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	var rig rigResponse
	if err := json.NewDecoder(rec.Body).Decode(&rig); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if rig.RunningCount != 1 {
		t.Errorf("RunningCount = %d, want 1", rig.RunningCount)
	}
}

func TestRigEnrichmentUsesExplicitProviderDetectedProcessNames(t *testing.T) {
	putExecutableOnPath(t, "codex")
	base := newFakeState(t)
	base.cfg.Workspace.Provider = ""
	base.cfg.Providers = map[string]config.ProviderSpec{
		"codex": config.BuiltinProviderAlias("codex"),
	}
	base.cfg.Agents = []config.Agent{
		{Name: "worker", Dir: "myrig", Provider: "codex", MaxActiveSessions: intPtr(1)},
	}
	sp := &falseNegativeSessionProvider{Fake: runtime.NewFake()}
	if err := sp.Start(context.Background(), "myrig--worker", runtime.Config{ProcessNames: []string{"codex"}}); err != nil {
		t.Fatalf("Start existing session: %v", err)
	}
	state := &sessionProviderOverrideState{
		fakeState: base,
		provider:  sp,
	}
	h := newTestCityHandler(t, state)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", cityURL(state, "/rig/myrig"), nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	var rig rigResponse
	if err := json.NewDecoder(rec.Body).Decode(&rig); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if rig.RunningCount != 1 {
		t.Errorf("RunningCount = %d, want 1", rig.RunningCount)
	}
}

func TestRigSuspendResume(t *testing.T) {
	state := newFakeMutatorState(t)
	h := newTestCityHandler(t, state)

	// Suspend rig.
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, newPostRequest(cityURL(state, "/rig/myrig/suspend"), nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("suspend: status = %d, want 200", rec.Code)
	}

	// Read-after-write: rig should show as suspended.
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", cityURL(state, "/rig/myrig"), nil))

	var rig rigResponse
	if err := json.NewDecoder(rec.Body).Decode(&rig); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !rig.Suspended {
		t.Fatal("rig should be suspended after suspend action")
	}

	// Resume rig.
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, newPostRequest(cityURL(state, "/rig/myrig/resume"), nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("resume: status = %d, want 200", rec.Code)
	}

	// Read-after-write: rig should show as not suspended.
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", cityURL(state, "/rig/myrig"), nil))

	if err := json.NewDecoder(rec.Body).Decode(&rig); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if rig.Suspended {
		t.Fatal("rig should not be suspended after resume action")
	}
}

func TestRigActionNotFound(t *testing.T) {
	state := newFakeMutatorState(t)
	h := newTestCityHandler(t, state)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, newPostRequest(cityURL(state, "/rig/nonexistent/suspend"), nil))

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
}

func TestRigActionUnknown(t *testing.T) {
	state := newFakeMutatorState(t)
	h := newTestCityHandler(t, state)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, newPostRequest(cityURL(state, "/rig/myrig/reboot"), nil))

	// RigActionInput.Action carries an enum:"suspend,resume,restart" schema, so
	// Huma rejects an unknown action at request validation with the typed
	// validation-failed contract (mirroring the agent-action surface) rather than
	// the pre-conversion legacy bare-404 body with empty code/type.
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422; body: %s", rec.Code, rec.Body.String())
	}
	var pd struct {
		Type string `json:"type"`
		Code string `json:"code"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&pd); err != nil {
		t.Fatalf("decode 422 body: %v", err)
	}
	if pd.Code != "validation-failed" {
		t.Errorf("code = %q, want validation-failed", pd.Code)
	}
	if pd.Type != "urn:gascity:error:validation-failed" {
		t.Errorf("type = %q, want urn:gascity:error:validation-failed", pd.Type)
	}
}
