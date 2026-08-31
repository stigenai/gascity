# Local Omnigent integration

**Status:** accepted direction; compatibility gates remain implementation work

**Roadmap:** `ga-ou2`
**Omnigent pin:** `2aba5079d4d3a2a84d8c9927884fc4b8ce0eeecc`

## Decision

Gas City composes a city-scoped `proxy_process` service with an ordinary Gas
City provider command. It does not add a runtime provider or a new primitive.

The service command is `gc omnigent serve`. It verifies an externally installed
Omnigent executable, launches its foreground loopback-only server and foreground
local host under separate exact process groups, exposes the server through the
service's existing Unix socket, and supplies the narrow Gas City compatibility
API for opaque execution profiles. The provider command
is `gc omnigent attach --profile <id>`. Herdr or tmux starts that command in the
same way it starts any other interactive agent. The command creates or resumes
one Omnigent conversation, posts operator input, renders the typed session
stream, and persists the returned opaque conversation ID through Gas City's
existing provider-session hook path.

```text
controller
  |
  +-- [[service]] proxy_process: gc omnigent serve
  |       |
  |       +-- verified external omnigent server (foreground, loopback)
  |       +-- verified external omnigent host (foreground, local runner dispatch)
  |       +-- profile catalog + failover adapter (Unix socket)
  |               |
  |               +-- Omnigent sessions, agents, auth, policies, transcript
  |
  +-- worker.Handle -> herdr or tmux
          |
          +-- gc omnigent attach --profile <opaque-id>
                  |
                  +-- same Unix socket and Omnigent conversation
```

This is an opt-in provider/service composition. Existing cities, runtime
selection, `worker.Handle`, and session reconciliation do not branch on
Omnigent.

## Primitive Test

| Capability | Atomicity | Improves with models | Transport only | Verdict |
|---|---|---|---|---|
| supervise a pinned local sidecar | one service owner and exact-process-group teardown are required | models still need it | process transport | existing `[[service]]` primitive |
| host an interactive pane | provider/runtime attachment must retain identity | models still need it | terminal transport | existing Session/runtime primitive |
| persist an opaque conversation ID | concurrent restarts must not fork identity | models still need it | metadata transport | existing Session/bead projection |
| select a configured profile ID | config validation must reject ambiguity | models still need it | config transport | provider-specific adapter config |
| advance an ordered fallback chain | must be atomic per conversation | models still need deterministic auth recovery | protocol state transition, no semantic ranking | Omnigent adapter layer |
| decide which Gas City agent gets work | not an Omnigent concern | smarter models may decide better | cognition | formulas/prompts, unchanged |

Every needed SDK function already belongs to a current primitive. The
profile/failover contract is specific to the external harness layer and stays
in `internal/omnigent`; it is not generalized into core config, runtime, or
session interfaces.

## Ownership and source-of-truth matrix

| Concern | Owner | Source of truth / operation |
|---|---|---|
| desired worker, pool, formula, retry | Gas City | beads, formulas, controller |
| sidecar desired lifecycle | Gas City | city-scoped `[[service]]` |
| exact Omnigent executable | operator installs; Gas City verifies | profile catalog pin and executable digest |
| foreground process start/stop | Gas City service adapter | exact server and host process groups |
| readiness/liveness | adapter | server `GET /health` plus exactly one online local host; uncertainty is unavailable, never absent |
| agent identity | Gas City | session bead and `GC_SESSION_ID` |
| runtime placement | Gas City | worker boundary plus herdr/tmux selection |
| workspace/worktree | Gas City | `session.Info.WorkDir`; sent once as absolute `workspace` |
| prompt and operator input | Gas City provider command transports once | Omnigent typed `message` event |
| harness and model | Omnigent profile | registered Omnigent agent spec |
| auth and backend connection | Omnigent profile | agent `executor.auth` / named provider; Gas City sees no resolved value |
| tool and local sandbox policy | Omnigent profile | agent spec around the assigned workspace |
| conversation ID and transcript | Omnigent | opaque `conv_*` ID and session store |
| Gas City resume projection | Gas City | existing `session_key` metadata containing only validated opaque ID |
| ordered profile chain | Omnigent adapter | profile catalog plus conversation labels |
| active/fallback profile | Omnigent adapter | conversation labels, returned as non-secret status |
| attach/detach | Gas City runtime owns pane; Omnigent owns conversation | `gc omnigent attach` connections only |
| output ordering | Omnigent | SSE publish order, snapshot/item-ID reconciliation |
| policy request | Omnigent produces typed request; Gas City transports mail | message bead and explicit response |
| stop worker pane | Gas City | `worker.Handle` / runtime provider |
| stop harness turn | Omnigent | `interrupt` or `stop_session` event |
| remote placement | Gas City, future | current Gas City remote runtime concepts; excluded from adapter |

