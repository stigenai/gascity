#!/usr/bin/env bash
# gate-sweep — evaluate and close pending gates.
#
# Runs as an exec order (no LLM, no agent, no wisp). bd dispatches per
# type. The `|| true` on the gh-gate line is load-bearing: bd shells
# out to `gh` for gh:run / gh:pr gates, and fresh cities without
# `gh auth` would otherwise fail this order on every 30s cooldown.
# bd's combined output reaches the controller log only on non-zero
# exit (see the `if err != nil` branch of `dispatchOne` in
# cmd/gc/order_dispatch.go), so suppressing gh-gate errors also
# hides real bd errors on that line — diagnose by hand.
#
# Timer-gate evaluation is local-only (no `gh` shell-out, no auth
# requirement) so its failures should propagate to the controller log.
# `|| true` would silently mask real bd regressions in timer-gate
# evaluation — see #1734 for the rationale.
#
# Bead-type gates are skipped: in beads v1.0.2, checkBeadGate is
# hard-coded to fail because cross-rig routing was removed upstream.
# Restore `gc bd gate check --type=bead --escalate` when beads adds it back.
#
# Cross-rig (gt-15s): gh:pr / gh:run / timer gate beads live in the PER-RIG
# stores — agents create them there — but this order is discovered at city
# scope (core-pack orders are scanned at the city root in
# orderdiscovery.ScanAll, so the order's Scope field cannot fan it out per
# rig). A bare `gc bd gate check` is HQ-scoped and sees none of the per-rig
# gates, so a merged PR's gate stays OPEN forever and the work bead stays
# blocked. Walk HQ + every non-HQ rig explicitly, the way
# renudge-stale-human-gates.sh does. `--rig` is a gc flag (not a bd flag),
# so it routes through `gc bd`.
set -euo pipefail

# Trace bd invocations to $GC_BD_TRACE when set (no-op otherwise).
__SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck disable=SC1091
. "$__SCRIPT_DIR/_bd_trace.sh" "gate-sweep"

CITY="${GC_CITY:-.}"
PACK_STATE_DIR="${GC_PACK_STATE_DIR:-${GC_CITY_RUNTIME_DIR:-$CITY/.gc/runtime}/packs/core}"
mkdir -p "$PACK_STATE_DIR"

# Build the list of scopes to sweep: HQ (empty scope, bare gc bd) plus every
# non-HQ rig. `gc bd gate check` without --rig is HQ-scoped from the city cwd,
# so per-rig gates are invisible to a bare query — walk each rig explicitly
# (gt-15s). The HQ entry is excluded from `gc rig list` (it reports the city
# root as an hq=true pseudo-rig), matching renudge-stale-human-gates.sh. jq
# missing, `gc rig list --json` failing, and it returning nothing usable are
# all best-effort: each falls back to HQ-only, which is the pre-gt-15s
# behavior (no regression) — but each must also say so on stderr (gcy-dgk),
# not silently revert to a partial sweep.
SCOPES_FILE="$(mktemp "$PACK_STATE_DIR/.gate-sweep-scopes.XXXXXX")"
trap 'rm -f "$SCOPES_FILE"' EXIT
printf '\n' > "$SCOPES_FILE" # HQ scope: an empty line
if command -v jq >/dev/null 2>&1; then
    RIGS_JSON="$(gc rig list --json 2>/dev/null)" && RIG_LIST_OK=1 || RIG_LIST_OK=0
    RIG_NAMES=""
    # PARSE_OK stays 0 (pessimistic) unless gc rig list --json actually
    # succeeded with usable output AND jq parsed it cleanly -- a healthy
    # zero-non-HQ-rig city (gcy-gec) is the ONLY way to reach PARSE_OK=1
    # with an empty RIG_NAMES.
    PARSE_OK=0
    if [ "$RIG_LIST_OK" -eq 1 ] && [ -n "$RIGS_JSON" ]; then
        RIG_NAMES="$(printf '%s' "$RIGS_JSON" \
            | jq -r '(.rigs // [])[] | select(.hq != true) | .name' 2>/dev/null)" && PARSE_OK=1 || PARSE_OK=0
    fi
    if [ -n "$RIG_NAMES" ]; then
        printf '%s\n' "$RIG_NAMES" >> "$SCOPES_FILE"
    elif [ "$PARSE_OK" -eq 1 ]; then
        # gc rig list --json succeeded and jq parsed it cleanly -- zero
        # non-HQ rigs is a legitimate, healthy state (fresh gc city init, or
        # a deliberately HQ-only deployment), not a degraded fallback
        # (gcy-gec). Distinct low-noise line, not the failure-shaped one
        # below, so a real gc rig list failure stays diagnosable.
        echo "gate-sweep: city has no non-HQ rigs registered; sweeping HQ only" >&2
    else
        echo "gate-sweep: gc rig list --json returned no usable rigs; sweeping HQ only" >&2
    fi
else
    echo "gate-sweep: jq not found in PATH; sweeping HQ only (per-rig gates will not resolve)" >&2
fi

# A scope with a resolved-but-merged gate and a scope with no gates at all
# both cost a gate-list round-trip; gate-sweep's 30s cadence is what makes a
# merged PR's gate drop within one tick, so we accept the per-scope fan-out.
FAILED=0
while IFS= read -r scope; do
    RIG_ARG1=""
    RIG_ARG2=""
    if [ -n "$scope" ]; then
        RIG_ARG1="--rig"
        RIG_ARG2="$scope"
    fi
    # Isolated per scope (gcy-vb9): under set -e, one scope's timer-gate
    # failure would otherwise abort every later-ordered scope's checks for
    # this tick. Still loud-fails per #1734 (logged here, exit-1'd after the
    # loop) instead of a blanket `|| true` that would silently mask a real
    # bd regression.
    if ! gc bd gate check ${RIG_ARG1:+"$RIG_ARG1" "$RIG_ARG2"} --type=timer --escalate; then
        echo "gate-sweep: FAILED timer-gate check for scope '${scope:-HQ}' (will retry next sweep)" >&2
        FAILED=$((FAILED + 1))
    fi
    gc bd gate check ${RIG_ARG1:+"$RIG_ARG1" "$RIG_ARG2"} --type=gh --escalate || true
done < "$SCOPES_FILE"

if [ "$FAILED" -gt 0 ]; then
    echo "gate-sweep: $FAILED scope(s) failed timer-gate check (see above; will retry next sweep)" >&2
    exit 1
fi
