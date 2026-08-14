package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/config"
)

func TestBdListShouldFanOut(t *testing.T) {
	tests := []struct {
		name         string
		rigName      string
		cityExplicit bool
		args         []string
		want         bool
	}{
		{"eligible: bare list --json", "", false, []string{"list", "--json"}, true},
		{"eligible: list --json with other filters", "", false, []string{"list", "--type=molecule", "--limit=0", "--json"}, true},
		{"not list: show", "", false, []string{"show", "gc-1", "--json"}, false},
		{"not list: create", "", false, []string{"create", "--json", "title"}, false},
		{"empty args", "", false, nil, false},
		{"explicit --rig", "myrig", false, []string{"list", "--json"}, false},
		{"explicit --city", "", true, []string{"list", "--json"}, false},
		{"explicit -C dir", "", false, []string{"list", "--json", "-C", "/some/rig"}, false},
		{"explicit --directory= dir", "", false, []string{"list", "--json", "--directory=/some/rig"}, false},
		{"no --json", "", false, []string{"list", "--type=molecule"}, false},
		{"--format overrides output shape", "", false, []string{"list", "--json", "--format", "dot"}, false},
		{"--format= overrides output shape", "", false, []string{"list", "--json", "--format=digraph"}, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := bdListShouldFanOut(tc.rigName, tc.cityExplicit, tc.args); got != tc.want {
				t.Fatalf("bdListShouldFanOut(%q, %v, %v) = %v, want %v", tc.rigName, tc.cityExplicit, tc.args, got, tc.want)
			}
		})
	}
}

func TestBdListRequestedLimit(t *testing.T) {
	tests := []struct {
		name          string
		args          []string
		wantLimit     int
		wantUnlimited bool
	}{
		{"no limit flag defaults to bd's own 50", []string{"list", "--json"}, 50, false},
		{"--limit=0 is unlimited", []string{"list", "--limit=0", "--json"}, 0, true},
		{"-n 0 is unlimited", []string{"list", "-n", "0", "--json"}, 0, true},
		{"--limit=10", []string{"list", "--limit=10", "--json"}, 10, false},
		{"--limit 10 space form", []string{"list", "--limit", "10", "--json"}, 10, false},
		{"-n 5 short form", []string{"list", "-n", "5", "--json"}, 5, false},
		{"trailing --limit with no value", []string{"list", "--limit"}, 50, false},
		{"malformed value falls back to default", []string{"list", "--limit=abc"}, 50, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			gotLimit, gotUnlimited := bdListRequestedLimit(tc.args)
			if gotLimit != tc.wantLimit || gotUnlimited != tc.wantUnlimited {
				t.Fatalf("bdListRequestedLimit(%v) = (%d, %v), want (%d, %v)", tc.args, gotLimit, gotUnlimited, tc.wantLimit, tc.wantUnlimited)
			}
		})
	}
}

func TestBdFanOutTargets(t *testing.T) {
	cityDir := t.TempDir()
	cfg := &config.City{
		Rigs: []config.Rig{
			{Name: "alpha", Path: filepath.Join(cityDir, "alpha"), Prefix: "al"},
			{Name: "beta", Path: filepath.Join(cityDir, "beta"), Prefix: "be"},
			{Name: "unbound", Path: ""}, // no path binding — must be skipped
		},
	}

	t.Run("primary is city: adds every bound rig, no duplicate city", func(t *testing.T) {
		primary := bdCityScopeTarget(cityDir, cfg)
		got := bdFanOutTargets(cfg, cityDir, primary)
		var roots []string
		for _, tgt := range got {
			roots = append(roots, tgt.ScopeRoot)
		}
		if roots[0] != primary.ScopeRoot {
			t.Fatalf("targets[0] = %q, want primary %q first", roots[0], primary.ScopeRoot)
		}
		want := map[string]bool{primary.ScopeRoot: true, filepath.Join(cityDir, "alpha"): true, filepath.Join(cityDir, "beta"): true}
		if len(got) != len(want) {
			t.Fatalf("targets = %v, want exactly %v (unbound rig must be skipped, city not duplicated)", roots, want)
		}
		for _, r := range roots {
			if !want[r] {
				t.Fatalf("unexpected target root %q in %v", r, roots)
			}
		}
	})

	t.Run("primary is a configured rig: not duplicated, others still added", func(t *testing.T) {
		primary := bdRigScopeTarget(cityDir, cfg.Rigs[0]) // alpha
		got := bdFanOutTargets(cfg, cityDir, primary)
		if got[0].ScopeRoot != primary.ScopeRoot {
			t.Fatalf("targets[0] = %q, want primary %q first", got[0].ScopeRoot, primary.ScopeRoot)
		}
		count := 0
		for _, tgt := range got {
			if tgt.ScopeRoot == primary.ScopeRoot {
				count++
			}
		}
		if count != 1 {
			t.Fatalf("primary root %q appeared %d times in %v, want exactly 1 (no duplicate)", primary.ScopeRoot, count, got)
		}
		if len(got) != 3 { // alpha (primary) + beta + city
			t.Fatalf("targets = %v, want 3 (primary + beta + city)", got)
		}
	})

	t.Run("nil config returns just the primary", func(t *testing.T) {
		primary := bdCityScopeTarget(cityDir, cfg)
		got := bdFanOutTargets(nil, cityDir, primary)
		if len(got) != 1 || got[0].ScopeRoot != primary.ScopeRoot {
			t.Fatalf("targets = %v, want [primary] only", got)
		}
	})
}