An Omnigent runtime provider is rejected because it would make Omnigent choose
WHERE a process runs and duplicate herdr/tmux lifecycle. A nested tmux wrapper
is rejected because the pane and the Omnigent native wrapper would both own a
terminal lifecycle. A second worktree manager is rejected because the session
already has an authoritative Gas City work directory.

## Profile catalog v1

The catalog is a local YAML file under the service state root. It is not part of
`city.toml`, pack distribution, session beads, or the public Gas City API. A
pack may provide a non-secret example, while an operator-owned catalog supplies
machine-local agent paths and the executable digest.

```yaml
version: 1
omnigent:
  commit: 2aba5079d4d3a2a84d8c9927884fc4b8ce0eeecc
  package_version: 0.10.0.dev0
  executable: omnigent
  sha256: sha256:<platform-specific-64-hex-digest>

profiles:
  codex-local:
    display_name: Codex
    blurb: Codex coding harness using the local subscription profile.
    harness: codex
    backend: openai-subscription
    network: external-model
    agent: agents/codex.yaml
    # Availability requires every named variable to be nonempty. Names and
    # values never appear in Gas City discovery, state, or logs.
    environment: [CODEX_HOME]
    # Optional values are forwarded only when the workspace supplies them and
    # do not affect profile availability.
    optional_environment: [GITHUB_TOKEN, GIT_SSL_CAINFO, HOME]

  claude-primary:
    display_name: Claude primary
    blurb: Claude Code through the primary compatible gateway.
    harness: claude-sdk
    backend: primary-gateway
    network: external-model
    agent: agents/claude-primary.yaml
    fallbacks: [claude-secondary]
    environment: [CLAUDE_PRIMARY_TOKEN]
    optional_environment: [GITHUB_TOKEN, GIT_SSL_CAINFO, HOME]

  claude-secondary:
    display_name: Claude secondary
    blurb: Claude Code through an independent backup account and backend.
    harness: claude-sdk
    backend: backup-gateway
    network: external-model
    agent: agents/claude-secondary.yaml
    environment: [CLAUDE_SECONDARY_TOKEN]
    optional_environment: [GITHUB_TOKEN, GIT_SSL_CAINFO, HOME]
```

Profile IDs are the map keys. They must be stable, nonempty, bounded ASCII
identifiers. `display_name`, `blurb`, `harness`, `backend`, and `network` are
non-secret display metadata. `agent` resolves beneath the catalog directory;
absolute paths, `..`, symlinks escaping the root, duplicate IDs, missing agent
files, unknown fallback IDs, self-links, cycles, and cross-harness chains fail
validation. `environment` is an explicit allowlist of required process-only
variable names: all must be present for the profile to be available.
`optional_environment` is a separate allowlist forwarded only when a value is
present; an absent optional value does not affect availability. A name may not
appear in both lists. Neither names nor values enter public discovery. An agent
spec may refer to auth through `$ENV`, a Databricks profile, or a named Omnigent
provider; the adapter never expands it.

The public discovery response contains:

