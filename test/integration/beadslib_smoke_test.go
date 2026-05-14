//go:build integration

package integration

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestBeadsLibDriverServerModeCitySmoke(t *testing.T) {
	requireDoltIntegration(t)
	env := newIsolatedCommandEnv(t, true)
	env = cleanBeadsLibSmokeEnv(env)

	cityName := uniqueCityName()
	cityDir := filepath.Join(t.TempDir(), cityName)
	configPath := filepath.Join(t.TempDir(), cityName+".toml")
	writeBeadsLibSmokeCityConfig(t, configPath, cityName)

	initCityWithManagedDoltRecovery(t, env, configPath, cityDir)
	registerCityCommandEnv(cityDir, env)
	t.Cleanup(func() {
		unregisterCityCommandEnv(cityDir)
		runGCDoltWithEnv(env, "", "stop", cityDir)                //nolint:errcheck // best-effort cleanup
		runGCDoltWithEnv(env, "", "supervisor", "stop", "--wait") //nolint:errcheck // best-effort cleanup
	})

	if out, err := runGCDoltWithEnv(env, "", "start", cityDir); err != nil && !isGCStartAlreadyRunning(out) {
		t.Fatalf("gc start beadslib city: %v\n%s", err, out)
	}
	if out, err := waitForManagedDoltCityReady(env, cityDir, 20*time.Second); err != nil {
		t.Fatalf("beadslib city never reached managed Dolt readiness: %v\n%s", err, out)
	}

	for i := 0; i < 12; i++ {
		title := fmt.Sprintf("beadslib-smoke-%02d", i)
		out, err := gcDolt(cityDir, "bd", "create", title)
		requireNoBeadsLibSmokeStarvation(t, out)
		if err != nil {
			t.Fatalf("gc bd create %q: %v\n%s", title, err, out)
		}
	}

	listOut, err := gcDolt(cityDir, "bd", "list", "--json", "--limit=0")
	requireNoBeadsLibSmokeStarvation(t, listOut)
	if err != nil {
		t.Fatalf("gc bd list after beadslib writes: %v\n%s", err, listOut)
	}
	if !strings.Contains(listOut, "beadslib-smoke-11") {
		t.Fatalf("gc bd list output missing latest beadslib smoke bead:\n%s", listOut)
	}

	statusOut, err := gcDolt(cityDir, "status", "--json")
	requireNoBeadsLibSmokeStarvation(t, statusOut)
	if err != nil {
		t.Fatalf("gc status --json after beadslib writes: %v\n%s", err, statusOut)
	}
	if strings.Contains(statusOut, "cache_not_live") {
		t.Fatalf("gc status reported non-live cache after beadslib smoke writes:\n%s", statusOut)
	}

	start := time.Now()
	var wg sync.WaitGroup
	errs := make(chan string, 20)
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			out, err := gcDolt(cityDir, "bd", "list", "--json", "--limit=0")
			if err != nil {
				errs <- fmt.Sprintf("concurrent list %d: %v\n%s", i, err, out)
				return
			}
			if starvation := beadsLibSmokeStarvationMessage(out); starvation != "" {
				errs <- fmt.Sprintf("concurrent list %d reported %s\n%s", i, starvation, out)
				return
			}
			if !strings.Contains(out, "beadslib-smoke-00") {
				errs <- fmt.Sprintf("concurrent list %d missed seeded bead\n%s", i, out)
			}
		}(i)
	}
	wg.Wait()
	close(errs)
	if len(errs) > 0 {
		t.Fatal(<-errs)
	}
	if elapsed := time.Since(start); elapsed > 30*time.Second {
		t.Fatalf("20 concurrent beadslib read probes took %s, want <30s smoke budget", elapsed)
	}
}

func cleanBeadsLibSmokeEnv(env []string) []string {
	for _, name := range []string{
		"GC_AGENT",
		"GC_AGENT_ID",
		"GC_BEADS_SCOPE_ROOT",
		"GC_CITY",
		"GC_CITY_PATH",
		"GC_CITY_ROOT",
		"GC_RIG",
		"GC_TEMPLATE",
	} {
		env = filterEnv(env, name)
	}
	return env
}

func writeBeadsLibSmokeCityConfig(t *testing.T, path, cityName string) {
	t.Helper()
	content := fmt.Sprintf(`[workspace]
name = %q

[beads]
provider = "bd"

[backend]
driver = "beadslib"

[session]
provider = "fake"
`, cityName)
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write beadslib smoke city config: %v", err)
	}
}

func requireNoBeadsLibSmokeStarvation(t *testing.T, output string) {
	t.Helper()
	if msg := beadsLibSmokeStarvationMessage(output); msg != "" {
		t.Fatalf("beadslib smoke output reported %s:\n%s", msg, output)
	}
}

func beadsLibSmokeStarvationMessage(output string) string {
	lower := strings.ToLower(output)
	for _, needle := range []string{
		"too many connections",
		"connection pool",
		"pool exhausted",
		"database is locked",
		"driver: bad connection",
		"connection refused",
	} {
		if strings.Contains(lower, needle) {
			return needle
		}
	}
	return ""
}