// fanOutCall records one invocation for assertions in the injectable-runner
// tests below.
type fanOutCall struct {
	dir string
}

func TestDoBdListFanOutMergesAcrossStores(t *testing.T) {
	cityDir := t.TempDir()
	cfg := &config.City{Rigs: []config.Rig{
		{Name: "alpha", Path: filepath.Join(cityDir, "alpha"), Prefix: "al"},
		{Name: "beta", Path: filepath.Join(cityDir, "beta"), Prefix: "be"},
	}}
	primary := bdCityScopeTarget(cityDir, cfg)

	var calls []fanOutCall
	run := func(_ []string, dir string, _ []string, _ io.Writer) (string, error) {
		calls = append(calls, fanOutCall{dir: dir})
		switch dir {
		case cityDir:
			return `[{"id":"city-1"}]`, nil
		case filepath.Join(cityDir, "alpha"):
			return `[{"id":"al-1"}]`, nil
		case filepath.Join(cityDir, "beta"):
			return `[{"id":"be-1"}]`, nil
		default:
			t.Fatalf("unexpected store dir %q", dir)
			return "", nil
		}
	}

	var stdout, stderr bytes.Buffer
	code := doBdListFanOut(cfg, cityDir, []string{"list", "--json"}, primary, &stdout, &stderr, run)
	if code != 0 {
		t.Fatalf("doBdListFanOut() = %d, want 0; stderr=%q", code, stderr.String())
	}
	if len(calls) != 3 {
		t.Fatalf("run invoked %d times, want 3 (city + alpha + beta); calls=%v", len(calls), calls)
	}

	gotIDs := decodeIDs(t, stdout.Bytes())
	sort.Strings(gotIDs)
	want := []string{"al-1", "be-1", "city-1"}
	if !equalStrings(gotIDs, want) {
		t.Fatalf("merged ids = %v, want %v", gotIDs, want)
	}
}

func TestDoBdListFanOutRespectsLimit(t *testing.T) {
	cityDir := t.TempDir()
	cfg := &config.City{Rigs: []config.Rig{
		{Name: "alpha", Path: filepath.Join(cityDir, "alpha"), Prefix: "al"},
		{Name: "beta", Path: filepath.Join(cityDir, "beta"), Prefix: "be"},
	}}
	primary := bdCityScopeTarget(cityDir, cfg)

	var calls []fanOutCall
	run := func(_ []string, dir string, _ []string, _ io.Writer) (string, error) {
		calls = append(calls, fanOutCall{dir: dir})
		// Every store "has" 2 rows; a limit of 3 should stop after the
		// second store (city:2 + alpha:1, or however the truncation lands)
		// without ever reaching beta.
		switch dir {
		case cityDir:
			return `[{"id":"city-1"},{"id":"city-2"}]`, nil
		case filepath.Join(cityDir, "alpha"):
			return `[{"id":"al-1"},{"id":"al-2"}]`, nil
		case filepath.Join(cityDir, "beta"):
			return `[{"id":"be-1"},{"id":"be-2"}]`, nil
		default:
			t.Fatalf("unexpected store dir %q", dir)
			return "", nil
		}
	}

	var stdout, stderr bytes.Buffer
	code := doBdListFanOut(cfg, cityDir, []string{"list", "--limit=3", "--json"}, primary, &stdout, &stderr, run)
	if code != 0 {
		t.Fatalf("doBdListFanOut() = %d, want 0; stderr=%q", code, stderr.String())
	}
	gotIDs := decodeIDs(t, stdout.Bytes())
	if len(gotIDs) != 3 {
		t.Fatalf("merged ids = %v, want exactly 3 (limit=3)", gotIDs)
	}
	if len(calls) != 2 {
		t.Fatalf("run invoked %d times, want 2 (must stop querying once the limit is reached, never reach beta); calls=%v", len(calls), calls)
	}
}

