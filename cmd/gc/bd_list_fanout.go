package main

import (
	"encoding/json"
	"fmt"
	"io"
	"os/exec"
	"strconv"
	"strings"

	"github.com/gastownhall/gascity/internal/config"
)

// bdListDefaultLimit mirrors bd list's own documented default (`bd list
// --help`: "-n, --limit int Limit results (default 50, use 0 for
// unlimited)").
const bdListDefaultLimit = 50

// bdListFanOutRunner runs `bd <args...>` against one store's dir and env and
// returns its stdout. Injectable so doBdListFanOut's merge/limit logic can be
// tested without a real bd subprocess — mirrors hookStoreRunner
// (hook_cross_store.go).
type bdListFanOutRunner func(args []string, dir string, env []string) (string, error)

// bdListShouldFanOut reports whether a `gc bd` invocation is eligible for
// cross-store read federation.
//
// Why this exists (gcy-deki): resolveBdScopeTarget picks exactly one store
// per call, using GC_RIG env or cwd as its best guess when no explicit scope
// is given. `gc bd show <id>` and `gc hook` both route around a wrong guess —
// show via the bead-ID prefix in its positional arg, hook by federating
// across every configured store outright (hook_cross_store.go). `gc bd list`
// has no positional bead ID to route by (every list filter is a flag), so an
// inferred guess that lands on the wrong store leaves list blind to real
// data show/hook can both still find directly.
//
// Scope is deliberately narrow: only the read-only, mergeable case. An
// explicit --rig/--city/-C pin is a deliberate single-store choice and is
// left exactly as resolveBdScopeTarget already handles it; --format changes
// the output shape to something a flat JSON-array merge can't safely handle.
func bdListShouldFanOut(rigName string, cityExplicit bool, bdArgs []string) bool {
	if len(bdArgs) == 0 || bdArgs[0] != "list" {
		return false
	}
	if rigName != "" || cityExplicit {
		return false
	}
	if extractBdDirectoryFlag(bdArgs) != "" {
		return false
	}
	if !bdArgsHasJSONFlag(bdArgs) {
		return false
	}
	if bdArgsHasFormatFlag(bdArgs) {
		return false
	}
	return true
}

func bdArgsHasJSONFlag(args []string) bool {
	for _, a := range args {
		if a == "--json" {
			return true
		}
	}
	return false
}

func bdArgsHasFormatFlag(args []string) bool {
	for _, a := range args {
		if a == "--format" || strings.HasPrefix(a, "--format=") {
			return true
		}
	}
	return false
}

// bdListRequestedLimit returns the effective --limit value bd would apply to
// a `list` call: the explicit value if given (0 meaning unlimited, per bd's
// own semantics), or bd's own default of 50 when no --limit/-n flag is
// present or its value fails to parse (bd itself owns rejecting a malformed
// value; this only needs a safe number to truncate the merge by).
func bdListRequestedLimit(args []string) (limit int, unlimited bool) {
	for i := 0; i < len(args); i++ {
		a := args[i]
		var raw string
		switch {
		case a == "-n" || a == "--limit":
			if i+1 >= len(args) {
				return bdListDefaultLimit, false
			}
			raw = args[i+1]
		case strings.HasPrefix(a, "--limit="):
			raw = strings.TrimPrefix(a, "--limit=")
		default:
			continue
		}
		n, err := strconv.Atoi(raw)
		if err != nil {
			return bdListDefaultLimit, false
		}
		if n == 0 {
			return 0, true
		}
		return n, false
	}
	return bdListDefaultLimit, false
}

// bdFanOutTargets returns every store a fanned-out list query should reach:
// the already-resolved primary target first (so its results sort first and
// its errors stay authoritative per doBdListFanOut), then every other
// configured, bound rig, then the city — each exactly once, deduplicated by
// ScopeRoot.
func bdFanOutTargets(cfg *config.City, cityPath string, primary execStoreTarget) []execStoreTarget {
	targets := []execStoreTarget{primary}
	seen := map[string]bool{primary.ScopeRoot: true}
	if cfg == nil {
		return targets
	}
	resolveRigPaths(cityPath, cfg.Rigs)
	for i := range cfg.Rigs {
		rig := cfg.Rigs[i]
		if strings.TrimSpace(rig.Path) == "" {
			continue
		}
		t := bdRigScopeTarget(cityPath, rig)
		if seen[t.ScopeRoot] {
			continue
		}
		seen[t.ScopeRoot] = true
		targets = append(targets, t)
	}
	cityTarget := bdCityScopeTarget(cityPath, cfg)
	if !seen[cityTarget.ScopeRoot] {
		targets = append(targets, cityTarget)
	}
	return targets
}

