package session

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/gastownhall/gascity/internal/runtime"
)

func TestCapsuleStateReconcilerRetainsContinuityAndPurgesOnlyExplicitTerminalState(t *testing.T) {
	t.Parallel()
	provider := runtime.NewFake()
	live := ensureCapsuleTestState(t, provider, "city", "live")
	closed := ensureCapsuleTestState(t, provider, "city", "closed")
	orphan := ensureCapsuleTestState(t, provider, "city", "orphan")
	purge := ensureCapsuleTestState(t, provider, "city", "purge")
	foreign := ensureCapsuleTestState(t, provider, "foreign-city", "foreign")
	ledger := newCapsuleStateLedger([]CapsuleStateSessionFact{
		{Key: live.Key, Live: true},
		{Key: closed.Key, Terminal: true},
		{Key: purge.Key, Terminal: true, PurgeAuthorized: true},
	})
	if err := provider.Start(context.Background(), "orphan-place", runtime.Config{}); err != nil {
		t.Fatal(err)
	}
	if err := provider.AttachCapsuleState(context.Background(), "orphan-place", orphan); err != nil {
		t.Fatal(err)
	}
	reconciler := ledger.reconciler(provider)
	report, err := reconciler.Reconcile(context.Background(), "city", false)
	if err != nil {
		t.Fatal(err)
	}
	actions := capsuleStateActions(report)
	for sessionID, want := range map[string]CapsuleStateReconcileAction{
		"live": CapsuleStateRetained, "closed": CapsuleStateRetained,
		"orphan": CapsuleStateRetainedOrphan, "purge": CapsuleStatePurged,
	} {
		if actions[sessionID] != want {
			t.Fatalf("action[%s] = %q, want %q; report=%#v", sessionID, actions[sessionID], want, report)
		}
	}
	if report.IgnoredForeign != 1 {
		t.Fatalf("IgnoredForeign = %d, want 1", report.IgnoredForeign)
	}
	for _, ref := range []runtime.CapsuleStateReference{live, closed, orphan, foreign} {
		if _, ok, err := provider.OpenCapsuleState(context.Background(), ref.Key); err != nil || !ok {
			t.Fatalf("retained state %q ok=%v err=%v", ref.Key.SessionID, ok, err)
		}
	}
	if _, ok, err := provider.OpenCapsuleState(context.Background(), purge.Key); err != nil || ok {
		t.Fatalf("purged state ok=%v err=%v", ok, err)
	}
	if !ledger.isPurged(purge.Key) {
		t.Fatal("purge completion was not recorded")
	}
}

func TestCapsuleStateReconcilerDryRunStaleIntentAndProviderFailure(t *testing.T) {
	t.Parallel()
	t.Run("dry run", func(t *testing.T) {
		provider := runtime.NewFake()
		ref := ensureCapsuleTestState(t, provider, "city", "dry")
		ledger := newCapsuleStateLedger([]CapsuleStateSessionFact{{Key: ref.Key, Terminal: true, PurgeAuthorized: true}})
		report, err := ledger.reconciler(provider).Reconcile(context.Background(), "city", true)
		if err != nil || capsuleStateActions(report)["dry"] != CapsuleStateWouldPurge {
			t.Fatalf("dry-run report=%#v err=%v", report, err)
		}
		if _, ok, err := provider.OpenCapsuleState(context.Background(), ref.Key); err != nil || !ok || ledger.isPurged(ref.Key) {
			t.Fatalf("dry run mutated provider/ledger: ok=%v recorded=%v err=%v", ok, ledger.isPurged(ref.Key), err)
		}
	})
	t.Run("stale intent", func(t *testing.T) {
		provider := runtime.NewFake()
		ref := ensureCapsuleTestState(t, provider, "city", "raced")
		ledger := newCapsuleStateLedger([]CapsuleStateSessionFact{{Key: ref.Key, Terminal: true, PurgeAuthorized: true}})
		ledger.lookupOverride = func(fact CapsuleStateSessionFact) CapsuleStateSessionFact {
			fact.Live = true
			return fact
		}
		report, err := ledger.reconciler(provider).Reconcile(context.Background(), "city", false)
		if err != nil || capsuleStateActions(report)["raced"] != CapsuleStateRetained {
			t.Fatalf("stale-intent report=%#v err=%v", report, err)
		}
		if _, ok, _ := provider.OpenCapsuleState(context.Background(), ref.Key); !ok {
			t.Fatal("stale intent deleted state")
		}
	})
	t.Run("provider unavailable", func(t *testing.T) {
		provider := runtime.NewFake()
		provider.CapsuleListError = runtime.ErrRuntimeUnavailable
		ledger := newCapsuleStateLedger(nil)
		if _, err := ledger.reconciler(provider).Reconcile(context.Background(), "city", false); !errors.Is(err, runtime.ErrRuntimeUnavailable) {
			t.Fatalf("provider error = %v", err)
		}
	})
}

