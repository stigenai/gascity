//go:build acceptance_live_gascity

// Live acceptance tests for ga-673qo6: gascity Dolt local-only remote durability.
//
// Verifies that after applying the bd bridge fix (commit 06c631d25) the live
// gascity Dolt instance does not re-register the origin SQL remote after normal
// bd/gc activity, even when dolt.local-only: true is configured.
//
// Run with:
//
//	go test -tags acceptance_live_gascity ./test/acceptance/ -run TestDoltLocalOnly -v
//
// Skip condition: if /home/jaword/projects/gascity/.beads/dolt-server.port is
// unreadable or the Dolt server is not reachable, the tests skip gracefully.
package acceptance_test

import (
	"context"
	"database/sql"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	mysql "github.com/go-sql-driver/mysql"
)

const (
	liveGascityRoot     = "/home/jaword/projects/gascity"
	liveGascityBeadsDir = "/home/jaword/projects/gascity/.beads"
)

// doltRemoteCounts holds aggregate counts from the dolt_remotes table.
type doltRemoteCounts struct {
	Total             int
	OriginCount       int
	BackupExportCount int
}

// openLiveDolt opens the live gascity Dolt SQL connection.
// Skips the test if the port file is missing or the server is unreachable.
func openLiveDolt(t *testing.T) (*sql.DB, string) {
	t.Helper()
	portPath := filepath.Join(liveGascityBeadsDir, "dolt-server.port")
	portBytes, err := os.ReadFile(portPath)
	if err != nil {
		t.Skipf("dolt-server.port not readable (%v); skipping live gascity test", err)
	}
	port := strings.TrimSpace(string(portBytes))

	cfg := mysql.NewConfig()
	cfg.User = "root"
	cfg.Net = "tcp"
	cfg.Addr = "127.0.0.1:" + port
	cfg.DBName = "gascity"
	cfg.Timeout = 5 * time.Second
	cfg.ReadTimeout = 5 * time.Second
	cfg.WriteTimeout = 5 * time.Second
	cfg.AllowNativePasswords = true

	db, err := sql.Open("mysql", cfg.FormatDSN())
	if err != nil {
		t.Skipf("sql.Open for live Dolt: %v", err)
	}

	pingCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := db.PingContext(pingCtx); err != nil {
		db.Close() //nolint:errcheck
		t.Skipf("live gascity Dolt not reachable on port %s (%v); skipping", port, err)
	}

	t.Cleanup(func() { db.Close() }) //nolint:errcheck
	return db, port
}

// countDoltRemotes queries dolt_remotes for the origin and backup_export counts.
func countDoltRemotes(t *testing.T, db *sql.DB) doltRemoteCounts {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	var counts doltRemoteCounts
	var originCount, backupExportCount sql.NullInt64
	row := db.QueryRowContext(ctx, `
		SELECT
			COUNT(*),
			SUM(CASE WHEN name = 'origin'        THEN 1 ELSE 0 END),
			SUM(CASE WHEN name = 'backup_export' THEN 1 ELSE 0 END)
		FROM dolt_remotes
	`)
	if err := row.Scan(&counts.Total, &originCount, &backupExportCount); err != nil {
		t.Fatalf("scanning dolt_remotes: %v", err)
	}
	if originCount.Valid {
		counts.OriginCount = int(originCount.Int64)
	}
	if backupExportCount.Valid {
		counts.BackupExportCount = int(backupExportCount.Int64)
	}
	return counts
}

// tailLines returns the last n lines of s.
func tailLines(s string, n int) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	if len(lines) <= n {
		return strings.TrimSpace(s)
	}
	return strings.Join(lines[len(lines)-n:], "\n")
}

// findGoModDir walks up from dir to find the directory that contains go.mod.
func findGoModDir(t *testing.T, dir string) string {
	t.Helper()
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatalf("go.mod not found walking up from %s", dir)
		}
		dir = parent
	}
}

