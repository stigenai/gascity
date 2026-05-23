package main

import (
	"context"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/emergency"
	"github.com/gastownhall/gascity/internal/events"
)

func TestStartEmergencyEventRelayRecordsSignaled(t *testing.T) {
	ep := events.NewFake()
	emergencyCh := make(chan emergency.Record, 1)
	cs := &controllerState{emergencyCh: emergencyCh, eventProv: ep}

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	cs.startEmergencyEventRelay(ctx)

	rec := emergency.Record{
		ID:       "20260101T000000Z-aabbccdd",
		Severity: emergency.SeverityError,
		Actor:    "test-agent",
		Message:  "relay smoke test",
	}
	emergencyCh <- rec

	deadline := time.Now().Add(2 * time.Second)
	for {
		evts, err := ep.List(events.Filter{Type: events.EmergencySignaled})
		if err != nil {
			t.Fatalf("ep.List: %v", err)
		}
		if len(evts) >= 1 {
			got := evts[0]
			if got.Type != events.EmergencySignaled {
				t.Errorf("event.Type = %q, want %q", got.Type, events.EmergencySignaled)
			}
			if got.Actor != rec.Actor {
				t.Errorf("event.Actor = %q, want %q", got.Actor, rec.Actor)
			}
			if got.Message != rec.Message {
				t.Errorf("event.Message = %q, want %q", got.Message, rec.Message)
			}
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("no emergency.signaled event recorded within 2s")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestStartEmergencyEventRelayCancelExits(t *testing.T) {
	ep := events.NewFake()
	emergencyCh := make(chan emergency.Record, 2)
	cs := &controllerState{emergencyCh: emergencyCh, eventProv: ep}

	ctx, cancel := context.WithCancel(context.Background())
	cs.startEmergencyEventRelay(ctx)

	// Allow goroutine to enter its select loop.
	time.Sleep(20 * time.Millisecond)

	cancel()

	// Allow goroutine to observe ctx.Done() and exit.
	time.Sleep(100 * time.Millisecond)

	// Send a record after cancel — should buffer but not be relayed.
	emergencyCh <- emergency.Record{
		ID:       "20260101T000001Z-aabbccdd",
		Severity: emergency.SeverityInfo,
		Actor:    "test-cancel",
		Message:  "should not relay after cancel",
	}

	time.Sleep(50 * time.Millisecond)

	evts, err := ep.List(events.Filter{Type: events.EmergencySignaled})
	if err != nil {
		t.Fatalf("ep.List: %v", err)
	}
	if len(evts) != 0 {
		t.Errorf("goroutine relayed %d event(s) after ctx cancel; want 0", len(evts))
	}
}

func TestStartEmergencyEventRelayNilEventProv(t *testing.T) {
	emergencyCh := make(chan emergency.Record, 1)
	cs := &controllerState{emergencyCh: emergencyCh, eventProv: nil}

	// Must return without spawning a goroutine.
	cs.startEmergencyEventRelay(context.Background())

	rec := emergency.Record{
		ID:       "20260101T000002Z-aabbccdd",
		Severity: emergency.SeverityError,
		Actor:    "test-nil-prov",
		Message:  "no-op: nil eventProv",
	}
	// Send to buffered channel — must not block.
	emergencyCh <- rec

	// Record should remain buffered (no goroutine is consuming it).
	select {
	case got := <-emergencyCh:
		if got.ID != rec.ID {
			t.Errorf("drained unexpected record: got ID %q, want %q", got.ID, rec.ID)
		}
	case <-time.After(100 * time.Millisecond):
		t.Error("buffered record not found — it may have been consumed by a spurious relay goroutine")
	}
}

func TestStartEmergencyEventRelayNilCh(t *testing.T) {
	ep := events.NewFake()
	cs := &controllerState{emergencyCh: nil, eventProv: ep}

	// Must return without panicking.
	cs.startEmergencyEventRelay(context.Background())

	evts, err := ep.List(events.Filter{Type: events.EmergencySignaled})
	if err != nil {
		t.Fatalf("ep.List: %v", err)
	}
	if len(evts) != 0 {
		t.Errorf("unexpected events recorded: %d", len(evts))
	}
}