```json
{
  "id": "claude-primary",
  "display_name": "Claude primary",
  "blurb": "Claude Code through the primary compatible gateway.",
  "harness": "claude-sdk",
  "backend": "primary-gateway",
  "network": "external-model",
  "availability": "available",
  "chain": ["claude-primary", "claude-secondary"]
}
```

It never contains agent YAML, environment names or values, provider connection
fields, tokens, URLs carrying userinfo, or raw upstream errors.

### Sticky ordered failover

On session creation the adapter resolves and validates the full chain, registers
each agent with Omnigent, creates the conversation with the first agent, and
sets non-secret labels containing the immutable chain and active index. The
conversation ID is the atomic scope of stickiness. Concurrent failover requests
serialize per conversation and use compare-and-set semantics on the expected
active index.

The adapter advances exactly one position only when Omnigent returns a typed
failure in one of these protocol classes:

- authentication rejected or expired;
- rate limited;
- configured backend unavailable.

It does not parse model prose, rank candidates, balance load, compare cost, or
change Gas City routing. Unknown, malformed, tool, policy, workspace, sandbox,
and user-code failures stay on the current profile and surface unchanged but
redacted. A successful next turn remains on the new profile; recovery of an
earlier profile affects new conversations only. Exhaustion is terminal and
names only profile IDs/backends.

The live pane forwards only the bounded machine fields of terminal error
events—type, source, sequence, error code, and HTTP classifier—to the private
city-local `/gascity/v1/failover` sidecar endpoint. Provider prose never crosses
that adapter edge. The sidecar alone classifies the event, reconciles the
conversation-owned chain, performs `switch-agent`, and records the transition.
The pane receives only the active index/profile plus the old/new profile IDs,
non-secret catalog blurbs, reason category, and timestamp for display. It keeps
the existing conversation stream open; duplicate or stale observations are
ignored, and exhaustion terminates visibly without creating a conversation.

At the reviewed upstream pin, Omnigent lacks this compatibility API. The
adapter implements it with Omnigent's own `switch-agent` operation, preserving
the same conversation and transcript. State is stored in Omnigent conversation
labels rather than a Gas City status file. If Omnigent adds an equivalent
native profile-chain API, the adapter should become a pass-through after
contract tests prove parity.

## Conversation and attachment contract

`gc omnigent attach` has two start shapes:

```text
# Fresh: creates one Omnigent session and persists its returned ID.
gc omnigent attach --profile claude-primary

# Explicit import/debug resume: it must match any stored ID exactly.
gc omnigent attach --profile claude-primary --conversation conv_abc123
```

The provider has no `session_id_flag`; Gas City therefore does not pre-generate
a provider ID. Before resolving an attachment, the command reads the managed
session bead's existing `session_key`; when present, that exact ID is the only
conversation it requests. After fresh creation, and before opening a stream or
accepting work, the command binds the returned ID through the typed session
front door. The bind is first-writer-wins and idempotent. Conditional stores
use metadata compare-and-set across processes; legacy stores serialize within
the `gc` process. An ambiguous write is verified by rereading durable state.
If a duplicate launch loses the bind, it stops its unused fresh conversation
and resumes the stored winner. If persistence fails, it stops the unpersisted
conversation and exits rather than starting work.

The opaque key is never carried through a credential-bearing environment or a
parallel status file. A Gas City or sidecar restart reconstructs attachment
solely from the session bead and Omnigent's own local conversation store.
Explicit `--conversation` cannot override a stored key. Conversation forking is
not supported by this adapter; a replacement Gas City session receives a new
identity and therefore may create its own conversation.

The client establishes the stream before the snapshot, deduplicates stable item
IDs, renders committed history once, and then renders live events in publish
order. stdin lines become typed message events. Interrupt sends Omnigent's
`interrupt` event. EOF or pane detach closes the client only. Gas City stop may
send `stop_session` before terminating the pane, but never deletes the
conversation.

