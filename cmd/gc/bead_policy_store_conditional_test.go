package main

import (
	"context"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/rollout/gate"
)

// TestBeadPolicyStoreResolvesConditionalWritesThroughWrapper pins the stage-3
// wiring hazard: every factory store is policy-wrapped, and interface
// embedding hides the factory's conditional-writes stamp — without the
// wrapper's declared resolution target, a require deployment would silently
// resolve unset→legacy through the wrapper on every consumer.
func TestBeadPolicyStoreResolvesConditionalWritesThroughWrapper(t *testing.T) {
	result, err := beads.OpenStoreAtForCity(context.Background(), beads.StoreOpenOptions{
		ScopeRoot:         t.TempDir(),
		Provider:          "file",
		ConditionalWrites: gate.Require,
		OpenFileStore:     func() (beads.Store, error) { return beads.NewMemStore(), nil },
	})
	if err != nil {
		t.Fatalf("OpenStoreAtForCity: %v", err)
	}

	wrapped := wrapStoreWithBeadPolicies(result.Store, nil)
	if _, _, ok := unwrapBeadPolicyStore(wrapped); !ok {
		t.Fatalf("test premise: store %T is not policy-wrapped", wrapped)
	}

	writer, diag, resolveErr := beads.ResolveConditionalWriter(wrapped)
	if resolveErr != nil || diag != nil {
		t.Fatalf("resolve through policy wrapper = diag %v err %v, want the stamped store's writer", diag, resolveErr)
	}
	if writer == nil {
		t.Fatal("resolve through policy wrapper returned no writer: the require stamp was hidden by interface embedding")
	}
}

func TestBeadPolicyStoreConditionalWriterForPreservesCacheBoundary(t *testing.T) {
	backing := beads.NewMemStore()
	cache := beads.NewCachingStoreForTest(backing, nil)
	if err := cache.Prime(context.Background()); err != nil {
		t.Fatalf("Prime: %v", err)
	}
	wrapped := wrapStoreWithBeadPolicies(cache, nil)

	created, err := wrapped.Create(beads.Bead{Title: "async journal owner"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	writer, ok := beads.ConditionalWriterFor(wrapped)
	if !ok {
		t.Fatal("ConditionalWriterFor(policy(cache(backing))) = unavailable")
	}
	if got, ok := writer.(*beads.CachingStore); !ok || got != cache {
		t.Fatalf("ConditionalWriterFor(policy(cache(backing))) = %T, want cache writer", writer)
	}
	swapped, err := writer.CompareAndSetMetadataKey(created.ID, "async_journal", "", "admitted")
	if err != nil || !swapped {
		t.Fatalf("CompareAndSetMetadataKey through policy/cache = (%t,%v), want true,nil", swapped, err)
	}
	persisted, err := backing.Get(created.ID)
	if err != nil {
		t.Fatalf("backing Get: %v", err)
	}
	if got := persisted.Metadata["async_journal"]; got != "admitted" {
		t.Fatalf("persisted async_journal = %q, want admitted", got)
	}
	contextWriter, ok := beads.ContextMetadataCASWriterFor(wrapped)
	if !ok {
		t.Fatal("ContextMetadataCASWriterFor(policy(cache(backing))) = unavailable")
	}
	if got, ok := contextWriter.(*beads.CachingStore); !ok || got != cache {
		t.Fatalf("ContextMetadataCASWriterFor(policy(cache(backing))) = %T, want cache writer", contextWriter)
	}
	swapped, err = contextWriter.CompareAndSetMetadataKeyContext(context.Background(), created.ID, "async_journal", "admitted", "cleanup")
	if err != nil || !swapped {
		t.Fatalf("CompareAndSetMetadataKeyContext through policy/cache = (%t,%v), want true,nil", swapped, err)
	}
	persisted, err = backing.Get(created.ID)
	if err != nil {
		t.Fatalf("backing Get after context CAS: %v", err)
	}
	if got := persisted.Metadata["async_journal"]; got != "cleanup" {
		t.Fatalf("persisted async_journal after context CAS = %q, want cleanup", got)
	}
}