// TestDoltLocalOnlyRemoteDurabilityLiveGascity verifies that normal bd activity
// does not re-register the origin SQL remote in the live gascity Dolt database
// after the bd bridge fix (06c631d25) is installed and dolt.local-only is true.
//
// Acceptance criteria covered: AC-1, AC-2, AC-3, AC-4, AC-6 (ga-673qo6.3).
func TestDoltLocalOnlyRemoteDurabilityLiveGascity(t *testing.T) {
	db, port := openLiveDolt(t)

	// AC-1: port recorded.
	t.Logf("AC-1: dolt-server.port=%s (from %s)", port, liveGascityBeadsDir)

	// AC-2: query dolt_remotes before trigger.
	before := countDoltRemotes(t, db)
	t.Logf("AC-2 before trigger: remote_count=%d origin_count=%d backup_export_count=%d",
		before.Total, before.OriginCount, before.BackupExportCount)

	// If origin is present, perform the one-time controlled cleanup permitted by
	// the architecture decision (ga-om4nvp): at most one CALL DOLT_REMOTE remove.
	if before.OriginCount > 0 {
		t.Logf("origin present before trigger; performing at-most-one cleanup (ga-om4nvp guardrail)")
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		_, cleanErr := db.ExecContext(ctx, "CALL DOLT_REMOTE('remove', 'origin')")
		cancel()
		if cleanErr != nil {
			t.Fatalf("at-most-one cleanup CALL DOLT_REMOTE('remove', 'origin'): %v", cleanErr)
		}
		postClean := countDoltRemotes(t, db)
		t.Logf("post-cleanup: remote_count=%d origin_count=%d", postClean.Total, postClean.OriginCount)
		if postClean.OriginCount != 0 {
			t.Fatalf("cleanup did not remove origin: origin_count still %d", postClean.OriginCount)
		}
		before = postClean
	}
	nonOriginBefore := before.Total - before.OriginCount

	// AC-3: trigger bd activity of the class that previously caused origin to reappear.
	triggerCmd := exec.Command("bd", "list", "--status=open")
	triggerCmd.Dir = liveGascityRoot
	triggerOut, triggerErr := triggerCmd.CombinedOutput()
	t.Logf("AC-3 trigger: bd list --status=open (cwd=%s)", liveGascityRoot)
	t.Logf("AC-3 trigger output (tail): %s", tailLines(string(triggerOut), 3))
	if triggerErr != nil {
		t.Fatalf("AC-3 trigger command failed: %v", triggerErr)
	}

	// AC-4: origin_count must be 0 after trigger.
	after := countDoltRemotes(t, db)
	t.Logf("AC-4 after trigger: remote_count=%d origin_count=%d backup_export_count=%d",
		after.Total, after.OriginCount, after.BackupExportCount)

	if after.OriginCount != 0 {
		t.Errorf("AC-4 FAIL: origin_count=%d after bd trigger; bd is re-registering SQL remote under local-only config", after.OriginCount)
	}

	// AC-4: non-origin remotes must not be silently removed.
	nonOriginAfter := after.Total - after.OriginCount
	if nonOriginAfter < nonOriginBefore {
		t.Errorf("AC-4 FAIL: non-origin remote count decreased: before=%d after=%d", nonOriginBefore, nonOriginAfter)
	}

	// AC-6: backup_export remote specifically must not be removed.
	if after.BackupExportCount < before.BackupExportCount {
		t.Errorf("AC-6 FAIL: backup_export remote was removed: before=%d after=%d", before.BackupExportCount, after.BackupExportCount)
	}
}

// TestDoltLocalOnlyGa673qo61RegressionSuiteGreenCheck runs the unit regression
// suites from ga-673qo6.1 as subprocesses and verifies they pass. These tests
// require the builder's implementation from ga-673qo6.2 (checks_dolt_local_only.go
// and the newDoctorDoltLocalOnlyCheck registration in cmd_doctor.go) to be
// present on this branch.
//
// This test will report FAIL until ga-673qo6.1 test files and ga-673qo6.2
// implementation are merged to the current branch.
//
// Acceptance criteria covered: AC-5 (ga-673qo6.3).
func TestDoltLocalOnlyGa673qo61RegressionSuiteGreenCheck(t *testing.T) {
	wd, err := os.Getwd()
	if err != nil {
		t.Fatalf("os.Getwd: %v", err)
	}
	modRoot := findGoModDir(t, wd)
	t.Logf("AC-5: module root: %s", modRoot)

	suites := []struct {
		pkg string
		run string
	}{
		{
			pkg: "./internal/doctor/...",
			run: "TestDoltLocalOnlyRemoteCheck",
		},
		{
			pkg: "./cmd/gc/...",
			run: "TestDoDoctorRegistersLocalOnlyRemoteCheckForActiveManagedRigs|TestDoDoctorSkipsLocalOnlyCheckWhenGCDoltSkip",
		},
	}

	for _, s := range suites {
		s := s
		t.Run(s.pkg, func(t *testing.T) {
			cmd := exec.Command("go", "test", "-count=1", "-run", s.run, s.pkg)
			cmd.Dir = modRoot
			out, err := cmd.CombinedOutput()
			t.Logf("go test -count=1 -run %s %s:\n%s", s.run, s.pkg, out)
			if err != nil {
				t.Errorf("AC-5 FAIL (%s): %v — requires ga-673qo6.1 test files and ga-673qo6.2 implementation on this branch", s.pkg, err)
			}
		})
	}
}