func TestDoBdListFanOutUnlimitedReturnsEverything(t *testing.T) {
	cityDir := t.TempDir()
	cfg := &config.City{Rigs: []config.Rig{{Name: "alpha", Path: filepath.Join(cityDir, "alpha"), Prefix: "al"}}}
	primary := bdCityScopeTarget(cityDir, cfg)

	run := func(_ []string, dir string, _ []string, _ io.Writer) (string, error) {
		if dir == filepath.Join(cityDir, "alpha") {
			return `[{"id":"al-1"},{"id":"al-2"},{"id":"al-3"}]`, nil
		}
		return `[{"id":"city-1"}]`, nil
	}

	var stdout, stderr bytes.Buffer
	code := doBdListFanOut(cfg, cityDir, []string{"list", "--limit=0", "--json"}, primary, &stdout, &stderr, run)
	if code != 0 {
		t.Fatalf("doBdListFanOut() = %d, want 0; stderr=%q", code, stderr.String())
	}
	gotIDs := decodeIDs(t, stdout.Bytes())
	if len(gotIDs) != 4 {
		t.Fatalf("merged ids = %v, want all 4 rows (unlimited)", gotIDs)
	}
}

func TestDoBdListFanOutPrimaryErrorIsFatal(t *testing.T) {
	cityDir := t.TempDir()
	cfg := &config.City{Rigs: []config.Rig{{Name: "alpha", Path: filepath.Join(cityDir, "alpha"), Prefix: "al"}}}
	primary := bdCityScopeTarget(cityDir, cfg)

	wantErr := errors.New("boom")
	called := 0
	run := func(_ []string, dir string, _ []string, _ io.Writer) (string, error) {
		called++
		if dir == cityDir {
			return "", wantErr
		}
		t.Fatalf("federated store %q must not be queried once the primary has already failed fatally", dir)
		return "", nil
	}

	var stdout, stderr bytes.Buffer
	code := doBdListFanOut(cfg, cityDir, []string{"list", "--json"}, primary, &stdout, &stderr, run)
	if code != 1 {
		t.Fatalf("doBdListFanOut() = %d, want 1 on primary error", code)
	}
	if !strings.Contains(stderr.String(), "boom") {
		t.Fatalf("stderr = %q, want it to surface the primary error", stderr.String())
	}
	if called != 1 {
		t.Fatalf("run called %d times, want 1 (primary only)", called)
	}
}

func TestDoBdListFanOutFederatedErrorIsBestEffort(t *testing.T) {
	cityDir := t.TempDir()
	cfg := &config.City{Rigs: []config.Rig{{Name: "alpha", Path: filepath.Join(cityDir, "alpha"), Prefix: "al"}}}
	primary := bdCityScopeTarget(cityDir, cfg)

	run := func(_ []string, dir string, _ []string, _ io.Writer) (string, error) {
		if dir == cityDir {
			return `[{"id":"city-1"}]`, nil
		}
		return "", errors.New("alpha store unreachable")
	}

	var stdout, stderr bytes.Buffer
	code := doBdListFanOut(cfg, cityDir, []string{"list", "--json"}, primary, &stdout, &stderr, run)
	if code != 0 {
		t.Fatalf("doBdListFanOut() = %d, want 0 (a federated store's error must not fail the whole call); stderr=%q", code, stderr.String())
	}
	gotIDs := decodeIDs(t, stdout.Bytes())
	if !equalStrings(gotIDs, []string{"city-1"}) {
		t.Fatalf("merged ids = %v, want [city-1] (primary's results survive a federated failure)", gotIDs)
	}
}

