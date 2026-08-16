package herdr

import (
	"fmt"
	"os/exec"
	"sync/atomic"
	"testing"

	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/runtime/runtimetest"
)

// TestHerdrConformance runs the full runtime.Provider conformance suite against
// the herdr provider backed by a real herdr binary. Each session gets its own
// isolated herdr session-server so the contract's session-scoped assertions
// (ListRunning, orphan detection, …) don't observe sibling sessions. Skipped
// when herdr is unavailable or in -short mode.
func TestHerdrConformance(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping live herdr conformance in -short mode")
	}
	if _, err := exec.LookPath("herdr"); err != nil {
		t.Skip("herdr not installed")
	}
	var counter int64
	runtimetest.RunProviderTests(t, func(t *testing.T) (runtime.Provider, runtime.Config, string) {
		n := atomic.AddInt64(&counter, 1)
		p := New(fmt.Sprintf("gctest-conf-%d", n), t.TempDir(), t.TempDir(), 0, 0)
		// The provider starts its session-server lazily, on Start. Cases that
		// never Start anything therefore ran against a socket that had never
		// been bound. A live provider is the suite's premise, not a thing under
		// test; the TeardownServer cleanup below already assumes it.
		if err := p.ConfigureServer(); err != nil {
			t.Fatalf("configure herdr session-server: %v", err)
		}
		t.Cleanup(func() { _ = p.TeardownServer() })
		return p, runtime.Config{WorkDir: t.TempDir()}, fmt.Sprintf("conf-%d", n)
	})
}