func TestCapsuleStateReconcilerConcurrentPassesAreIdempotent(t *testing.T) {
	t.Parallel()
	provider := runtime.NewFake()
	ref := ensureCapsuleTestState(t, provider, "city", "concurrent")
	ledger := newCapsuleStateLedger([]CapsuleStateSessionFact{{Key: ref.Key, Terminal: true, PurgeAuthorized: true}})
	reconciler := ledger.reconciler(provider)
	const passes = 16
	errs := make(chan error, passes)
	var wg sync.WaitGroup
	for range passes {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := reconciler.Reconcile(context.Background(), "city", false)
			errs <- err
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	if _, ok, err := provider.OpenCapsuleState(context.Background(), ref.Key); err != nil || ok || !ledger.isPurged(ref.Key) {
		t.Fatalf("concurrent result ok=%v recorded=%v err=%v", ok, ledger.isPurged(ref.Key), err)
	}
}

func TestCapsuleStateReconcilerHonorsRecordedCompletionAndConflictsOnReappearance(t *testing.T) {
	t.Parallel()

	provider := runtime.NewFake()
	ref := ensureCapsuleTestState(t, provider, "city", "completed")
	if err := provider.PurgeCapsuleState(context.Background(), ref.Key); err != nil {
		t.Fatal(err)
	}
	ledger := newCapsuleStateLedger([]CapsuleStateSessionFact{{
		Key: ref.Key, Terminal: true, PurgeAuthorized: true, PurgeCompleted: true,
	}})

	report, err := ledger.reconciler(provider).Reconcile(context.Background(), "city", true)
	if err != nil {
		t.Fatal(err)
	}
	if got := capsuleStateActions(report)[ref.Key.SessionID]; got != CapsuleStatePurgeRecorded {
		t.Fatalf("completed absent action = %q, want %q", got, CapsuleStatePurgeRecorded)
	}

	if _, _, err := provider.EnsureCapsuleState(context.Background(), ref.Key); err != nil {
		t.Fatal(err)
	}
	report, err = ledger.reconciler(provider).Reconcile(context.Background(), "city", false)
	if !errors.Is(err, runtime.ErrCapsuleStateConflict) {
		t.Fatalf("reappeared state error = %v, want ErrCapsuleStateConflict", err)
	}
	if got := capsuleStateActions(report)[ref.Key.SessionID]; got != CapsuleStateConflict {
		t.Fatalf("reappeared action = %q, want %q", got, CapsuleStateConflict)
	}
	if _, ok, err := provider.OpenCapsuleState(context.Background(), ref.Key); err != nil || !ok {
		t.Fatalf("reappeared state was deleted: exists=%t err=%v", ok, err)
	}
}

func TestCapsuleStateReconcilerRetriesFailureAndRejectsStaleAllocation(t *testing.T) {
	t.Parallel()
	t.Run("provider unavailable", func(t *testing.T) {
		provider := runtime.NewFake()
		ref := ensureCapsuleTestState(t, provider, "city", "retry")
		ledger := newCapsuleStateLedger([]CapsuleStateSessionFact{{Key: ref.Key, Terminal: true, PurgeAuthorized: true}})
		provider.CapsuleStateErrors[ref.Key.Digest] = errors.New("provider temporarily unavailable")
		if _, err := ledger.reconciler(provider).Reconcile(context.Background(), "city", false); err == nil {
			t.Fatal("provider failure did not surface")
		}
		delete(provider.CapsuleStateErrors, ref.Key.Digest)
		if _, err := ledger.reconciler(provider).Reconcile(context.Background(), "city", false); err != nil {
			t.Fatalf("retry: %v", err)
		}
		if !ledger.isPurged(ref.Key) {
			t.Fatal("retry did not record purge")
		}
	})
	t.Run("replacement still attached", func(t *testing.T) {
		provider := runtime.NewFake()
		ref := ensureCapsuleTestState(t, provider, "city", "attached")
		ledger := newCapsuleStateLedger([]CapsuleStateSessionFact{{Key: ref.Key, Terminal: true, PurgeAuthorized: true}})
		if err := provider.Start(context.Background(), "replacement", runtime.Config{}); err != nil {
			t.Fatal(err)
		}
		if err := provider.AttachCapsuleState(context.Background(), "replacement", ref); err != nil {
			t.Fatal(err)
		}
		if _, err := ledger.reconciler(provider).Reconcile(context.Background(), "city", false); !errors.Is(err, runtime.ErrCapsuleStateConflict) {
			t.Fatalf("attached purge error = %v", err)
		}
		if err := provider.DetachCapsuleState(context.Background(), "replacement"); err != nil {
			t.Fatal(err)
		}
		if _, err := ledger.reconciler(provider).Reconcile(context.Background(), "city", false); err != nil {
			t.Fatalf("retry after detach: %v", err)
		}
	})
}

func TestCapsuleStateReconcilerPreservesDeletionRacesAndAmbiguousInventory(t *testing.T) {
	t.Parallel()
	t.Run("session deleted after inventory", func(t *testing.T) {
		provider := runtime.NewFake()
		ref := ensureCapsuleTestState(t, provider, "city", "deleted-race")
		ledger := newCapsuleStateLedger([]CapsuleStateSessionFact{{Key: ref.Key, Terminal: true, PurgeAuthorized: true}})
		reconciler := ledger.reconciler(provider)
		reconciler.Lookup = func(context.Context, runtime.CapsuleKey) (CapsuleStateSessionFact, bool, error) {
			return CapsuleStateSessionFact{}, false, nil
		}
		report, err := reconciler.Reconcile(context.Background(), "city", false)
		if err != nil || capsuleStateActions(report)["deleted-race"] != CapsuleStateRetainedOrphan {
			t.Fatalf("deletion-race report=%#v err=%v", report, err)
		}
		if _, ok, _ := provider.OpenCapsuleState(context.Background(), ref.Key); !ok {
			t.Fatal("session deletion race purged state")
		}
	})
	t.Run("stale provider observation", func(t *testing.T) {
		provider := runtime.NewFake()
		ref := ensureCapsuleTestState(t, provider, "city", "stale")
		ledger := newCapsuleStateLedger([]CapsuleStateSessionFact{{Key: ref.Key, Terminal: true, PurgeAuthorized: true}})
		stale := ref
		stale.ResourceID += "-old"
		override := capsuleInventoryOverride{CapsuleStateRuntime: provider, refs: []runtime.CapsuleStateReference{stale}}
		report, err := ledger.reconciler(override).Reconcile(context.Background(), "city", false)
		if !errors.Is(err, runtime.ErrCapsuleStateConflict) || capsuleStateActions(report)["stale"] != CapsuleStateConflict {
			t.Fatalf("stale report=%#v err=%v", report, err)
		}
		if _, ok, _ := provider.OpenCapsuleState(context.Background(), ref.Key); !ok {
			t.Fatal("stale provider observation purged state")
		}
	})
	t.Run("duplicate provider claims", func(t *testing.T) {
		provider := runtime.NewFake()
		ref := ensureCapsuleTestState(t, provider, "city", "ambiguous")
		ledger := newCapsuleStateLedger([]CapsuleStateSessionFact{{Key: ref.Key, Terminal: true, PurgeAuthorized: true}})
		override := capsuleInventoryOverride{CapsuleStateRuntime: provider, refs: []runtime.CapsuleStateReference{ref, ref}}
		report, err := ledger.reconciler(override).Reconcile(context.Background(), "city", false)
		if err != nil || capsuleStateActions(report)["ambiguous"] != CapsuleStateConflict {
			t.Fatalf("ambiguous report=%#v err=%v", report, err)
		}
		if _, ok, _ := provider.OpenCapsuleState(context.Background(), ref.Key); !ok {
			t.Fatal("ambiguous provider claims purged state")
		}
	})
}

type capsuleInventoryOverride struct {
	runtime.CapsuleStateRuntime
	refs []runtime.CapsuleStateReference
}

func (o capsuleInventoryOverride) ListCapsuleStates(context.Context) ([]runtime.CapsuleStateReference, error) {
	return append([]runtime.CapsuleStateReference(nil), o.refs...), nil
}

type capsuleStateLedger struct {
	mu             sync.Mutex
	facts          map[string]CapsuleStateSessionFact
	lookupOverride func(CapsuleStateSessionFact) CapsuleStateSessionFact
}

func newCapsuleStateLedger(facts []CapsuleStateSessionFact) *capsuleStateLedger {
	ledger := &capsuleStateLedger{facts: make(map[string]CapsuleStateSessionFact)}
	for _, fact := range facts {
		ledger.facts[fact.Key.Digest] = fact
	}
	return ledger
}

func (l *capsuleStateLedger) reconciler(provider runtime.CapsuleStateRuntime) CapsuleStateReconciler {
	return CapsuleStateReconciler{
		Runtime: provider,
		ListSessions: func(_ context.Context, scope string) ([]CapsuleStateSessionFact, error) {
			l.mu.Lock()
			defer l.mu.Unlock()
			var facts []CapsuleStateSessionFact
			for _, fact := range l.facts {
				if fact.Key.CityScope == scope {
					facts = append(facts, fact)
				}
			}
			return facts, nil
		},
		Lookup: func(_ context.Context, key runtime.CapsuleKey) (CapsuleStateSessionFact, bool, error) {
			l.mu.Lock()
			defer l.mu.Unlock()
			fact, ok := l.facts[key.Digest]
			if ok && l.lookupOverride != nil {
				fact = l.lookupOverride(fact)
			}
			return fact, ok, nil
		},
		MarkPurged: func(_ context.Context, key runtime.CapsuleKey) error {
			l.mu.Lock()
			defer l.mu.Unlock()
			fact := l.facts[key.Digest]
			fact.PurgeCompleted = true
			l.facts[key.Digest] = fact
			return nil
		},
	}
}

func (l *capsuleStateLedger) isPurged(key runtime.CapsuleKey) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.facts[key.Digest].PurgeCompleted
}

func ensureCapsuleTestState(t *testing.T, provider *runtime.Fake, scope, sessionID string) runtime.CapsuleStateReference {
	t.Helper()
	key, err := runtime.NewCapsuleKey(scope, sessionID)
	if err != nil {
		t.Fatal(err)
	}
	ref, _, err := provider.EnsureCapsuleState(context.Background(), key)
	if err != nil {
		t.Fatal(err)
	}
	return ref
}

func capsuleStateActions(report CapsuleStateReconcileReport) map[string]CapsuleStateReconcileAction {
	actions := make(map[string]CapsuleStateReconcileAction, len(report.Items))
	for _, item := range report.Items {
		actions[item.SessionID] = item.Action
	}
	return actions
}
