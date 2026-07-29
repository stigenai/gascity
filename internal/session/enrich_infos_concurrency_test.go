package session

import (
	"sync"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/testutil"
)

type blockingEnrichmentProvider struct {
	runtime.Provider
	started chan string
	release chan struct{}
	once    sync.Once
}

func newBlockingEnrichmentProvider(t *testing.T) *blockingEnrichmentProvider {
	t.Helper()
	p := &blockingEnrichmentProvider{
		Provider: runtime.NewFake(),
		started:  make(chan string, 2),
		release:  make(chan struct{}),
	}
	t.Cleanup(func() { p.once.Do(func() { close(p.release) }) })
	return p
}

func (p *blockingEnrichmentProvider) IsAttached(name string) bool {
	p.started <- name
	<-p.release
	return false
}

func (p *blockingEnrichmentProvider) IsRunning(string) bool {
	return true
}

func (p *blockingEnrichmentProvider) GetLastActivity(string) (time.Time, error) {
	return time.Unix(123, 0), nil
}

func TestEnrichInfosBoundsIndependentRuntimeProbesConcurrently(t *testing.T) {
	provider := newBlockingEnrichmentProvider(t)
	manager := NewManagerWithOptions(beads.NewMemStore(), provider)
	infos := []Info{
		{ID: "st-first", State: StateActive, SessionName: "first"},
		{ID: "st-second", State: StateActive, SessionName: "second"},
	}

	result := make(chan []Info, 1)
	go func() {
		result <- manager.EnrichInfos(infos)
	}()

	for _, want := range []string{"first", "second"} {
		select {
		case <-provider.started:
		case <-time.After(testutil.GoroutineRaceTimeout):
			t.Fatalf("runtime enrichment did not start %q concurrently", want)
		}
	}
	provider.once.Do(func() { close(provider.release) })

	select {
	case got := <-result:
		if got[0].ID != "st-first" || got[1].ID != "st-second" {
			t.Fatalf("enriched order = [%q, %q], want persisted order", got[0].ID, got[1].ID)
		}
		for _, info := range got {
			if !info.LastActive.Equal(time.Unix(123, 0)) {
				t.Fatalf("%s last active = %v, want runtime activity", info.ID, info.LastActive)
			}
		}
	case <-time.After(testutil.GoroutineRaceTimeout):
		t.Fatal("concurrent runtime enrichment did not finish")
	}
}