// doBdListFanOut runs bdArgs (a `list --json` invocation already gated by
// bdListShouldFanOut) against every store bdFanOutTargets returns and merges
// the results into one JSON array, truncated to bd's own --limit semantics.
//
// The primary target's env/subprocess/parse error is fatal, matching doBd's
// existing single-store contract for the store its caller was already going
// to land on (doBd's own bd-store-contract gate has also already run against
// the primary before this is called — see cmd_bd.go). A federated
// (non-primary) store's incompatible provider, error, or unparseable output
// is skipped best-effort — mirroring firstStoreWithWork's federated-store
// handling in hook_cross_store.go — so one unreachable or differently
// provisioned rig store can't wedge an otherwise-working list call.
//
// Not attempted: reproducing bd's own --sort ordering across merged stores.
// Results are concatenated in target order (primary, then configured rigs,
// then city); --limit truncates that concatenation rather than a globally
// re-sorted merge. Good enough for the enumerate-everything callers this
// fixes (gcy-deki's witness reconciliation query), but a caller relying on
// cross-store sort order should pin an explicit --rig instead.
func doBdListFanOut(cfg *config.City, cityPath string, bdArgs []string, primary execStoreTarget, stdout, stderr io.Writer, run bdListFanOutRunner) int {
	limit, unlimited := bdListRequestedLimit(bdArgs)
	merged := make([]json.RawMessage, 0)

	for i, target := range bdFanOutTargets(cfg, cityPath, primary) {
		fatal := i == 0
		if !fatal {
			if provider := rawBeadsProviderForScope(target.ScopeRoot, cityPath); !providerUsesBdStoreContract(provider) {
				continue
			}
		}
		env, err := bdCommandEnv(cityPath, cfg, target)
		if err != nil {
			if fatal {
				fmt.Fprintf(stderr, "gc bd: %v\n", err) //nolint:errcheck // best-effort stderr
				return 1
			}
			continue
		}
		out, err := run(bdArgs, target.ScopeRoot, env)
		if err != nil {
			if fatal {
				fmt.Fprintf(stderr, "gc bd: %v\n", err) //nolint:errcheck // best-effort stderr
				return 1
			}
			continue
		}
		var rows []json.RawMessage
		if jsonErr := json.Unmarshal([]byte(out), &rows); jsonErr != nil {
			if fatal {
				// Primary output didn't parse as a flat JSON array — the
				// --json assumption this fan-out relies on doesn't hold for
				// this invocation (unexpected bd version/output shape).
				// Pass it through unmerged rather than guess at federation.
				fmt.Fprint(stdout, out) //nolint:errcheck // best-effort stdout
				return 0
			}
			continue
		}
		merged = append(merged, rows...)
		if !unlimited && len(merged) >= limit {
			merged = merged[:limit]
			break
		}
	}

	encoded, err := json.Marshal(merged)
	if err != nil {
		fmt.Fprintf(stderr, "gc bd: encoding merged list output: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}
	fmt.Fprint(stdout, string(encoded)) //nolint:errcheck // best-effort stdout
	return 0
}

// runBdListFanOut is the production bdListFanOutRunner: a real bd subprocess
// against one store, matching doBd's own single-target exec.Command setup
// (bd resolved from PATH, cmd.Dir/cmd.Env pinned to the target store).
func runBdListFanOut(args []string, dir string, env []string) (string, error) {
	bdPath, err := exec.LookPath("bd")
	if err != nil {
		return "", fmt.Errorf("bd not found in PATH")
	}
	cmd := exec.Command(bdPath, args...)
	cmd.Dir = dir
	cmd.Env = workQueryEnvForDir(env, dir)
	out, err := cmd.Output()
	if err != nil {
		return "", err
	}
	return string(out), nil
}
