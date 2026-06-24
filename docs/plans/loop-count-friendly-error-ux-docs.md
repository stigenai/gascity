# loop.count Friendly Error UX and Docs Plan

Root bead: `ga-sdv68f`  
Source: designer handoff for decision `ga-n0j310` Option B

## Goal

When a formula author writes a string or template expression in `loop.count`,
they receive an actionable parse error instead of an internal JSON decode error.
The tutorial should also make the integer-only rule and `range` alternative easy
to find.

## Work Packages

1. `ga-sdv68f.1` - Add regression coverage for loop.count string parse errors

   Acceptance:
   - A parser or unmarshal regression test exercises `[steps.loop] count = "{{cups}}"`.
   - The error semantics include `integer` and `range`.
   - The test lives near existing formula parsing or unmarshal coverage.
   - The test does not over-constrain exact punctuation or full prose.

2. `ga-sdv68f.3` - Implement friendly loop.count error for string and template values

   Acceptance:
   - String-valued `loop.count` is detected before the opaque JSON decode path.
   - The parse error says `loop.count` must be an integer.
   - The message points users to `range = "1..{n}"` with `var = "n"` for variable-driven counts.
   - The message uses single-brace range substitution, not `{{n}}`.
   - Valid integer counts continue to parse unchanged.

3. `ga-sdv68f.2` - Document loop.count integer-only rule in formula tutorial

   Acceptance:
   - `docs/tutorials/05-formulas.md` gains a targeted `<Note>` immediately after the range/count explanation.
   - The note says `count` accepts an integer literal only.
   - The note says template variables such as `{{var}}` are not supported in `count`.
   - The note directs users to `range = "1..{n}"` with `var = "n"`.

## Dependency Order

`ga-sdv68f.1` blocks `ga-sdv68f.3`; `ga-sdv68f.3` blocks `ga-sdv68f.2`.

This keeps the downstream build sequence aligned with TDD: coverage first,
behavior second, docs after the behavior language is settled.

## Out Of Scope

- Do not change `LoopSpec.Count`.
- Do not change the TOML alias count field type.
- Do not change loop validation sentinel behavior.
- Do not change TOML schema or OpenAPI output.
- Do not route this work back to design; the design handoff is complete.