// TestDoBdListFanOutPrimarySilentFallbackIsFatal covers gcy-qu1d: bd exiting
// 0 with its silent fallback-to-on-disk marker on the primary store must be
// treated as fatal, matching doBd's own single-store contract exactly (same
// exit code), not merged in as if the (possibly stale) results were
// authoritative.
func TestDoBdListFanOutPrimarySilentFallbackIsFatal(t *testing.T) {
	cityDir := t.TempDir()
	cfg := &config.City{Rigs: []config.Rig{{Name: "alpha", Path: filepath.Join(cityDir, "alpha"), Prefix: "al"}}}
	primary := bdCityScopeTarget(cityDir, cfg)

	called := 0
	run := func(_ []string, dir string, _ []string, _ io.Writer) (string, error) {
		called++
		if dir == cityDir {
			return "", errBdListFanOutSilentFallback
		}
		t.Fatalf("federated store %q must not be queried once the primary has already failed fatally", dir)
		return "", nil
	}

	var stdout, stderr bytes.Buffer
	code := doBdListFanOut(cfg, cityDir, []string{"list", "--json"}, primary, &stdout, &stderr, run)
	if code != bdSilentFallbackExitCode {
		t.Fatalf("doBdListFanOut() = %d, want %d (bdSilentFallbackExitCode) on a primary silent fallback", code, bdSilentFallbackExitCode)
	}
	if !strings.Contains(stderr.String(), bdSilentFallbackUserMessage) {
		t.Fatalf("stderr = %q, want it to contain bdSilentFallbackUserMessage", stderr.String())
	}
	if called != 1 {
		t.Fatalf("run called %d times, want 1 (primary only)", called)
	}
}

// TestDoBdListFanOutFederatedSilentFallbackWarnsAndSkips covers gcy-qu1d: a
// federated store's silent fallback must not fail the whole call (primary's
// results still merge), but — unlike a generic federated error, which is a
// fully silent skip — it must surface a visible warning, since silently
// merging possibly-stale on-disk data as if authoritative is a data
// integrity concern the ordinary "store unreachable" skip isn't.
func TestDoBdListFanOutFederatedSilentFallbackWarnsAndSkips(t *testing.T) {
	cityDir := t.TempDir()
	cfg := &config.City{Rigs: []config.Rig{{Name: "alpha", Path: filepath.Join(cityDir, "alpha"), Prefix: "al"}}}
	primary := bdCityScopeTarget(cityDir, cfg)

	run := func(_ []string, dir string, _ []string, _ io.Writer) (string, error) {
		if dir == cityDir {
			return `[{"id":"city-1"}]`, nil
		}
		return "", errBdListFanOutSilentFallback
	}

	var stdout, stderr bytes.Buffer
	code := doBdListFanOut(cfg, cityDir, []string{"list", "--json"}, primary, &stdout, &stderr, run)
	if code != 0 {
		t.Fatalf("doBdListFanOut() = %d, want 0 (a federated store's silent fallback must not fail the whole call); stderr=%q", code, stderr.String())
	}
	gotIDs := decodeIDs(t, stdout.Bytes())
	if !equalStrings(gotIDs, []string{"city-1"}) {
		t.Fatalf("merged ids = %v, want [city-1] (primary's results survive a federated silent fallback)", gotIDs)
	}
	if !strings.Contains(stderr.String(), bdSilentFallbackUserMessage) {
		t.Fatalf("stderr = %q, want a visible warning for the skipped federated store, not a fully silent skip", stderr.String())
	}
}

func TestDoBdListFanOutPrimaryUnparseableOutputPassesThrough(t *testing.T) {
	cityDir := t.TempDir()
	cfg := &config.City{Rigs: []config.Rig{{Name: "alpha", Path: filepath.Join(cityDir, "alpha"), Prefix: "al"}}}
	primary := bdCityScopeTarget(cityDir, cfg)

	const plainText = "id    title\ngc-1  example\n"
	called := 0
	run := func(_ []string, dir string, _ []string, _ io.Writer) (string, error) {
		called++
		if dir == cityDir {
			return plainText, nil
		}
		t.Fatalf("federated store %q must not be queried once the primary output failed to parse", dir)
		return "", nil
	}

	var stdout, stderr bytes.Buffer
	code := doBdListFanOut(cfg, cityDir, []string{"list", "--json"}, primary, &stdout, &stderr, run)
	if code != 0 {
		t.Fatalf("doBdListFanOut() = %d, want 0 (fall back to passthrough, not an error)", code)
	}
	if stdout.String() != plainText {
		t.Fatalf("stdout = %q, want unparsed primary output passed through verbatim %q", stdout.String(), plainText)
	}
	if called != 1 {
		t.Fatalf("run called %d times, want 1 (primary only)", called)
	}
}

