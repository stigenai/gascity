package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/fsys"
	"github.com/gastownhall/gascity/internal/runtime"
	sessionpkg "github.com/gastownhall/gascity/internal/session"
)

type asyncCleanupCASStore struct {
	*beads.MemStore
	mu             sync.Mutex
	failArm        bool
	armErr         error
	failClearCount int
}

func (s *asyncCleanupCASStore) CompareAndSetMetadataKey(id, key, expected, value string) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if key == sessionpkg.AsyncStartCleanupObligationMetadataKey {
		if s.failArm && expected != "" && value != "" {
			if s.armErr != nil {
				return false, s.armErr
			}
			return false, errors.New("injected durable arm failure")
		}
		if value == "" && s.failClearCount > 0 {
			s.failClearCount--
			return false, errors.New("injected journal clear failure")
		}
	}
	return s.MemStore.CompareAndSetMetadataKey(id, key, expected, value)
}

func (s *asyncCleanupCASStore) CompareAndSetMetadataKeyContext(ctx context.Context, id, key, expected, value string) (bool, error) {
	s.mu.Lock()
	if key == sessionpkg.AsyncStartCleanupObligationMetadataKey && s.failArm && expected != "" && value != "" {
		err := s.armErr
		s.mu.Unlock()
		if err != nil {
			return false, err
		}
		return false, errors.New("injected durable arm failure")
	}
	s.mu.Unlock()
	return s.MemStore.CompareAndSetMetadataKeyContext(ctx, id, key, expected, value)
}

// synchronousOnlyBlockingCASStore deliberately offers only the legacy,
// synchronous conditional-writer seam. Its admitted-to-cleanup CAS blocks so
// shutdown tests prove that a bounded handoff never invokes a writer that can
// outlive its deadline. The embedded Store interface intentionally hides the
// MemStore's optional context-aware capability.
type synchronousOnlyBlockingCASStore struct {
	beads.Store
	writer     beads.ConditionalWriter
	armStarted chan struct{}
	armRelease chan struct{}
	armOnce    sync.Once
	armCalls   atomic.Int32
}

func newSynchronousOnlyBlockingCASStore(t *testing.T) *synchronousOnlyBlockingCASStore {
	t.Helper()
	backing := beads.NewMemStore()
	writer, ok := beads.ConditionalWriterFor(backing)
	if !ok {
		t.Fatal("MemStore conditional writer unavailable")
	}
	return &synchronousOnlyBlockingCASStore{
		Store:      backing,
		writer:     writer,
		armStarted: make(chan struct{}),
		armRelease: make(chan struct{}),
	}
}

func (s *synchronousOnlyBlockingCASStore) UpdateIfMatch(id string, revision int64, opts beads.UpdateOpts) error {
	return s.writer.UpdateIfMatch(id, revision, opts)
}

func (s *synchronousOnlyBlockingCASStore) CloseIfMatch(id string, revision int64) error {
	return s.writer.CloseIfMatch(id, revision)
}

func (s *synchronousOnlyBlockingCASStore) DeleteIfMatch(id string, revision int64) error {
	return s.writer.DeleteIfMatch(id, revision)
}

func (s *synchronousOnlyBlockingCASStore) CompareAndSetMetadataKey(id, key, expected, value string) (bool, error) {
	if key == sessionpkg.AsyncStartCleanupObligationMetadataKey && expected != "" && value != "" {
		s.armCalls.Add(1)
		s.armOnce.Do(func() { close(s.armStarted) })
		<-s.armRelease
	}
	return s.writer.CompareAndSetMetadataKey(id, key, expected, value)
}

type asyncCleanupProbeProvider struct {
	runtime.Provider
	token       string
	probeErr    error
	stopErr     error
	stopCalls   atomic.Int32
	probeCalls  atomic.Int32
	stopStarted chan struct{}
	stopRelease chan struct{}
}

type unsafeNameStopProbeProvider struct {
	runtime.Provider
	stopCalls atomic.Int32
}

func (p *unsafeNameStopProbeProvider) Stop(string) error {
	p.stopCalls.Add(1)
	return nil
}

func (p *asyncCleanupProbeProvider) GetMeta(_, key string) (string, error) {
	if key != "GC_INSTANCE_TOKEN" {
		return "", nil
	}
	p.probeCalls.Add(1)
	return p.token, p.probeErr
}

func (p *asyncCleanupProbeProvider) Stop(string) error {
	return errors.New("unsafe name-based Stop called by async cleanup")
}

func (p *asyncCleanupProbeProvider) StopIfInstanceToken(_ string, expectedToken string) error {
	if p.probeErr != nil {
		return p.probeErr
	}
	actualToken := strings.TrimSpace(p.token)
	if actualToken == "" {
		return runtime.ErrRuntimeUnavailable
	}
	if actualToken != strings.TrimSpace(expectedToken) {
		return runtime.ErrInstanceTokenMismatch
	}
	p.stopCalls.Add(1)
	if p.stopStarted != nil {
		close(p.stopStarted)
	}
	if p.stopRelease != nil {
		<-p.stopRelease
	}
	return p.stopErr
}

