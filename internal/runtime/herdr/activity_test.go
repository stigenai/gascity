package herdr

import (
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/runtime"
)

func TestObservedActivityPersistsTransitionsAndWorkingHeartbeats(t *testing.T) {
	metaDir := t.TempDir()
	p := New("test", metaDir, t.TempDir(), time.Second, time.Second)
	now := time.Date(2026, 7, 28, 12, 0, 0, 0, time.UTC)
	p.now = func() time.Time { return now }

	if err := p.recordObservedActivity("worker", "idle"); err != nil {
		t.Fatalf("record initial activity: %v", err)
	}
	assertLastActivity(t, p, now)

	now = now.Add(time.Minute)
	if err := p.recordObservedActivity("worker", "idle"); err != nil {
		t.Fatalf("record unchanged idle activity: %v", err)
	}
	assertLastActivity(t, p, now.Add(-time.Minute))

	if err := p.recordObservedActivity("worker", "working"); err != nil {
		t.Fatalf("record transition to working: %v", err)
	}
	assertLastActivity(t, p, now)

	now = now.Add(time.Minute)
	if err := p.recordObservedActivity("worker", "working"); err != nil {
		t.Fatalf("record working heartbeat: %v", err)
	}
	assertLastActivity(t, p, now)

	now = now.Add(time.Minute)
	if err := p.recordObservedActivity("worker", "idle"); err != nil {
		t.Fatalf("record transition to idle: %v", err)
	}
	assertLastActivity(t, p, now)

	// Activity is a durable provider observation, not process-local state.
	restarted := New("test", metaDir, t.TempDir(), time.Second, time.Second)
	assertLastActivity(t, restarted, now)
}

func TestMarkActivityDoesNotInventAStatusTransition(t *testing.T) {
	p := New("test", t.TempDir(), t.TempDir(), time.Second, time.Second)
	now := time.Date(2026, 7, 28, 12, 0, 0, 0, time.UTC)
	p.now = func() time.Time { return now }

	if err := p.recordObservedActivity("worker", "idle"); err != nil {
		t.Fatalf("record idle: %v", err)
	}
	now = now.Add(time.Minute)
	if err := p.markActivity("worker"); err != nil {
		t.Fatalf("mark nudge activity: %v", err)
	}
	assertLastActivity(t, p, now)

	now = now.Add(time.Minute)
	if err := p.recordObservedActivity("worker", "idle"); err != nil {
		t.Fatalf("record unchanged idle after nudge: %v", err)
	}
	assertLastActivity(t, p, now.Add(-time.Minute))
}

func TestNudgeDoesNotCountControllerInputAsAgentActivity(t *testing.T) {
	p, _, state := newFakeHerdrProvider(t)
	setState(t, state, "registered")
	now := time.Date(2026, 7, 28, 12, 0, 0, 0, time.UTC)
	p.now = func() time.Time { return now }
	if err := p.recordObservedActivity("worker", "idle"); err != nil {
		t.Fatalf("record idle: %v", err)
	}

	now = now.Add(time.Minute)
	if err := p.Nudge("worker", []runtime.ContentBlock{{Type: "text", Text: "wake"}}); err != nil {
		t.Fatalf("Nudge: %v", err)
	}
	assertLastActivity(t, p, now.Add(-time.Minute))
}

func TestGetLastActivityRejectsCorruptSidecar(t *testing.T) {
	p := New("test", t.TempDir(), t.TempDir(), time.Second, time.Second)
	if err := p.SetMeta("worker", metaLastActivity, "not-a-time"); err != nil {
		t.Fatalf("seed corrupt activity: %v", err)
	}
	if _, err := p.GetLastActivity("worker"); err == nil {
		t.Fatal("GetLastActivity accepted a corrupt activity timestamp")
	}
}

func TestCapabilitiesDeclareActivity(t *testing.T) {
	p := New("test", t.TempDir(), t.TempDir(), time.Second, time.Second)
	if !p.Capabilities().CanReportActivity {
		t.Fatal("CanReportActivity = false")
	}
}

func TestObserveLivenessPublishesDurableActivity(t *testing.T) {
	p, _, state := newFakeHerdrProvider(t)
	setState(t, state, "registered")
	setState(t, state, "busy")
	now := time.Date(2026, 7, 28, 12, 0, 0, 0, time.UTC)
	p.now = func() time.Time { return now }

	got := p.ObserveLiveness("worker", nil)
	if !got.Running || !got.Alive {
		t.Fatalf("ObserveLiveness = %+v, want running and alive", got)
	}
	assertLastActivity(t, p, now)

	now = now.Add(time.Minute)
	got = p.ObserveLiveness("worker", nil)
	if !got.Running || !got.Alive {
		t.Fatalf("second ObserveLiveness = %+v, want running and alive", got)
	}
	assertLastActivity(t, p, now.Add(-time.Minute))
}

func TestObserveBoundSessionPublishesDurableActivity(t *testing.T) {
	p, _, state := newFakeHerdrProvider(t)
	setState(t, state, "busy")
	bindTestPane(t, p, "worker", bindModeAgent)
	now := time.Date(2026, 7, 28, 12, 0, 0, 0, time.UTC)
	p.now = func() time.Time { return now }

	got := p.ObserveLiveness("worker", nil)
	if !got.Running || !got.Alive {
		t.Fatalf("ObserveLiveness = %+v, want bound session running and alive", got)
	}
	assertLastActivity(t, p, now)
}

func assertLastActivity(t *testing.T, p *Provider, want time.Time) {
	t.Helper()
	got, err := p.GetLastActivity("worker")
	if err != nil {
		t.Fatalf("GetLastActivity(%q): %v", "worker", err)
	}
	if !got.Equal(want) {
		t.Fatalf("GetLastActivity(%q) = %s, want %s", "worker", got, want)
	}
}