func decodeIDs(t *testing.T, out []byte) []string {
	t.Helper()
	var rows []struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(out, &rows); err != nil {
		t.Fatalf("output did not decode as a JSON array: %v; out=%q", err, out)
	}
	ids := make([]string, len(rows))
	for i, r := range rows {
		ids[i] = r.ID
	}
	return ids
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	sort.Strings(a)
	sort.Strings(b)
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// ---- end-to-end: through doBd itself, with a real bd subprocess ----

func writeFanOutTestCity(t *testing.T, cityDir string, rigs []string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(cityDir, ".beads"), 0o700); err != nil {
		t.Fatal(err)
	}
	var rigsToml strings.Builder
	for _, r := range rigs {
		if err := os.MkdirAll(filepath.Join(cityDir, r, ".beads"), 0o700); err != nil {
			t.Fatal(err)
		}
		rigsToml.WriteString("\n[[rigs]]\nname = \"" + r + "\"\npath = \"" + r + "\"\nprefix = \"" + r[:2] + "\"\n")
	}
	cfg := "[workspace]\nname = \"demo\"\n" + rigsToml.String()
	if err := os.WriteFile(filepath.Join(cityDir, "city.toml"), []byte(cfg), 0o644); err != nil {
		t.Fatal(err)
	}
}

// fanOutFakeBdScript writes a fake `bd` that, on `list`, echoes one bead
// whose id is derived from GC_STORE_SCOPE/GC_RIG so each store's response is
// distinguishable, and appends an invocation-count line to CAPTURE_PATH.
func fanOutFakeBdScript(t *testing.T, binDir, capturePath string) {
	t.Helper()
	script := filepath.Join(binDir, "bd")
	if err := os.WriteFile(script, []byte(`#!/bin/sh
set -eu
echo "1" >> "${CAPTURE_PATH}"
scope="${GC_STORE_SCOPE:-}"
rig="${GC_RIG:-}"
id="city"
if [ "$scope" = "rig" ]; then
  id="$rig"
fi
for arg in "$@"; do
  if [ "$arg" = "--json" ]; then
    printf '[{"id":"%s-1"}]' "$id"
    exit 0
  fi
done
printf 'id    title\n%s-1  example\n' "$id"
`), 0o755); err != nil {
		t.Fatal(err)
	}
}

func TestDoBdListFanOutEndToEndAcrossConfiguredRigs(t *testing.T) {
	disableManagedDoltRecoveryForTest(t)
	origCityFlag, origRigFlag := cityFlag, rigFlag
	defer func() { cityFlag, rigFlag = origCityFlag, origRigFlag }()
	cityFlag, rigFlag = "", ""

	cityDir := t.TempDir()
	writeFanOutTestCity(t, cityDir, []string{"alpha", "beta"})
	// cwd deliberately outside the city entirely so resolution falls
	// through to the city default — the exact ambiguous-fallback shape
	// gcy-deki reported, not an explicit or inferred single-rig match.
	setCwd(t, t.TempDir())

	binDir := t.TempDir()
	capture := filepath.Join(t.TempDir(), "calls.txt")
	fanOutFakeBdScript(t, binDir, capture)
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("CAPTURE_PATH", capture)
	t.Setenv("GC_CITY_PATH", cityDir)
	t.Setenv("GC_RIG", "")

	var stdout, stderr bytes.Buffer
	if got := doBd([]string{"list", "--json"}, &stdout, &stderr); got != 0 {
		t.Fatalf("doBd() = %d, want 0; stderr=%q", got, stderr.String())
	}

	gotIDs := decodeIDs(t, stdout.Bytes())
	want := []string{"alpha-1", "beta-1", "city-1"}
	if !equalStrings(gotIDs, want) {
		t.Fatalf("merged ids = %v, want %v", gotIDs, want)
	}

	calls, err := os.ReadFile(capture)
	if err != nil {
		t.Fatal(err)
	}
	if got := len(strings.Split(strings.TrimSpace(string(calls)), "\n")); got != 3 {
		t.Fatalf("bd invoked %d times, want 3 (city + alpha + beta)", got)
	}
}