func seedAsyncStartCleanupObligation(t *testing.T, store beads.Store) (preparedStart, string) {
	t.Helper()
	return seedNamedAsyncStartCleanupObligation(t, store, "gc-worker", "worker")
}

// seedNamedAsyncStartCleanupObligation is seedAsyncStartCleanupObligation
// parameterized by bead ID and session name, so a single store can hold
// several concurrently admitted journals (e.g. to test per-sweep fan-out
// bounds) instead of just the one "gc-worker" fixture.
func seedNamedAsyncStartCleanupObligation(t *testing.T, store beads.Store, id, name string) (preparedStart, string) {
	t.Helper()
	const token = "tok-worker"
	created, err := store.Create(beads.Bead{
		ID:     id,
		Title:  name,
		Type:   sessionpkg.BeadType,
		Status: "open",
		Labels: []string{sessionpkg.LabelSession},
		Metadata: map[string]string{
			"session_name":         name,
			"template":             "worker",
			"state":                string(sessionpkg.StateCreating),
			"instance_token":       token,
			"pending_create_claim": "true",
		},
	})
	if err != nil {
		t.Fatalf("create session bead: %v", err)
	}
	info, err := sessionFrontDoor(store).Get(created.ID)
	if err != nil {
		t.Fatalf("load session bead: %v", err)
	}
	item := preparedStart{candidate: startCandidate{
		info: info,
		tp:   TemplateParams{SessionName: name, TemplateName: "worker", Command: "agent"},
	}}
	raw, err := claimAsyncStartCleanupObligation(sessionFrontDoor(store), item, time.Now(), time.Minute)
	if err != nil {
		t.Fatalf("claim async-start cleanup obligation: %v", err)
	}
	return item, raw
}

func TestSweepAsyncStartCleanupObligationsFailsClosedUnlessTokenExactlyMatches(t *testing.T) {
	tests := []struct {
		name         string
		arm          bool
		actualToken  string
		probeErr     error
		stopErr      error
		wantResolved int
		wantPending  int
		wantStops    int32
		wantMarker   bool
		wantAsleep   bool
	}{
		{name: "ordinary crash exact token adopts without stop", actualToken: "tok-worker", wantResolved: 1},
		{name: "armed exact nonempty token stops", arm: true, actualToken: "tok-worker", wantResolved: 1, wantStops: 1, wantAsleep: true},
		{name: "mismatch skips stop and releases old journal", arm: true, actualToken: "replacement", wantResolved: 1},
		{name: "empty token fails closed", wantPending: 1, wantMarker: true},
		{name: "probe error fails closed", probeErr: errors.New("transport unavailable"), wantPending: 1, wantMarker: true},
		{name: "stop error retains armed journal", arm: true, actualToken: "tok-worker", stopErr: errors.New("delete failed"), wantPending: 1, wantStops: 1, wantMarker: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			store := beads.NewMemStore()
			item, raw := seedAsyncStartCleanupObligation(t, store)
			if tt.arm {
				if _, armed, err := armAsyncStartCleanupObligation(sessionFrontDoor(store), item.candidate.info.ID, raw); err != nil || !armed {
					t.Fatalf("arm obligation = (%t,%v), want true,nil", armed, err)
				}
			}
			provider := &asyncCleanupProbeProvider{
				Provider: runtime.NewFake(),
				token:    tt.actualToken,
				probeErr: tt.probeErr,
				stopErr:  tt.stopErr,
			}
			resolved, pending, err := sweepAsyncStartCleanupObligations(provider, store, time.Now(), io.Discard)
			if err != nil {
				t.Fatalf("sweep: %v", err)
			}
			if resolved != tt.wantResolved || pending != tt.wantPending {
				t.Fatalf("sweep = (resolved=%d pending=%d), want (%d,%d)", resolved, pending, tt.wantResolved, tt.wantPending)
			}
			if got := provider.stopCalls.Load(); got != tt.wantStops {
				t.Fatalf("Stop calls = %d, want %d", got, tt.wantStops)
			}
			info, err := sessionFrontDoor(store).Get(item.candidate.info.ID)
			if err != nil {
				t.Fatalf("Get: %v", err)
			}
			if got := info.AsyncStartCleanupObligation != ""; got != tt.wantMarker {
				t.Fatalf("marker present = %t, want %t", got, tt.wantMarker)
			}
			if got := info.MetadataState == string(sessionpkg.StateAsleep); got != tt.wantAsleep {
				t.Fatalf("state = %q (asleep=%t), want asleep=%t", info.MetadataState, got, tt.wantAsleep)
			}
		})
	}
}

