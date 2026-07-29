---
title: "herdr Session Provider"
---

[herdr](https://herdr.dev) is a terminal workspace manager built for AI coding
agents. Gas City ships a native **herdr** session-provider backend as an
**opt-in** alternative to tmux: one shared herdr session-server per city, one
workspace per rig (and one for the town), and one tab per agent. tmux stays the
default backend and the fallback — herdr is additive, selected through the same
runtime-selection setting that picks tmux, k8s, ssh, or exec.

## Prerequisites

Install the `herdr` binary and make sure it is on `PATH`:

```bash
herdr --version   # the provider is verified against herdr 0.7.1+
```

The backend is registered as a builtin runtime name (`herdr`) — no pack or
`[runtimes.*]` declaration is needed. If the binary is missing, sessions
selected onto herdr fail to start; install it before flipping the selector.

## Enabling herdr

`herdr` is selected with the same runtime selector used for every other
backend. It can be the city-wide provider, or the local half of the hybrid
provider while selected sessions run remotely on Kubernetes. It is not an
individual agent transport.

### City default

Set the session provider in `city.toml`:

```toml
[session]
provider = "herdr"
```

Every agent the city starts runs under herdr, except agents on the ACP transport
(whether pinned `session = "acp"` or because their provider defaults to ACP),
which route to the separate ACP backend instead (see below).

### Hybrid local backend

Use herdr as the hybrid provider's local backend when only selected sessions
should move to Kubernetes:

```toml
[session]
provider = "hybrid"
hybrid_local = "herdr"
remote_match = "review-pre"
```

Sessions whose names contain `review-pre` route to Kubernetes. All other
sessions remain on herdr. `GC_HYBRID_LOCAL` and `GC_HYBRID_REMOTE_MATCH`
override these values for one process. The default `hybrid_local` remains
`tmux` for backward compatibility. Unsupported local backend names fail
provider construction instead of silently moving sessions to tmux.

Changing `remote_match` transfers ownership between backends. Drain or stop
matching sessions before an in-place reload so the old backend cannot retain a
live duplicate. A container replacement naturally terminates the local herdr
server, but the rollout should still verify that each selected session appears
only in Kubernetes before accepting the change.

### Per-agent / per-rig

herdr cannot be selected as an individual agent transport. It is chosen
city-wide by `[session] provider = "herdr"`, as the local backend of
`provider = "hybrid"`, or process-wide by `GC_SESSION`.

The per-agent patch field is `session`, but it selects a **transport**, not a
backend. It accepts only `acp`, `tmux`, or omission (`IsValidSessionTransport`
in `internal/config/provider.go`), so `session = "herdr"` never selects the
herdr runtime. Config validation flags it as a warning:

```text
agent "dog-1": session "herdr" is not a valid session transport (use "acp", "tmux", or omit)
```

Under a herdr city, the transport router (`internal/runtime/auto`) sends only
ACP-registered sessions to the separate ACP backend and routes everything else
to the city's base provider, which is herdr. Two consequences follow:

- `session = "acp"` (or a provider that defaults to ACP) moves that agent off
  herdr, onto the ACP backend. It is the one per-agent lever that changes which
  backend an agent runs on.
- `session = "tmux"` does not keep an agent on tmux. The herdr provider does not
  implement the transport-capability check, so the pin is neither honored nor
  rejected; the agent falls back to the base provider and runs on herdr. To put
  an agent on tmux, the whole city (or process) must default to tmux.

### Environment (one-off)

For a quick local trial without editing config, export the selector:

```bash
export GC_SESSION=herdr
gc start <city>
```

`GC_SESSION` overrides the effective provider name for that process, the same
way it selects `exec:<script>` or any other backend.

## Piloting safely

herdr is opt-in. Start with a whole scratch city, then use hybrid routing when
you are ready to isolate selected Kubernetes workers. Recommended path:

1. **Try it per-process on a scratch city.** Select herdr with the environment
   variable on a throwaway city, so nothing is committed and your real city is
   untouched:

   ```bash
   GC_SESSION=herdr gc start <scratch-city>
   ```

   Every agent in that process runs under herdr, except agents on the ACP
   transport (whether pinned `session = "acp"` or because their provider
   defaults to ACP), which still route to the separate ACP backend. Watch it
   through a normal work cycle.

2. **Promote to the scratch city's default.** Once the per-process trial looks
   good, set the default in that city's `city.toml` and run it end to end:

   ```toml
   [session]
   provider = "herdr"
   ```

3. **Widen** to your real city by flipping its `[session] provider` to
   `"herdr"`, once the scratch city has been stable across several work cycles.

4. **Isolate selected workers** by switching the city to `provider = "hybrid"`,
   retaining `hybrid_local = "herdr"`, and choosing a narrow `remote_match`.
   Verify those sessions in Kubernetes before widening the match.

## Applying and verifying

- Reload or restart the city to apply a selector change (`gc reload`, or restart
  the city). Agents launched after the switch run under herdr; already-running
  sessions keep their current backend until they next restart.
- Confirm the effective selector with `gc config show` (the `[session]`
  `provider` value, plus any agent `session` overrides).
- Once agents are on herdr, their workspaces and tabs are visible through
  herdr's own UI (`herdr` lists the per-rig/town workspaces and per-agent tabs).

## Layout

Within the single per-city herdr session-server, gc places agents so the
workspace/tab structure mirrors the town:

- **One workspace per rig**, plus one for the town (mayor and town-level
  agents).
- **One tab per agent.** Rig polecats land in their rig's workspace as
  `polecat-<themed-name>` tabs; the placement is display-only and does not
  change agent identity.

See `internal/runtime/herdr-provider-design.md` in the source tree for the
provider's design notes, capabilities, and pilot rationale.