func TestDoBdListNoFanOutWithExplicitRig(t *testing.T) {
	disableManagedDoltRecoveryForTest(t)
	origCityFlag, origRigFlag := cityFlag, rigFlag
	defer func() { cityFlag, rigFlag = origCityFlag, origRigFlag }()
	cityFlag, rigFlag = "", ""

	cityDir := t.TempDir()
	writeFanOutTestCity(t, cityDir, []string{"alpha", "beta"})
	setCwd(t, t.TempDir())

	binDir := t.TempDir()
	capture := filepath.Join(t.TempDir(), "calls.txt")
	fanOutFakeBdScript(t, binDir, capture)
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("CAPTURE_PATH", capture)
	t.Setenv("GC_CITY_PATH", cityDir)
	t.Setenv("GC_RIG", "")

	var stdout, stderr bytes.Buffer
	if got := doBd([]string{"--rig", "alpha", "list", "--json"}, &stdout, &stderr); got != 0 {
		t.Fatalf("doBd() = %d, want 0; stderr=%q", got, stderr.String())
	}

	gotIDs := decodeIDs(t, stdout.Bytes())
	if !equalStrings(gotIDs, []string{"alpha-1"}) {
		t.Fatalf("merged ids = %v, want [alpha-1] only — an explicit --rig must not fan out", gotIDs)
	}
	calls, err := os.ReadFile(capture)
	if err != nil {
		t.Fatal(err)
	}
	if got := len(strings.Split(strings.TrimSpace(string(calls)), "\n")); got != 1 {
		t.Fatalf("bd invoked %d times, want 1 (explicit --rig must stay single-store)", got)
	}
}

func TestDoBdListNoFanOutWithoutJSON(t *testing.T) {
	disableManagedDoltRecoveryForTest(t)
	origCityFlag, origRigFlag := cityFlag, rigFlag
	defer func() { cityFlag, rigFlag = origCityFlag, origRigFlag }()
	cityFlag, rigFlag = "", ""

	cityDir := t.TempDir()
	writeFanOutTestCity(t, cityDir, []string{"alpha", "beta"})
	setCwd(t, t.TempDir())

	binDir := t.TempDir()
	capture := filepath.Join(t.TempDir(), "calls.txt")
	fanOutFakeBdScript(t, binDir, capture)
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("CAPTURE_PATH", capture)
	t.Setenv("GC_CITY_PATH", cityDir)
	t.Setenv("GC_RIG", "")

	var stdout, stderr bytes.Buffer
	if got := doBd([]string{"list"}, &stdout, &stderr); got != 0 {
		t.Fatalf("doBd() = %d, want 0; stderr=%q", got, stderr.String())
	}
	if !strings.Contains(stdout.String(), "city-1") {
		t.Fatalf("stdout = %q, want the single (non-fanned-out) city store's plain-text output", stdout.String())
	}
	calls, err := os.ReadFile(capture)
	if err != nil {
		t.Fatal(err)
	}
	if got := len(strings.Split(strings.TrimSpace(string(calls)), "\n")); got != 1 {
		t.Fatalf("bd invoked %d times, want 1 (no --json means no fan-out)", got)
	}
}

// fanOutFakeBdScriptWithSilentFallback writes a fake `bd` that behaves like
// fanOutFakeBdScript, except when the target store's derived id equals
// fallbackID: it still exits 0 with valid JSON on stdout, but also emits
// bd's real silent-fallback marker pair to stderr — exercising
// runBdListFanOut's actual subprocess stderr-tee-and-scan path end to end,
// not just the injectable mock the tests above use.
func fanOutFakeBdScriptWithSilentFallback(t *testing.T, binDir, capturePath, fallbackID string) {
	t.Helper()
	script := filepath.Join(binDir, "bd")
	if err := os.WriteFile(script, []byte(`#!/bin/sh
set -eu
echo "1" >> "${CAPTURE_PATH}"
scope="${GC_STORE_SCOPE:-}"
rig="${GC_RIG:-}"
id="city"
if [ "$scope" = "rig" ]; then
  id="$rig"
fi
if [ "$id" = "`+fallbackID+`" ]; then
  echo "auto-importing into empty database" >&2
fi
for arg in "$@"; do
  if [ "$arg" = "--json" ]; then
    printf '[{"id":"%s-1"}]' "$id"
    exit 0
  fi
done
printf 'id    title\n%s-1  example\n' "$id"
`), 0o755); err != nil {
		t.Fatal(err)
	}
}