func TestSweepAsyncStartCleanupObligationsMissingRuntimeSettlesOnlyAfterVisibilityDeadline(t *testing.T) {
	store := beads.NewMemStore()
	_, raw := seedAsyncStartCleanupObligation(t, store)
	obligation, err := decodeAsyncStartCleanupObligation(raw)
	if err != nil {
		t.Fatal(err)
	}
	provider := &asyncCleanupProbeProvider{Provider: runtime.NewFake(), probeErr: runtime.ErrSessionNotFound}

	resolved, pending, err := sweepAsyncStartCleanupObligations(provider, store, obligation.NotBefore.Add(-time.Nanosecond), io.Discard)
	if err != nil || resolved != 0 || pending != 1 {
		t.Fatalf("pre-deadline sweep = (%d,%d,%v), want (0,1,nil)", resolved, pending, err)
	}
	resolved, pending, err = sweepAsyncStartCleanupObligations(provider, store, obligation.NotBefore, io.Discard)
	if err != nil || resolved != 1 || pending != 0 {
		t.Fatalf("post-deadline sweep = (%d,%d,%v), want (1,0,nil)", resolved, pending, err)
	}
}

func TestSweepAsyncStartCleanupObligationsMissingExpectedTokenFailsClosed(t *testing.T) {
	store := beads.NewMemStore()
	item, raw := seedAsyncStartCleanupObligation(t, store)
	malformed := `{"version":1,"mode":"cleanup","session_name":"worker","instance_token":"","not_before":"2099-01-01T00:00:00Z"}`
	replaced, err := sessionFrontDoor(store).ReplaceAsyncStartCleanupObligation(item.candidate.info.ID, raw, malformed)
	if err != nil || !replaced {
		t.Fatalf("replace marker = (%t,%v), want true,nil", replaced, err)
	}
	provider := &asyncCleanupProbeProvider{Provider: runtime.NewFake(), token: "tok-worker"}
	resolved, pending, err := sweepAsyncStartCleanupObligations(provider, store, time.Now(), io.Discard)
	if err != nil || resolved != 0 || pending != 1 {
		t.Fatalf("sweep = (%d,%d,%v), want (0,1,nil)", resolved, pending, err)
	}
	if provider.stopCalls.Load() != 0 {
		t.Fatalf("Stop calls = %d, want 0 without an expected token", provider.stopCalls.Load())
	}
}

func TestSweepAsyncStartCleanupObligationsRefusesNameStopWithoutAtomicFence(t *testing.T) {
	store := beads.NewMemStore()
	item, raw := seedAsyncStartCleanupObligation(t, store)
	if _, armed, err := armAsyncStartCleanupObligation(sessionFrontDoor(store), item.candidate.info.ID, raw); err != nil || !armed {
		t.Fatalf("arm obligation = (%t,%v), want true,nil", armed, err)
	}
	provider := &unsafeNameStopProbeProvider{Provider: runtime.NewFake()}
	resolved, pending, err := sweepAsyncStartCleanupObligations(provider, store, time.Now(), io.Discard)
	if err != nil || resolved != 0 || pending != 1 {
		t.Fatalf("sweep = (%d,%d,%v), want (0,1,nil)", resolved, pending, err)
	}
	if provider.stopCalls.Load() != 0 {
		t.Fatalf("unsafe name-based Stop calls = %d, want 0", provider.stopCalls.Load())
	}
}

// TestSweepAsyncStartCleanupObligationsBoundsProbesPerSweep guards the
// tick-level budget: a sweep must not issue more than
// defaultMaxAsyncStartCleanupObligationsPerTick provider probes, no matter how many
// journals are concurrently admitted, so N simultaneously-admitted journals
// under a merely-slow (not hard-down) API server cost a bounded, not
// cumulative, stall in a single controller tick. Journals left over must
// still count as pending (not silently dropped) and must resolve on a
// subsequent sweep, since callers such as the startup barrier loop retry
// until pending reaches zero.
func TestSweepAsyncStartCleanupObligationsBoundsProbesPerSweep(t *testing.T) {
	store := beads.NewMemStore()
	const extra = 3
	total := defaultMaxAsyncStartCleanupObligationsPerTick + extra
	for i := 0; i < total; i++ {
		id := fmt.Sprintf("gc-worker-%d", i)
		name := fmt.Sprintf("worker-%d", i)
		seedNamedAsyncStartCleanupObligation(t, store, id, name)
	}
	provider := &asyncCleanupProbeProvider{Provider: runtime.NewFake(), token: "tok-worker"}

	resolved, pending, err := sweepAsyncStartCleanupObligations(provider, store, time.Now(), io.Discard)
	if err != nil {
		t.Fatalf("first sweep: %v", err)
	}
	if got := provider.probeCalls.Load(); got != int32(defaultMaxAsyncStartCleanupObligationsPerTick) {
		t.Fatalf("first sweep probe calls = %d, want %d", got, defaultMaxAsyncStartCleanupObligationsPerTick)
	}
	if resolved != defaultMaxAsyncStartCleanupObligationsPerTick || pending != extra {
		t.Fatalf("first sweep = (resolved=%d pending=%d), want (%d,%d)", resolved, pending, defaultMaxAsyncStartCleanupObligationsPerTick, extra)
	}
	if resolved+pending != total {
		t.Fatalf("first sweep resolved+pending = %d, want %d (no journal silently dropped)", resolved+pending, total)
	}

	resolved, pending, err = sweepAsyncStartCleanupObligations(provider, store, time.Now(), io.Discard)
	if err != nil {
		t.Fatalf("second sweep: %v", err)
	}
	if got := provider.probeCalls.Load(); got != int32(total) {
		t.Fatalf("cumulative probe calls after second sweep = %d, want %d", got, total)
	}
	if resolved != extra || pending != 0 {
		t.Fatalf("second sweep = (resolved=%d pending=%d), want (%d,0)", resolved, pending, extra)
	}
}

