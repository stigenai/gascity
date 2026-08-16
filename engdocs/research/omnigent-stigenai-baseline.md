# Omnigent integration baseline

**Status:** Active research baseline

**Bead:** `ga-ou2.1.7`
**Captured:** 2026-08-15
**Release refresh:** 2026-08-15

This note fixes the repository baseline and ownership assumptions for the
Omnigent integration. It is evidence for the architecture work, not a promise
that every current seam is the final implementation seam.

## Repository identities

| Name | Repository | Role | Captured revision |
|---|---|---|---|
| `origin` | `github.com/stigenai/gascity` | Integration and delivery target | `b0f06c135f3aa52f747178e3f6a269054b7bd7a6` |
| `upstream` | `github.com/gastownhall/gascity` | Upstream mergeability reference | `126029e5a3a7c4f71a29e3aa9691c272955d64c1` |
| merge base | both repositories | Last shared mainline revision | `dcee9b82ff0c3f12a8b3540e13a09ed92a209b0d` |
| integration branch | `codex/omniagent-integration` | Published Omnigent work branch | `8908589c81964ba5c161571f0425cec75efd8112` |
| reviewed Omnigent pin | `github.com/omnigent-ai/omnigent` | Executable and local API contract implemented by this branch | `2aba5079d4d3a2a84d8c9927884fc4b8ce0eeecc` |
| current Omnigent `main` | `github.com/omnigent-ai/omnigent` | Upgrade-audit reference only | `901aa8d12a2c7e51764d6d14269fc4576ec77c07` |

At the release refresh, `origin/main...upstream/main` reported 115 commits only
on the StigenAI side and 310 commits only on the upstream side. The integration
branch reported two commits ahead of `origin/main` and zero behind: the
pre-existing managed-checkout `.gitignore` commit plus the Omnigent integration
checkpoint. The pre-existing commit is unrelated to Omnigent and must remain
intact.

The Omnigent revision in the contract is deliberately still `2aba5079...`.
Although Omnigent `main` has advanced to `901aa8d...`, that newer revision has
not passed the compatibility, locality, and live-profile audit required to
change the pin. A release refresh must not turn an observed upstream head into
an executable upgrade.

## Release-time upstream overlap

Since the original upstream capture at `b4ef85b8...`, upstream added 12 commits
touching 110 files. The integration branch touches 104 files relative to
`origin/main`. The exact path intersection is four files:

| Path | Upstream change | Integration change | Landing treatment |
|---|---|---|---|
| `TESTING.md` | Test inventory and resource-count maintenance | Omnigent test lanes and resource expectations | Preserve both descriptions; rerun the routed test and resource manifests after a real upstream merge. |
| `cmd/gc/city_runtime.go` | Event-fed route-recovery and graph-completion lanes, plus detached-handoff recovery | Managed-Dolt preflight ordering and durable async-start journal sweep ordering | Semantically separate edits in one lifecycle file; resolve structurally and rerun startup, tick, order-dispatch, and async-cleanup tests. |
| `internal/testpolicy/resourcecensus/census.go` | Upstream subprocess baseline changes | Omnigent listener/process baselines and cleanup coverage | Regenerate from the merged executable tree; do not hand-add numeric baselines. |
| `test/test-resources.toml` | Upstream subprocess ledger changes | Omnigent listener/process ledger changes | Regenerate with the census and verify the manifest as one unit. |

The newest upstream commit, `126029e5a`, only raises the hook work-query timeout
and does not add another exact overlap with the integration branch. The four
overlaps above are therefore the complete release-time collision set, not a
sample. `origin/main` remains at the revision used to build the branch, so no
StigenAI rebase is required before the remaining release scorecard. A future
upstream merge still requires the explicit treatment above; path-count evidence
alone is not proof that the branches merge safely.

## Relevant fork history

StigenAI already owns the deepest runtime-specific integration needed by this
work: the production herdr provider. The current fork-only history includes:

- `1f696d7a9` — initial herdr provider;
- `92687e725`, `22a14b783`, `2d7b33d70`, `889370f36` — the herdr 0.7.5
  pane-shell rewrite, provisional binding, name mapping, and exited-pane reaping;
