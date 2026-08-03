package main

import (
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	sessionpkg "github.com/gastownhall/gascity/internal/session"
)

// TestRecordCreateFailureReason covers the diagnostic added after 2026-08-01,
// when 2,209 session creates failed and every one of them persisted the same
// canonical close reason — "session create failed: aborted before
// creation_complete" — with no indication of cause. Three separate hypotheses
// (auth expiry, slot contention, an environmental fault) were argued from
// timing alone before the real cause was found, because the record itself said
// nothing. The reason has to survive onto the bead.
func TestRecordCreateFailureReason(t *testing.T) {
	t.Run("persists the error onto the session bead", func(t *testing.T) {
		env := newReconcilerTestEnv()
		bead := env.createSessionBead("s-gc-failreason", "worker")
		store := sessionpkg.NewStore(beads.SessionStore{Store: env.store})

		recordCreateFailureReason(
			sessionpkg.Info{ID: bead.ID},
			store,
			errors.New("provider refused: no slot available"),
			io.Discard,
		)

		got, err := env.store.Get(bead.ID)
		if err != nil {
			t.Fatalf("Get: %v", err)
		}
		reason := got.Metadata[beadmeta.CreateFailureReasonMetadataKey]
		if !strings.Contains(reason, "no slot available") {
			t.Fatalf("create_failure_reason = %q, want it to carry the underlying error", reason)
		}
	})

	t.Run("truncates a pathological error", func(t *testing.T) {
		env := newReconcilerTestEnv()
		bead := env.createSessionBead("s-gc-failreason-long", "worker")
		store := sessionpkg.NewStore(beads.SessionStore{Store: env.store})

		recordCreateFailureReason(
			sessionpkg.Info{ID: bead.ID},
			store,
			errors.New(strings.Repeat("x", 5000)),
			io.Discard,
		)

		got, err := env.store.Get(bead.ID)
		if err != nil {
			t.Fatalf("Get: %v", err)
		}
		if n := len(got.Metadata[beadmeta.CreateFailureReasonMetadataKey]); n > createFailureReasonMaxLen {
			t.Fatalf("create_failure_reason length = %d, want <= %d", n, createFailureReasonMaxLen)
		}
	})

	// A nil error, an empty id, or a nil store must be no-ops rather than
	// panics: commitStartFailure's rollback arm runs on the async start
	// goroutine, where a panic takes the reconciler down mid-tick.
	t.Run("degenerate inputs are no-ops", func(t *testing.T) {
		env := newReconcilerTestEnv()
		bead := env.createSessionBead("s-gc-failreason-nil", "worker")
		store := sessionpkg.NewStore(beads.SessionStore{Store: env.store})

		recordCreateFailureReason(sessionpkg.Info{ID: bead.ID}, store, nil, io.Discard)
		recordCreateFailureReason(sessionpkg.Info{ID: ""}, store, errors.New("x"), io.Discard)
		recordCreateFailureReason(sessionpkg.Info{ID: bead.ID}, nil, errors.New("x"), io.Discard)

		got, err := env.store.Get(bead.ID)
		if err != nil {
			t.Fatalf("Get: %v", err)
		}
		if reason := got.Metadata[beadmeta.CreateFailureReasonMetadataKey]; reason != "" {
			t.Fatalf("create_failure_reason = %q, want no write for degenerate inputs", reason)
		}
	})
}