func TestAsyncStartTrackerWaitsForBlockedProviderStopAndMarkerSettlement(t *testing.T) {
	store := beads.NewMemStore()
	item, raw := seedAsyncStartCleanupObligation(t, store)
	if _, armed, err := armAsyncStartCleanupObligation(sessionFrontDoor(store), item.candidate.info.ID, raw); err != nil || !armed {
		t.Fatalf("pre-shutdown arm = (%t,%v), want true,nil", armed, err)
	}
	provider := &asyncCleanupProbeProvider{
		Provider:    runtime.NewFake(),
		token:       "tok-worker",
		stopStarted: make(chan struct{}),
		stopRelease: make(chan struct{}),
	}
	var tracker asyncStartTracker
	finish, ok := tracker.startWithCompletionCleanup()
	if !ok {
		t.Fatal("tracker rejected work")
	}
	if tracker.waitUntilWithCompletionCleanup(time.Hour, func() bool { return true }) {
		t.Fatal("forced shutdown unexpectedly drained tracker")
	}
	go finish(
		func() bool { return completeAsyncStartObligation(item, raw, provider, store, io.Discard) },
		func() bool { return stopAsyncStartAfterShutdown(item, raw, provider, store, io.Discard) },
	)
	<-provider.stopStarted
	if tracker.wait(0) {
		t.Fatal("tracker completed while provider Stop was blocked")
	}
	close(provider.stopRelease)
	if !tracker.wait(time.Second) {
		t.Fatal("tracker did not complete after provider Stop and marker settlement")
	}
	info, err := sessionFrontDoor(store).Get(item.candidate.info.ID)
	if err != nil {
		t.Fatal(err)
	}
	if info.AsyncStartCleanupObligation != "" || info.MetadataState != string(sessionpkg.StateAsleep) {
		t.Fatalf("settled info = marker %q state %q, want empty/asleep", info.AsyncStartCleanupObligation, info.MetadataState)
	}
}

func TestNilAsyncStartTrackerStillRunsCompletionHandoff(t *testing.T) {
	var tracker *asyncStartTracker
	registered := false
	completed := false
	cleaned := false
	finish, tracking, err := tracker.startWithCompletionCleanupRegistered("gc-worker", func() (string, error) {
		registered = true
		return `{"version":1}`, nil
	})
	if err != nil || !tracking || finish == nil {
		t.Fatalf("nil tracker registration = finish:%t tracking:%t err:%v", finish != nil, tracking, err)
	}
	finish(func() bool {
		completed = true
		return true
	}, func() bool {
		cleaned = true
		return true
	})
	if !registered || !completed || cleaned {
		t.Fatalf("handoff = registered:%t completed:%t cleaned:%t, want true/true/false", registered, completed, cleaned)
	}
}