- `8afc675c9`, `f2fecf5d4`, `18ec726de`, `8749838de` — activity and stale-registry
  liveness fences;
- `7c98f09cd` — provider-native session identity persistence;
- `a46030068` — retaining herdr as the hybrid local backend;
- `0c2b91e5f` — fail-closed herdr runtime uncertainty.

Relative to the shared merge base, the fork's herdr surface is concentrated in
`internal/runtime/herdr/` plus the small registration seam in
`cmd/gc/runtime_registry.go`. The scoped diff adds roughly 3,480 lines and
removes roughly 260 lines, mostly tests and fork-owned provider files. This is
the correct ownership boundary to preserve: Omnigent composition should reuse
herdr rather than duplicate or replace it.

Upstream has relevant post-divergence fixes that the integration must evaluate
before landing:

- `6fd8f97c4` — herdr 0.7.4/0.7.5 liveness and launch hardening;
- `24bb1b70c` — confirmed first-turn submission and swallowed-submit recovery;
- `fb4c42530` — worker transcript metadata retry;
- newer runtime conformance and session-lifecycle changes visible under
  `internal/runtime/`, `internal/worker/`, and `cmd/gc/`.

These are candidates for a normal fork sync, not code to copy into an Omnigent
adapter. Before porting any slice, compare it with the StigenAI equivalents and
prefer the smallest proven change.

## Ownership boundaries

| Concern | Owner for this integration | Existing seam |
|---|---|---|
| Formulas, beads, routing, retries, pools | Gas City | orchestrator and domain packages |
| Rig, checkout, worktree, working directory | Gas City | rig and worker staging |
| Runtime placement and visible terminal | Gas City | herdr first, tmux fallback |
| Omnigent daemon lifecycle | Gas City | city-scoped `[[service]]` supervision |
| Harness, model, auth, tool policy | Omnigent | pinned local API/config contract |
| Conversation and harness-specific lifecycle | Omnigent | opaque conversation identifier |
| Profile failover | Omnigent | ordered, sticky profile chain |
| Remote placement | Gas City | existing runtime concepts; out of the Omnigent MVP |
| Durable session projection | Gas City | `worker.Handle` and session beads |

Existing `[[service]]` support is city-scoped and already provides supervised
long-lived process infrastructure. Existing agent providers carry harness/model
configuration. Existing herdr and tmux providers own terminal placement and
interaction. The architecture phase must prove that composing those seams is
insufficient before adding a new primitive or an Omnigent runtime provider.

## Sync procedure

Run this before implementation starts and again before landing:

```bash
git fetch --prune origin
git fetch --prune upstream
git status --porcelain=v1
git rev-parse HEAD origin/main upstream/main
git merge-base origin/main upstream/main
git rev-list --left-right --count HEAD...origin/main
git rev-list --left-right --count origin/main...upstream/main
git log --oneline upstream/main..origin/main -- \
  internal/runtime/herdr internal/config internal/worker cmd/gc
git log --oneline origin/main..upstream/main -- \
  internal/runtime/herdr internal/config internal/worker cmd/gc
```

With a clean worktree, rebase the integration branch onto `origin/main`. Resolve
conflicts in favor of newer StigenAI behavior unless the Omnigent design records
a tested reason to change it. Inspect the final fork delta with scoped diffs and
`git range-diff`, rerun all affected gates, then push the integration branch to
`origin`. Never push this work to `upstream`, and never run a beads Dolt remote
operation: this repository's beads store is local-only.

## Current risks

- The fork and upstream are substantially divergent, so an unscoped upstream
  merge is not an implementation shortcut.
- Herdr is a high-value, fork-owned integration surface with extensive
  regression history. Omnigent must compose with it without changing herdr into
  a conversation owner.
- `[[service]]` is HTTP-oriented today. The Omnigent contract audit must verify
  whether its local server fits that lifecycle directly or needs a small
  fork-owned adapter.
- The current integration branch contains one unrelated but intentional
  `.gitignore` commit. Subsequent commits must not fold Omnigent work into it.
