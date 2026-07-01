# GraphOnlyReadyStore Rollup Deploy

Owner: `gascity/pm`
Created: 2026-07-01
Implementation root: `ga-ifavnc`
Failed deploy bead: `ga-oeu196`
Replacement deploy bead: `ga-ifavnc.5`

## Goal

Ship the complete GraphOnlyReadyStore implementation as one clean reviewed
release unit after the implementation stack is green.

The earlier deploy gate, `ga-oeu196`, targeted the contract-test commit
`370ef0b49e3fe9fdab9cbebe17c5e0d428615809`. Deployer rejected that candidate
because the tests are intentionally red until `ga-ifavnc.2`, `ga-ifavnc.3`,
and `ga-ifavnc.4` land, and because the branch contained prior production
commits outside the reviewed contract-test scope.

## Decision

Do not deploy the contract-tests-only branch.

Queue one replacement deploy lane for the complete GraphOnlyReadyStore stack.
The deploy bead remains blocked on `ga-ifavnc.4`, which already depends on
`ga-ifavnc.3`, which depends on `ga-ifavnc.2`, which depends on the closed
contract-coverage bead `ga-ifavnc.1`.

## Work Packages

| Bead | Route | Label | Acceptance focus |
| --- | --- | --- | --- |
| `ga-ifavnc.5` | `gascity/deployer` | `needs-deploy` | Gate a clean, reviewed rollup branch containing the contract coverage plus the DoltliteReadStore, CachingStore, and beadPolicyStore implementations; open or update a scoped PR only on PASS. |

## Dependency Graph

```text
ga-ifavnc.1
  -> ga-ifavnc.2
      -> ga-ifavnc.3
          -> ga-ifavnc.4
              -> ga-ifavnc.5
```

## Deployer Acceptance

- Start only after the full implementation stack has reviewer PASS for the
  complete GraphOnlyReadyStore scope.
- Do not reuse `370ef0b49e3fe9fdab9cbebe17c5e0d428615809` as the deploy unit.
- Gate a clean candidate based on current `origin/main`.
- Confirm the candidate includes the contract coverage and all three
  implementation layers, with no unrelated prior production commits or stale
  release-gate artifacts.
- Run the standard deploy gate, including the native build/test/vet checks
  required by `ga-ifavnc.4`.
- On PASS, open or update a GraphOnlyReadyStore-scoped PR and route merge
  authority to mayor/mpr only.
- On FAIL, append the failed criteria and artifact path to `ga-ifavnc.5`, then
  route back to PM with the exact blocker.

## Out Of Scope

- PM-authored implementation, tests, branch surgery, PR approval, or merge.
- Deploying contract coverage before the implementation stack is green.
- Bundling unrelated production changes into the GraphOnlyReadyStore deploy
  candidate.