func TestAsyncStartAdmissionRegistrationLinearizesBeforeShutdownArm(t *testing.T) {
	store := beads.NewMemStore()
	item, _ := seedAsyncStartCleanupObligation(t, store)
	// Remove the seeding helper's marker so the registration callback below is
	// the sole admission journal owner.
	seeded, err := sessionFrontDoor(store).Get(item.candidate.info.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !clearAsyncStartCleanupObligation(sessionFrontDoor(store), seeded.ID, seeded.AsyncStartCleanupObligation, io.Discard) {
		t.Fatal("clear seeded marker")
	}

	var tracker asyncStartTracker
	registerEntered := make(chan struct{})
	releaseRegister := make(chan struct{})
	admitted := make(chan struct{})
	var finish func(func() bool, func() bool)
	var raw string
	var admitErr error
	go func() {
		finish, _, admitErr = tracker.startWithCompletionCleanupRegistered(item.candidate.info.ID, func() (string, error) {
			close(registerEntered)
			<-releaseRegister
			var err error
			raw, err = claimAsyncStartCleanupObligation(sessionFrontDoor(store), item, time.Now(), time.Minute)
			return raw, err
		})
		close(admitted)
	}()
	<-registerEntered
	admissionClosed := make(chan struct{})
	go func() {
		tracker.stopAdmission()
		close(admissionClosed)
	}()
	select {
	case <-admissionClosed:
		t.Fatal("shutdown closed admission before the in-lock journal registration completed")
	case <-time.After(20 * time.Millisecond):
	}
	close(releaseRegister)
	<-admitted
	<-admissionClosed
	if admitErr != nil || finish == nil {
		t.Fatalf("admission = finish:%t err:%v", finish != nil, admitErr)
	}
	if got := tracker.startJournalSnapshot()[item.candidate.info.ID]; got != raw || got == "" {
		t.Fatalf("tracked journal = %q, want %q", got, raw)
	}
	if failures := armAsyncStartCleanupObligations(context.Background(), runtime.NewFake(), store, tracker.startJournalSnapshot(), io.Discard); len(failures) != 0 {
		t.Fatalf("arm failures = %+v", failures)
	}
	info, err := sessionFrontDoor(store).Get(item.candidate.info.ID)
	if err != nil {
		t.Fatal(err)
	}
	obligation, err := decodeAsyncStartCleanupObligation(info.AsyncStartCleanupObligation)
	if err != nil || obligation.Mode != "cleanup" {
		t.Fatalf("armed obligation = mode:%q err:%v", obligation.Mode, err)
	}
	finish(nil, nil)
}

func TestCompletedAsyncStartJournalRetriesOnSteadyStateAfterTransientStoreFailures(t *testing.T) {
	store := &asyncCleanupCASStore{MemStore: beads.NewMemStore(), failClearCount: 4}
	item, raw := seedAsyncStartCleanupObligation(t, store)
	provider := &asyncCleanupProbeProvider{Provider: runtime.NewFake(), token: "tok-worker"}
	var tracker asyncStartTracker
	finish, tracking, err := tracker.startWithCompletionCleanupRegistered(item.candidate.info.ID, func() (string, error) { return raw, nil })
	if err != nil || !tracking {
		t.Fatalf("tracker registration = (%t,%v), want true,nil", tracking, err)
	}
	finish(func() bool { return completeAsyncStartObligation(item, raw, provider, store, io.Discard) }, nil)
	if tracker.ownsStartKey(item.candidate.info.ID) {
		t.Fatal("completed provider Start remained classified as in-flight")
	}
	if got := tracker.startJournalSnapshot()[item.candidate.info.ID]; got != raw {
		t.Fatalf("deferred tracker journal = %q, want %q", got, raw)
	}
	resolved, pending, err := sweepAsyncStartCleanupObligationsSkipping(
		provider, store, time.Now(), io.Discard,
		func(info sessionpkg.Info) bool { return tracker.ownsStartKey(info.ID) },
		func(info sessionpkg.Info, settledRaw string) { tracker.forgetStartJournal(info.ID, settledRaw) },
	)
	if err != nil || resolved != 1 || pending != 0 {
		t.Fatalf("steady-state sweep = (%d,%d,%v), want (1,0,nil)", resolved, pending, err)
	}
	if len(tracker.startJournalSnapshot()) != 0 {
		t.Fatalf("tracker retained settled journal: %+v", tracker.startJournalSnapshot())
	}
}

func TestCityRuntimeShutdownBoundsArmFailureAndPreservesUnarmedRuntime(t *testing.T) {
	store := &asyncCleanupCASStore{MemStore: beads.NewMemStore(), armErr: errors.New("injected durable arm failure for tok-worker")}
	item, raw := seedAsyncStartCleanupObligation(t, store)
	provider := runtime.NewFake()
	if err := provider.Start(context.Background(), "worker", runtime.Config{Command: "agent", Env: map[string]string{"GC_INSTANCE_TOKEN": "tok-worker"}}); err != nil {
		t.Fatal(err)
	}
	recorder := events.NewFake()
	var stderr strings.Builder
	cr := &CityRuntime{
		cfg:                 &config.City{Daemon: config.DaemonConfig{ShutdownTimeout: "30ms"}},
		sp:                  provider,
		rec:                 recorder,
		standaloneCityStore: store,
		logPrefix:           "gc test",
		stdout:              io.Discard,
		stderr:              &stderr,
	}
	finish, tracking, err := cr.asyncStarts.startWithCompletionCleanupRegistered(item.candidate.info.ID, func() (string, error) { return raw, nil })
	if err != nil || !tracking {
		t.Fatalf("tracker registration = (%t,%v), want true,nil", tracking, err)
	}
	store.mu.Lock()
	store.failArm = true
	store.mu.Unlock()
	started := time.Now()
	cr.shutdown()
	elapsed := time.Since(started)
	if elapsed < 25*time.Millisecond || elapsed > time.Second {
		t.Fatalf("bounded shutdown elapsed = %s, want roughly configured 30ms", elapsed)
	}
	if !provider.IsRunning("worker") {
		t.Fatal("unarmed runtime was stopped instead of fail-safe preservation")
	}
	for _, call := range provider.Calls {
		if call.Method == "Stop" || call.Method == "StopIfInstanceToken" {
			t.Fatalf("destructive provider call for unarmed runtime: %+v", call)
		}
	}
	info, err := sessionFrontDoor(store).Get(item.candidate.info.ID)
	if err != nil {
		t.Fatal(err)
	}
	obligation, err := decodeAsyncStartCleanupObligation(info.AsyncStartCleanupObligation)
	if err != nil || obligation.Mode != "admitted" {
		t.Fatalf("preserved obligation = mode:%q err:%v, want admitted", obligation.Mode, err)
	}
	eventsFound, err := recorder.List(events.Filter{Type: events.ShutdownCleanupIncomplete})
	if err != nil || len(eventsFound) != 1 {
		t.Fatalf("shutdown_cleanup_incomplete events = %d, err=%v", len(eventsFound), err)
	}
	logText := stderr.String()
	if !strings.Contains(logText, "shutdown_cleanup_incomplete") || !strings.Contains(logText, asyncStartTokenFingerprint("tok-worker")) {
		t.Fatalf("shutdown evidence missing from stderr: %q", logText)
	}
	if strings.Contains(logText, "tok-worker") || strings.Contains(string(eventsFound[0].Payload), "tok-worker") {
		t.Fatal("raw instance token leaked into shutdown evidence")
	}
	finish(nil, nil)
	successor := &asyncCleanupProbeProvider{Provider: runtime.NewFake(), token: "tok-worker"}
	resolved, pending, err := sweepAsyncStartCleanupObligations(successor, store, time.Now(), io.Discard)
	if err != nil || resolved != 1 || pending != 0 {
		t.Fatalf("successor adoption sweep = (%d,%d,%v), want (1,0,nil)", resolved, pending, err)
	}
	if successor.stopCalls.Load() != 0 || !provider.IsRunning("worker") {
		t.Fatal("successor treated preserved admitted work as destructive shutdown intent")
	}
}

func TestCityRuntimeShutdownDoesNotInvokeBlockingSynchronousJournalWriter(t *testing.T) {
	store := newSynchronousOnlyBlockingCASStore(t)
	item, raw := seedAsyncStartCleanupObligation(t, store)
	provider := runtime.NewFake()
	if err := provider.Start(context.Background(), "worker", runtime.Config{Command: "agent", Env: map[string]string{"GC_INSTANCE_TOKEN": "tok-worker"}}); err != nil {
		t.Fatal(err)
	}
	recorder := events.NewFake()
	cr := &CityRuntime{
		cfg:                 &config.City{Daemon: config.DaemonConfig{ShutdownTimeout: "25ms"}},
		sp:                  provider,
		rec:                 recorder,
		standaloneCityStore: store,
		logPrefix:           "gc test",
		stdout:              io.Discard,
		stderr:              io.Discard,
	}
	finish, tracking, err := cr.asyncStarts.startWithCompletionCleanupRegistered(item.candidate.info.ID, func() (string, error) { return raw, nil })
	if err != nil || !tracking {
		t.Fatalf("tracker registration = (%t,%v), want true,nil", tracking, err)
	}

	shutdownDone := make(chan struct{})
	go func() {
		cr.shutdown()
		close(shutdownDone)
	}()
	select {
	case <-shutdownDone:
	case <-store.armStarted:
		close(store.armRelease)
		<-shutdownDone
		t.Fatal("shutdown invoked the unbounded synchronous admitted-to-cleanup CAS")
	case <-time.After(250 * time.Millisecond):
		close(store.armRelease)
		t.Fatal("shutdown exceeded its bounded journal handoff budget")
	}
	if got := store.armCalls.Load(); got != 0 {
		t.Fatalf("blocking synchronous arm calls = %d, want 0", got)
	}
	if !provider.IsRunning("worker") {
		t.Fatal("runtime was stopped after the bounded writer capability was unavailable")
	}
	info, err := sessionFrontDoor(store).Get(item.candidate.info.ID)
	if err != nil {
		t.Fatal(err)
	}
	obligation, err := decodeAsyncStartCleanupObligation(info.AsyncStartCleanupObligation)
	if err != nil || obligation.Mode != "admitted" {
		t.Fatalf("preserved obligation = mode:%q err:%v, want admitted", obligation.Mode, err)
	}
	eventsFound, err := recorder.List(events.Filter{Type: events.ShutdownCleanupIncomplete})
	if err != nil || len(eventsFound) != 1 {
		t.Fatalf("shutdown_cleanup_incomplete events = %d, err=%v", len(eventsFound), err)
	}
	finish(nil, func() bool { return stopAsyncStartAfterShutdown(item, raw, provider, store, io.Discard) })
	if got := cr.asyncStarts.startJournalSnapshot()[item.candidate.info.ID]; got != raw {
		t.Fatalf("completion discarded preserved admitted journal = %q, want original", got)
	}
}

func TestCityRuntimeShutdownPreservesAdmittedStartWithoutAtomicFencedStop(t *testing.T) {
	store := beads.NewMemStore()
	item, raw := seedAsyncStartCleanupObligation(t, store)
	base := runtime.NewFake()
	if err := base.Start(context.Background(), "worker", runtime.Config{Command: "agent", Env: map[string]string{"GC_INSTANCE_TOKEN": "tok-worker"}}); err != nil {
		t.Fatal(err)
	}
	provider := &unsafeNameStopProbeProvider{Provider: base}
	recorder := events.NewFake()
	var stderr strings.Builder
	cr := &CityRuntime{
		cfg:                 &config.City{Daemon: config.DaemonConfig{ShutdownTimeout: "25ms"}},
		sp:                  provider,
		rec:                 recorder,
		standaloneCityStore: store,
		logPrefix:           "gc test",
		stdout:              io.Discard,
		stderr:              &stderr,
	}
	finish, tracking, err := cr.asyncStarts.startWithCompletionCleanupRegistered(item.candidate.info.ID, func() (string, error) { return raw, nil })
	if err != nil || !tracking {
		t.Fatalf("tracker registration = (%t,%v), want true,nil", tracking, err)
	}

	cr.shutdown()
	if got := provider.stopCalls.Load(); got != 0 {
		t.Fatalf("unsafe name-based Stop calls = %d, want 0", got)
	}
	if !base.IsRunning("worker") {
		t.Fatal("runtime was stopped without an atomic instance-token fence")
	}
	info, err := sessionFrontDoor(store).Get(item.candidate.info.ID)
	if err != nil {
		t.Fatal(err)
	}
	obligation, err := decodeAsyncStartCleanupObligation(info.AsyncStartCleanupObligation)
	if err != nil || obligation.Mode != "admitted" {
		t.Fatalf("preserved obligation = mode:%q err:%v, want admitted", obligation.Mode, err)
	}
	eventsFound, err := recorder.List(events.Filter{Type: events.ShutdownCleanupIncomplete})
	if err != nil || len(eventsFound) != 1 {
		t.Fatalf("shutdown_cleanup_incomplete events = %d, err=%v", len(eventsFound), err)
	}
	if !strings.Contains(stderr.String(), runtime.ErrFencedStopUnsupported.Error()) {
		t.Fatalf("shutdown evidence missing unsupported-fence reason: %q", stderr.String())
	}
	if strings.Contains(stderr.String(), "tok-worker") || strings.Contains(string(eventsFound[0].Payload), "tok-worker") {
		t.Fatal("raw instance token leaked into shutdown evidence")
	}

	finish(nil, func() bool { return stopAsyncStartAfterShutdown(item, raw, provider, store, io.Discard) })
	if got := cr.asyncStarts.startJournalSnapshot()[item.candidate.info.ID]; got != raw {
		t.Fatalf("completion discarded preserved admitted journal = %q, want original", got)
	}
	info, err = sessionFrontDoor(store).Get(item.candidate.info.ID)
	if err != nil {
		t.Fatal(err)
	}
	obligation, err = decodeAsyncStartCleanupObligation(info.AsyncStartCleanupObligation)
	if err != nil || obligation.Mode != "admitted" {
		t.Fatalf("post-completion obligation = mode:%q err:%v, want admitted", obligation.Mode, err)
	}

	successor := &asyncCleanupProbeProvider{Provider: runtime.NewFake(), token: "tok-worker"}
	resolved, pending, err := sweepAsyncStartCleanupObligations(successor, store, time.Now(), io.Discard)
	if err != nil || resolved != 1 || pending != 0 {
		t.Fatalf("successor adoption sweep = (%d,%d,%v), want (1,0,nil)", resolved, pending, err)
	}
	if successor.stopCalls.Load() != 0 || !base.IsRunning("worker") {
		t.Fatal("successor treated preserved admitted work as destructive shutdown intent")
	}
}

func TestAsyncStartCleanupObligationSurvivesControllerProcessBoundary(t *testing.T) {
	storePath := filepath.Join(t.TempDir(), "beads.json")
	store, err := beads.OpenFileStore(fsys.OSFS{}, storePath)
	if err != nil {
		t.Fatal(err)
	}
	created, err := store.Create(beads.Bead{
		ID:     "gc-worker",
		Title:  "worker",
		Type:   sessionpkg.BeadType,
		Status: "open",
		Labels: []string{sessionpkg.LabelSession},
		Metadata: map[string]string{
			"session_name":         "worker",
			"template":             "worker",
			"state":                string(sessionpkg.StateCreating),
			"instance_token":       "tok-worker",
			"pending_create_claim": "true",
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	cmd := exec.Command(os.Args[0], "-test.run=^TestAsyncStartCleanupObligationChildHelper$")
	cmd.Env = append(os.Environ(),
		"GC_TEST_ASYNC_CLEANUP_CHILD=1",
		"GC_TEST_ASYNC_CLEANUP_ARM=1",
		"GC_TEST_ASYNC_CLEANUP_STORE="+storePath,
		"GC_TEST_ASYNC_CLEANUP_SESSION_ID="+created.ID,
	)
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("cleanup-journal child: %v\n%s", err, output)
	}
	store, err = beads.OpenFileStore(fsys.OSFS{}, storePath)
	if err != nil {
		t.Fatal(err)
	}

	provider := &asyncCleanupProbeProvider{Provider: runtime.NewFake(), token: "tok-worker"}
	resolved, pending, err := sweepAsyncStartCleanupObligations(provider, store, time.Now(), io.Discard)
	if err != nil || resolved != 1 || pending != 0 {
		t.Fatalf("successor sweep = (%d,%d,%v), want (1,0,nil)", resolved, pending, err)
	}
	if provider.stopCalls.Load() != 1 {
		t.Fatalf("successor Stop calls = %d, want 1", provider.stopCalls.Load())
	}
	info, err := sessionFrontDoor(store).Get(created.ID)
	if err != nil {
		t.Fatal(err)
	}
	if info.AsyncStartCleanupObligation != "" || info.MetadataState != string(sessionpkg.StateAsleep) {
		t.Fatalf("successor state = marker %q state %q, want empty/asleep", info.AsyncStartCleanupObligation, info.MetadataState)
	}
}

func TestAsyncStartAdmissionJournalSurvivesOrdinaryControllerCrashWithoutCityStop(t *testing.T) {
	storePath := filepath.Join(t.TempDir(), "beads.json")
	store, err := beads.OpenFileStore(fsys.OSFS{}, storePath)
	if err != nil {
		t.Fatal(err)
	}
	created, err := store.Create(beads.Bead{
		Title:  "worker",
		Type:   sessionpkg.BeadType,
		Labels: []string{sessionpkg.LabelSession},
		Metadata: map[string]string{
			"session_name":         "worker",
			"template":             "worker",
			"state":                string(sessionpkg.StateCreating),
			"instance_token":       "tok-worker",
			"pending_create_claim": "true",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(os.Args[0], "-test.run=^TestAsyncStartCleanupObligationChildHelper$")
	cmd.Env = append(os.Environ(),
		"GC_TEST_ASYNC_CLEANUP_CHILD=1",
		"GC_TEST_ASYNC_CLEANUP_STORE="+storePath,
		"GC_TEST_ASYNC_CLEANUP_SESSION_ID="+created.ID,
	)
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("admission-journal child: %v\n%s", err, output)
	}
	store, err = beads.OpenFileStore(fsys.OSFS{}, storePath)
	if err != nil {
		t.Fatal(err)
	}
	provider := &asyncCleanupProbeProvider{Provider: runtime.NewFake(), token: "tok-worker"}
	resolved, pending, err := sweepAsyncStartCleanupObligations(provider, store, time.Now(), io.Discard)
	if err != nil || resolved != 1 || pending != 0 {
		t.Fatalf("successor sweep = (%d,%d,%v), want (1,0,nil)", resolved, pending, err)
	}
	if provider.stopCalls.Load() != 0 {
		t.Fatalf("ordinary-crash successor Stop calls = %d, want 0", provider.stopCalls.Load())
	}
	info, err := sessionFrontDoor(store).Get(created.ID)
	if err != nil {
		t.Fatal(err)
	}
	if info.AsyncStartCleanupObligation != "" || info.MetadataState == string(sessionpkg.StateAsleep) || info.SleepReason == string(sessionpkg.SleepReasonCityStop) {
		t.Fatalf("ordinary-crash state = marker %q state %q sleep_reason %q; want admitted journal cleared without city-stop", info.AsyncStartCleanupObligation, info.MetadataState, info.SleepReason)
	}
}

func TestAsyncStartCleanupObligationChildHelper(t *testing.T) {
	if os.Getenv("GC_TEST_ASYNC_CLEANUP_CHILD") != "1" {
		return
	}
	storePath := strings.TrimSpace(os.Getenv("GC_TEST_ASYNC_CLEANUP_STORE"))
	sessionID := strings.TrimSpace(os.Getenv("GC_TEST_ASYNC_CLEANUP_SESSION_ID"))
	store, err := beads.OpenFileStore(fsys.OSFS{}, storePath)
	if err != nil {
		t.Fatal(err)
	}
	info, err := sessionFrontDoor(store).Get(sessionID)
	if err != nil {
		t.Fatal(err)
	}
	item := preparedStart{candidate: startCandidate{
		info: info,
		tp:   TemplateParams{SessionName: "worker", TemplateName: "worker", Command: "agent"},
	}}
	raw, err := claimAsyncStartCleanupObligation(sessionFrontDoor(store), item, time.Now(), time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if os.Getenv("GC_TEST_ASYNC_CLEANUP_ARM") == "1" {
		if _, armed, err := armAsyncStartCleanupObligation(sessionFrontDoor(store), sessionID, raw); err != nil || !armed {
			t.Fatalf("arm obligation = (%t,%v), want true,nil", armed, err)
		}
	}
}

var _ runtime.Provider = (*asyncCleanupProbeProvider)(nil)
