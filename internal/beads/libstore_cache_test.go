package beads

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type beadsLibTestScope struct {
	root     string
	beadsDir string
	env      []string
	prefix   string
}

func requireBeadsLibToolchain(t testing.TB) {
	t.Helper()
	if os.Getenv("GC_FAST_UNIT") == "1" {
		t.Skip("skipping real bd/dolt BeadsLibStore test in fast unit mode")
	}
	if testing.Short() {
		t.Skip("skipping real bd/dolt BeadsLibStore test in short mode")
	}
	if _, err := exec.LookPath("bd"); err != nil {
		t.Skipf("bd not found: %v", err)
	}
	if _, err := exec.LookPath("dolt"); err != nil {
		t.Skipf("dolt not found: %v", err)
	}
}

func newBeadsLibTestScope(t testing.TB, prefix string) beadsLibTestScope {
	t.Helper()
	requireBeadsLibToolchain(t)

	root := t.TempDir()
	beadsDir := filepath.Join(root, ".beads")
	env := isolatedBeadsLibEnv(beadsDir)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	cmd := exec.CommandContext(ctx, "bd", "init", "--server", "-p", prefix, "--skip-hooks")
	cmd.Dir = root
	cmd.Env = env
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("bd init --server: %v\n%s", err, out)
	}

	t.Cleanup(func() {
		stopCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		stop := exec.CommandContext(stopCtx, "bd", "dolt", "stop")
		stop.Dir = root
		stop.Env = env
		_ = stop.Run()
	})

	return beadsLibTestScope{
		root:     root,
		beadsDir: beadsDir,
		env:      env,
		prefix:   prefix,
	}
}

func isolatedBeadsLibEnv(beadsDir string) []string {
	env := make([]string, 0, len(os.Environ())+2)
	for _, kv := range os.Environ() {
		switch {
		case strings.HasPrefix(kv, "BEADS_DIR="):
			continue
		case strings.HasPrefix(kv, "BEADS_DOLT_AUTO_START="):
			continue
		case strings.HasPrefix(kv, "BEADS_DOLT_SERVER_"):
			continue
		case strings.HasPrefix(kv, "GC_DOLT_"):
			continue
		}
		env = append(env, kv)
	}
	env = append(env,
		"BEADS_DIR="+beadsDir,
		"BEADS_DOLT_AUTO_START=1",
	)
	return env
}

func openBeadsLibStoreForTest(t testing.TB, scope beadsLibTestScope) *BeadsLibStore {
	t.Helper()
	store, err := NewBeadsLibStore(scope.root, scope.prefix)
	if err != nil {
		t.Fatalf("open BeadsLibStore: %v", err)
	}
	t.Cleanup(func() {
		if err := store.Shutdown(); err != nil {
			t.Errorf("shutdown BeadsLibStore: %v", err)
		}
	})
	return store
}

func TestBeadsLibStoreForeignWritesReachCachedPeerViaReconcile(t *testing.T) {
	scope := newBeadsLibTestScope(t, "lib")
	writer := openBeadsLibStoreForTest(t, scope)
	reader := openBeadsLibStoreForTest(t, scope)

	var observed []string
	cache := NewCachingStore(reader, func(eventType, beadID string, _ json.RawMessage) {
		observed = append(observed, eventType+":"+beadID)
	})
	if err := cache.Prime(context.Background()); err != nil {
		t.Fatalf("prime cache: %v", err)
	}
	if cached, ok := cache.CachedList(ListQuery{Status: "open"}); !ok || len(cached) != 0 {
		t.Fatalf("initial cached open beads = %+v, ok=%v; want empty live cache", cached, ok)
	}

	created, err := writer.Create(Bead{
		Title:    "foreign lib write",
		Metadata: map[string]string{"writer": "one"},
	})
	if err != nil {
		t.Fatalf("foreign Create: %v", err)
	}
	cache.runReconciliation()

	cached, ok := cache.CachedList(ListQuery{Status: "open"})
	if !ok {
		t.Fatal("cache did not report an initialized read model after foreign create")
	}
	if len(cached) != 1 || cached[0].ID != created.ID {
		t.Fatalf("cached open beads after foreign create = %+v, want %s", cached, created.ID)
	}
	assertObservedCacheEvent(t, observed, "bead.created", created.ID)

	updatedTitle := "foreign lib update"
	if err := writer.Update(created.ID, UpdateOpts{
		Title:    &updatedTitle,
		Metadata: map[string]string{"writer": "two"},
	}); err != nil {
		t.Fatalf("foreign Update: %v", err)
	}
	cache.runReconciliation()

	got, err := cache.Get(created.ID)
	if err != nil {
		t.Fatalf("cache Get after foreign update: %v", err)
	}
	if got.Title != updatedTitle {
		t.Fatalf("cached title after foreign update = %q, want %q", got.Title, updatedTitle)
	}
	if got.Metadata["writer"] != "two" {
		t.Fatalf("cached metadata writer = %q, want two; bead=%+v", got.Metadata["writer"], got)
	}
	assertObservedCacheEvent(t, observed, "bead.updated", created.ID)

	if err := writer.Close(created.ID); err != nil {
		t.Fatalf("foreign Close: %v", err)
	}
	cache.runReconciliation()

	cached, ok = cache.CachedList(ListQuery{Status: "open"})
	if !ok {
		t.Fatal("cache did not report an initialized read model after foreign close")
	}
	if len(cached) != 0 {
		t.Fatalf("cached open beads after foreign close = %+v, want empty", cached)
	}
	assertObservedCacheEvent(t, observed, "bead.closed", created.ID)
}

func assertObservedCacheEvent(t *testing.T, events []string, eventType, beadID string) {
	t.Helper()
	want := eventType + ":" + beadID
	for _, got := range events {
		if got == want {
			return
		}
	}
	t.Fatalf("observed events = %v, want %s", events, want)
}
