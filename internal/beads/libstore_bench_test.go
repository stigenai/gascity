package beads

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"testing"
	"time"
)

func BenchmarkLibStoreVsBdStore_Write(b *testing.B) {
	scope := newBeadsLibTestScope(b, "bmw")
	libStore := openBeadsLibStoreForTest(b, scope)
	bdStore := NewBdStoreWithPrefix(scope.root, beadsLibBenchmarkCommandRunner(scope.env), scope.prefix)

	b.ResetTimer()
	libDuration := benchmarkCreateBeads(b, libStore, "lib-write", b.N)
	bdDuration := benchmarkCreateBeads(b, bdStore, "bd-write", b.N)
	b.StopTimer()

	reportBeadsLibSpeedupGate(b, "write", bdDuration, libDuration, 10)
}

func BenchmarkLibStoreVsBdStore_BatchClose(b *testing.B) {
	scope := newBeadsLibTestScope(b, "bmc")
	libStore := openBeadsLibStoreForTest(b, scope)
	bdStore := NewBdStoreWithPrefix(scope.root, beadsLibBenchmarkCommandRunner(scope.env), scope.prefix)

	libIDs := seedBenchmarkBeads(b, libStore, "lib-close", b.N)
	bdIDs := seedBenchmarkBeads(b, bdStore, "bd-close", b.N)

	b.ResetTimer()
	libDuration := benchmarkCloseAll(b, libStore, libIDs)
	bdDuration := benchmarkCloseAll(b, bdStore, bdIDs)
	b.StopTimer()

	reportBeadsLibSpeedupGate(b, "batch close", bdDuration, libDuration, 4)
}

func beadsLibBenchmarkCommandRunner(env []string) CommandRunner {
	return func(dir, name string, args ...string) ([]byte, error) {
		ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
		defer cancel()
		cmd := exec.CommandContext(ctx, name, args...)
		cmd.Dir = dir
		cmd.Env = env
		out, err := cmd.CombinedOutput()
		if ctx.Err() == context.DeadlineExceeded {
			return out, fmt.Errorf("timed out after 120s: %s", bytes.TrimSpace(out))
		}
		if err != nil {
			return out, fmt.Errorf("%w: %s", err, bytes.TrimSpace(out))
		}
		return out, nil
	}
}

func benchmarkCreateBeads(b *testing.B, store Store, titlePrefix string, n int) time.Duration {
	b.Helper()
	start := time.Now()
	for i := 0; i < n; i++ {
		if _, err := store.Create(Bead{
			Title:    fmt.Sprintf("%s-%06d", titlePrefix, i),
			Metadata: map[string]string{"bench": titlePrefix},
		}); err != nil {
			b.Fatalf("%s create %d: %v", titlePrefix, i, err)
		}
	}
	return time.Since(start)
}

func seedBenchmarkBeads(b *testing.B, store Store, titlePrefix string, n int) []string {
	b.Helper()
	ids := make([]string, 0, n)
	for i := 0; i < n; i++ {
		created, err := store.Create(Bead{
			Title:    fmt.Sprintf("%s-%06d", titlePrefix, i),
			Metadata: map[string]string{"bench": titlePrefix},
		})
		if err != nil {
			b.Fatalf("%s seed %d: %v", titlePrefix, i, err)
		}
		ids = append(ids, created.ID)
	}
	return ids
}

func benchmarkCloseAll(b *testing.B, store Store, ids []string) time.Duration {
	b.Helper()
	start := time.Now()
	closed, err := store.CloseAll(ids, map[string]string{"bench_closed": "true"})
	if err != nil {
		b.Fatalf("CloseAll(%d ids): %v", len(ids), err)
	}
	if closed != len(ids) {
		b.Fatalf("CloseAll closed %d beads, want %d", closed, len(ids))
	}
	return time.Since(start)
}

func reportBeadsLibSpeedupGate(b *testing.B, name string, bdDuration, libDuration time.Duration, minRatio float64) {
	b.Helper()
	if libDuration <= 0 {
		b.Fatalf("%s lib duration = %s, want positive duration", name, libDuration)
	}
	ratio := float64(bdDuration) / float64(libDuration)
	b.ReportMetric(float64(libDuration.Nanoseconds())/float64(b.N), "lib-ns/op")
	b.ReportMetric(float64(bdDuration.Nanoseconds())/float64(b.N), "bd-ns/op")
	b.ReportMetric(ratio, "bd/lib")
	if ratio < minRatio {
		b.Fatalf("%s speedup = %.2fx, want >= %.2fx (bd=%s lib=%s)", name, ratio, minRatio, bdDuration, libDuration)
	}
}
