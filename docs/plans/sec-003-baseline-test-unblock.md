# SEC-003 Baseline Test Unblock

Owner: `gascity/pm`
Created: 2026-06-09
Source beads: `ga-7psli2`, `ga-x2i1lv.2`

## Goal

Unblock the SEC-003 clean-branch deploy without bundling unrelated baseline
test work into the SEC-003 release unit.

## Context

The deployer failed `ga-7psli2` on the standard release gate for
`release/sec-003-ga-x2i1lv` in `/home/jaword/projects/beads`.

SEC-003 candidate evidence:

- Base: `9a1c88b63aee89b091c9db7e5330a48cb4911987`
- Head: `17c63de541f3ee16147a904f79634c4ec53555f4`
- Diff: `internal/beads/context.go` only, `+6/-1`
- Passing checks before the blocker: build, vet, targeted SEC-003 test, and
  HOME-mismatch smoke
- Failing check: `make test` exits 2 in
  `cmd/bd TestOutputContextFunction CLI_Stealth` with unexpected `git status`
  text

The deployer also reported that the same focused failure reproduces on clean
`origin/main` at `9a1c88b63aee89b091c9db7e5330a48cb4911987`, so this is an
unrelated baseline blocker. SEC-003 should wait until the baseline blocker is
fixed or otherwise cleared, then deploy as a clean single-feature release.

## Work Packages

| Bead | Route | Label | Acceptance focus |
| --- | --- | --- | --- |
| `ga-7psli2.1` | `gascity/builder` | `ready-to-build` | Resolve or objectively clear the baseline `cmd/bd` `CLI_Stealth` failure without bundling SEC-003. |
| `ga-7psli2.2` | `gascity/validator` | `needs-tests` | Independently validate the blocker resolution and confirm no SEC-003 contamination. |
| `ga-7psli2.3` | `gascity/deployer` | `needs-deploy` | Retry the SEC-003 release gate only after the baseline blocker is cleared. |

## Dependency Graph

```text
ga-7psli2.1 -> ga-7psli2.2 -> ga-7psli2.3 -> ga-x2i1lv.2
```

The original clean-branch deploy bead remains blocked by the retry bead so the
SEC-003 PR cannot be opened while the unrelated baseline failure is unresolved.

## Acceptance Summary

1. Builder proves the `cmd/bd TestOutputContextFunction CLI_Stealth` blocker is
   independent of SEC-003 and resolves it as a separate release unit, or records
   an objective no-code resolution.
2. Validator confirms the focused stealth coverage no longer leaks
   `git status` text and that the blocker fix does not include the SEC-003
   home-directory change.
3. Deployer reruns the SEC-003 gate from a current clean `origin/main` base
   after the baseline blocker is cleared.
4. Deployer opens and routes a PR only if the standard gate passes; on failure,
   deployer records exact failed criteria and artifact path and routes back to
   PM.

## Tracker Import

No `tracker-to-beads` skill is installed in this PM worktree, so tracker import
is a no-op for this package.