If the supplied conversation is absent, archived incompatibly, belongs to a
different profile chain, reports a different workspace, or cannot be verified
because the sidecar is unavailable, attach exits nonzero with an actionable
error. It never falls back to fresh creation. This is the fail-closed boundary
that prevents split conversations after crash/restart.

Herdr and tmux execute the same provider argv. Herdr remains preferred through
existing runtime configuration; tmux is the existing fallback. Neither runtime
knows about profile, auth, or conversation semantics. The MVP does not use
Omnigent's native terminal WebSocket because that path attaches to an
Omnigent-owned tmux instance.

### Read-only diagnostics

The opt-in command group provides three read-only projections over the same
typed sidecar and session state:

```text
gc omnigent explain [--profile ID] [--json]
gc omnigent status [--profile ID] [--session GC_SESSION_ID] [--json]
gc omnigent doctor [--profile ID] [--session GC_SESSION_ID] [--json]
```

`explain` reports the verified pin and path, selection provenance, profile
blurb/harness/backend, ordered chain, and availability. `status` additionally
joins the managed session bead with the exact conversation, assigned workspace,
and herdr/tmux runtime view. `doctor` independently reloads the city-scoped
catalog, re-verifies the executable, requires the private sidecar to report
`mode: local`, checks profile availability, names missing environment references
without reading out their values, and validates an optional exact attachment.
Missing and archived conversations fail visibly and never create replacements.
All three have stable schema-versioned JSON forms and omit agent file contents,
environment values, provider prose, and credential-bearing URLs.

## Locality and threat model

### Allowed processes and flows

| Source | Destination | Purpose |
|---|---|---|
| controller | `gc omnigent serve` child | service supervision |
| adapter | exact verified `omnigent server` child | local harness service |
| adapter | exact verified `omnigent host --server <loopback>` child | foreground local runner dispatch |
| local host | exact harness runner child | execute the selected harness in Gas City's assigned workspace |
| adapter | Omnigent loopback port | readiness and API proxy |
| worker pane | city service Unix socket | profile/session API |
| Omnigent harness | explicit profile backend | model traffic only |
| worker pane / Omnigent harness | assigned worktree | coding tools within configured sandbox policy |
| adapter | Gas City mail command/API | opted-in typed policy request/response only |

The public service visibility is `private`; WebSocket publication is false.
The adapter binds the service-provided Unix socket. The Omnigent child binds a
random loopback TCP port. LAN/wildcard binds and non-loopback configured server
URLs fail validation.

### Forbidden behavior

- Omnigent `host_type:"managed"`, remote host/runner tunnels, managed-host APIs,
  Kubernetes, Daytona, collaboration links, and hosted control planes;
- session `git` configuration, repo clone, worktree creation, or a workspace
  different from the absolute Gas City assignment;
- `server --background`, pidfile-based lifecycle, auto-install, auto-upgrade,
  or update checks;
- implicit telemetry, product analytics, content capture, or implicit network
  probes;
- ambient credential forwarding beyond explicit operator configuration;
- credential values, auth headers, agent specs, raw backend responses, or
  secret paths in logs, events, labels, beads, status, or doctor output;
- interpreting a missing/unreachable conversation as permission to create one;
- dynamic model/profile ranking, cost routing, or dashboard profile selection.

Offline profiles point only to the hermetic loopback double and run with an
outbound-network denial. External traffic is opt-in through a profile whose
`network` field is `external-model`.

### Files and permissions

The service root is `.gc/services/omnigent`. Its `config`, `data`, `run`, and
`secrets` directories are `0700`; generated config and catalog are `0600`; logs
are `0640` and redacted. Omnigent receives distinct `OMNIGENT_CONFIG_HOME` and
`OMNIGENT_DATA_DIR` below that root. No state is placed in the user's shared
`~/.omnigent` directory. Conversation data and transcripts remain in
Omnigent's city-scoped database; Gas City beads contain only opaque IDs and
non-secret profile status.