// TestDoBdListFanOutEndToEndPrimarySilentFallbackIsFatal covers gcy-qu1d
// through a real bd subprocess (not the injectable mock): the primary
// store's stderr showing bd's real silent-fallback marker must fail the
// whole call with bdSilentFallbackExitCode, proving runBdListFanOut's
// stderr tee-and-scan actually works, not just its unit-level contract.
func TestDoBdListFanOutEndToEndPrimarySilentFallbackIsFatal(t *testing.T) {
	disableManagedDoltRecoveryForTest(t)
	origCityFlag, origRigFlag := cityFlag, rigFlag
	defer func() { cityFlag, rigFlag = origCityFlag, origRigFlag }()
	cityFlag, rigFlag = "", ""

	cityDir := t.TempDir()
	writeFanOutTestCity(t, cityDir, []string{"alpha", "beta"})
	// cwd outside the city so resolution falls through to the city default —
	// the same ambiguous-fallback shape TestDoBdListFanOutEndToEndAcrossConfiguredRigs
	// uses, which makes "city" the primary target.
	setCwd(t, t.TempDir())

	binDir := t.TempDir()
	capture := filepath.Join(t.TempDir(), "calls.txt")
	fanOutFakeBdScriptWithSilentFallback(t, binDir, capture, "city")
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("CAPTURE_PATH", capture)
	t.Setenv("GC_CITY_PATH", cityDir)
	t.Setenv("GC_RIG", "")

	var stdout, stderr bytes.Buffer
	got := doBd([]string{"list", "--json"}, &stdout, &stderr)
	if got != bdSilentFallbackExitCode {
		t.Fatalf("doBd() = %d, want %d (bdSilentFallbackExitCode); stderr=%q", got, bdSilentFallbackExitCode, stderr.String())
	}
	if !strings.Contains(stderr.String(), bdSilentFallbackUserMessage) {
		t.Fatalf("stderr = %q, want bdSilentFallbackUserMessage", stderr.String())
	}
}

// TestDoBdListFanOutEndToEndFederatedSilentFallbackWarnsAndSkips covers
// gcy-qu1d through a real bd subprocess: a federated (non-primary) store's
// real silent-fallback marker must not fail the whole call, but must
// forward a visible warning and exclude that store's results from the merge.
func TestDoBdListFanOutEndToEndFederatedSilentFallbackWarnsAndSkips(t *testing.T) {
	disableManagedDoltRecoveryForTest(t)
	origCityFlag, origRigFlag := cityFlag, rigFlag
	defer func() { cityFlag, rigFlag = origCityFlag, origRigFlag }()
	cityFlag, rigFlag = "", ""

	cityDir := t.TempDir()
	writeFanOutTestCity(t, cityDir, []string{"alpha", "beta"})
	setCwd(t, t.TempDir())

	binDir := t.TempDir()
	capture := filepath.Join(t.TempDir(), "calls.txt")
	fanOutFakeBdScriptWithSilentFallback(t, binDir, capture, "beta")
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("CAPTURE_PATH", capture)
	t.Setenv("GC_CITY_PATH", cityDir)
	t.Setenv("GC_RIG", "")

	var stdout, stderr bytes.Buffer
	got := doBd([]string{"list", "--json"}, &stdout, &stderr)
	if got != 0 {
		t.Fatalf("doBd() = %d, want 0 (a federated store's silent fallback must not fail the whole call); stderr=%q", got, stderr.String())
	}
	gotIDs := decodeIDs(t, stdout.Bytes())
	want := []string{"alpha-1", "city-1"}
	if !equalStrings(gotIDs, want) {
		t.Fatalf("merged ids = %v, want %v (beta's results skipped, others survive)", gotIDs, want)
	}
	if !strings.Contains(stderr.String(), bdSilentFallbackUserMessage) {
		t.Fatalf("stderr = %q, want a visible warning for the skipped federated store", stderr.String())
	}
}
