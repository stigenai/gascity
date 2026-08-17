package omnigentcapsule

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"
)

const (
	performanceTestEnv = "GC_OMNIGENT_PERF_TEST"
	startupSamples     = 25
	reconnectSamples   = 100
	startupBudget      = 2 * time.Second
	reconnectBudget    = 2 * time.Second
)

func TestReleaseGateStartupAndReconnectBudgets(t *testing.T) {
	if os.Getenv(performanceTestEnv) != "1" {
		t.Skip("set GC_OMNIGENT_PERF_TEST=1 to run local capsule latency budgets")
	}

	startupBase := t.TempDir()
	startupStarted := time.Now()
	for index := range startupSamples {
		root := filepath.Join(startupBase, fmt.Sprintf("capsule-%03d", index))
		if err := os.Mkdir(root, 0o700); err != nil {
			t.Fatal(err)
		}
		fixture, err := New(root, Config{
			CapsuleID: fmt.Sprintf("ga-perf-%03d", index),
			Transport: TransportSSH,
			ProfileID: ProfileCodex,
		})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := fixture.Start(); err != nil {
			t.Fatal(err)
		}
		if err := fixture.Cleanup(false); err != nil {
			t.Fatal(err)
		}
		if err := fixture.AssertClean(); err != nil {
			t.Fatal(err)
		}
	}
	startupElapsed := time.Since(startupStarted)
	if startupElapsed > startupBudget {
		t.Fatalf("%d capsule starts took %s, budget %s", startupSamples, startupElapsed, startupBudget)
	}

	reconnectFixture, err := New(t.TempDir(), Config{
		CapsuleID: "ga-perf-reconnect",
		Transport: TransportKubernetes,
		ProfileID: ProfileClaudePrimary,
	})
	if err != nil {
		t.Fatal(err)
	}
	started, err := reconnectFixture.Start()
	if err != nil {
		t.Fatal(err)
	}
	reconnectStarted := time.Now()
	for index := range reconnectSamples {
		reconnectFixture.Inject(FaultTransportLoss)
		if _, err := reconnectFixture.Run("must fail during transport loss"); !errors.Is(err, ErrTransportLost) {
			t.Fatalf("cycle %d transport-loss error = %v", index, err)
		}
		reconnectFixture.RestoreTransport()
		result, err := reconnectFixture.Run(fmt.Sprintf("reconnect-%03d", index))
		if err != nil {
			t.Fatal(err)
		}
		if result.ConversationID != started.ConversationID {
			t.Fatalf("cycle %d replaced conversation: got %q want %q", index, result.ConversationID, started.ConversationID)
		}
	}
	reconnectElapsed := time.Since(reconnectStarted)
	if reconnectElapsed > reconnectBudget {
		t.Fatalf("%d disconnect/reconnect cycles took %s, budget %s", reconnectSamples, reconnectElapsed, reconnectBudget)
	}
	if err := reconnectFixture.Cleanup(false); err != nil {
		t.Fatal(err)
	}
	if err := reconnectFixture.AssertClean(); err != nil {
		t.Fatal(err)
	}

	t.Logf("startup: %d samples in %s (budget %s); reconnect: %d samples in %s (budget %s)",
		startupSamples, startupElapsed, startupBudget,
		reconnectSamples, reconnectElapsed, reconnectBudget,
	)
}