## Policy mail

Policy enforcement is disabled unless a profile explicitly opts in. A pending
Omnigent policy interaction is translated into a typed message bead addressed
to a configurable Gas City identity. The mail includes conversation ID,
profile ID, request ID, request kind, bounded non-secret prompt, and the allowed
response schema. The response carries the same request ID and a schema-valid
answer. The reply body is strict JSON with `request_id`, `action`, and optional
`text`; replies with extra fields, the wrong sender, wrong request or mail ID,
or an action outside the advertised options are rejected visibly.

The sidecar journals the sanitized request, configured recipient, stable
idempotency key, exact Gas City mail ID, delivery state, and answer hash in the
Omnigent conversation labels. States move from `pending` to `delivering` to
`delivered`, or to `canceled`; replay after a crash reuses the same request
and mail binding. Gas City opens the mail provider only after an explicit
`policy.request` event and observes the bound mail thread asynchronously.

The adapter performs no default approval, denial, timeout verdict, escalation
ranking, or recipient selection. An unanswered request remains pending and
observable. Duplicate delivery is idempotent by conversation/request ID.
Responses to an unknown, canceled, already-resolved-with-different-answer, or
different-conversation request fail
visibly. Profiles without a configured policy-mail recipient reject policy
prompts rather than silently enabling a default role.

## Compatibility and rollout

The integration is enabled only when all of these gates pass:

1. the configured executable resolves to a regular executable file;
2. SHA-256 equals the exact catalog pin;
3. package version and configured commit match the reviewed contract;
4. foreground child stays alive and `GET /health` succeeds within the bound;
5. required OpenAPI operations and schemas match the compiled compatibility
   floor;
6. profile catalog and every referenced agent validate without secret
   expansion;
7. telemetry/update/locality environment is locked down;
8. the offline mock contract succeeds.

MVP supports the `codex` and `claude-sdk` API-oriented harnesses, multiple
Claude profiles, ordered sticky failover, herdr/tmux panes, continuity, status,
doctor, and opt-in policy mail. Non-goals are native TUI mirroring, dashboard
profile selection, dynamic routing, remote execution, managed sandboxes,
installation, upgrades, and a new public SDK abstraction.

Rollout begins with the offline profile, then one Codex profile, then two
Claude profiles with independent auth/backends, then policy mail. Each step
uses a new conversation. Disable stops the service and prevents new/resumed
Omnigent panes but leaves conversation data intact. Uninstall removes only the
opt-in config after an explicit operator command; service state is preserved by
default. Rollback restores the previous Gas City binary/config and the exact
previous Omnigent pin, then reuses the same city-scoped database only after its
schema compatibility check passes. No downgrade is allowed to guess at schema
compatibility.

The seam should be revisited only with executable evidence that two independent
Gas City consumers need a new primitive, or that upstream Omnigent provides a
tested profile-chain/attach API that can replace the adapter. In either case,
the zero-hardcoded-role rule, worker boundary, city-as-directory model, and
fork-isolation requirements remain binding.

## Test tiers

- Unit: catalog validation, pin verification, URL/locality validation, error
  classification, CAS failover, redaction, SSE reconciliation, and command
  construction.
- Hermetic process: real `gc omnigent serve` against a repo-owned sidecar
  double over Unix socket; crash/restart/cancellation and permission tests.
- Runtime composition: the same attach provider through real city-scoped herdr
  and tmux test instances, never the default tmux server.
- Workflow: one unchanged formula across Codex-shaped, primary-Claude-shaped,
  secondary-Claude-shaped, and forced-failover profiles.
- Live opt-in: exact external Omnigent pin plus real Codex/Claude auth; never a
  required offline CI gate and never stores credentials in fixtures.

Every error matrix belongs at the smallest owning layer. The critical
end-to-end proof is the unchanged-workflow portability journey; lower tests own
individual protocol and failure branches.
