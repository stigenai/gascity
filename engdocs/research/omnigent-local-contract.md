# Omnigent local contract audit

**Bead:** `ga-ou2.1.1`

**Audited:** 2026-08-15
**Status:** pinned, executable-verified contract with offline runtime proof

This note records the source-verified contract Gas City may consume from
Omnigent. It deliberately distinguishes behavior present at the reviewed
revision from behavior the integration still requires. Paths below are relative
to the canonical Omnigent repository unless stated otherwise.

## Reviewed artifact

| Field | Value |
|---|---|
| Repository | `https://github.com/omnigent-ai/omnigent` |
| Commit | `2aba5079d4d3a2a84d8c9927884fc4b8ce0eeecc` |
| Commit time | `2026-08-15T12:16:06Z` |
| Commit subject | `feat(bench): add CLI startup latency benchmark (#4793)` |
| Package version | `0.10.0.dev0` |
| OpenAPI info version | `0.1.0` |
| License | Apache-2.0 (`LICENSE`) |
| Release tag at commit | none |
| Verified executable | isolated frozen source build, `.venv/bin/omnigent` |
| Executable SHA-256 | `bbc878177989a0cbed7708451952e9332664aef95521c2bb9c7f8b3e78c4b7da` |
| Reported provenance | `omnigent 0.10.0.dev0 (2aba5079, built 2026-08-15T16:57:04Z)` |

The integration pin is the full Git commit, not the mutable development
version or OpenAPI document version. The `omnigent` and `omni` console scripts
both resolve to `omnigent.cli:main` (`pyproject.toml`). A built artifact records
its source revision through `setup.py`; `omnigent --version` exposes the short
commit. Gas City therefore verifies all three available identities on every
start: exact executable SHA-256, exact package version, and a reported commit
prefix matching the configured full commit. An unbuilt editable checkout that
reports only the package version fails closed.

## Process and command contract

The supported supervised form is the foreground server:

```text
omnigent server \
  --host 127.0.0.1 \
  --port <city-assigned-port> \
  --database-uri sqlite:///<city-state>/chat.db \
  --conversation-database-uri sqlite:///<city-state>/chat.db \
  --artifact-location <city-state>/artifacts \
  --config <city-state>/config/config.yaml \
  --no-open
```

Evidence: `omnigent/cli.py` defines those options and documents bare
`omnigent server` as a foreground process stopped by Ctrl-C. Default bind is
`127.0.0.1:6767`; Gas City must provide an explicit loopback port to avoid the
machine-global reuse path. `GET /health` is the readiness probe.

Gas City must not use `omnigent server --background` or the deprecated
`omnigent server start` alias. Those commands detach, choose/reuse a port, and
record state in `local_server.pid`, `local_server.sig`, and
`local_server.logpath` (`omnigent/host/local_server.py`). That conflicts with
Gas City's process-table-derived supervision and city-scoped ownership.
`omnigent server status --json` reports:

```json
{
  "running": true,
  "pid": 4321,
  "port": 6767,
  "url": "http://127.0.0.1:6767",
  "log_path": "...",
  "live_sessions": 0,
  "daemon_attached": false
}
```

This is a diagnostic surface only. It is defined to return exit status zero
even when no background server is running, so it is not a lifecycle oracle
(`tests/cli/test_server_lifecycle.py`). Click usage and `ClickException`
failures exit nonzero (normally 2 and 1 respectively); harness child failures
may preserve the child's return code. Gas City must treat any nonzero exit as a
typed startup/runtime failure and preserve stderr rather than branch on
undocumented numeric values.

The detached-server stop path sends `SIGTERM`, waits, then escalates to
`SIGKILL` (`omnigent/host/local_server.py`). The foreground server follows
uvicorn's normal SIGINT/SIGTERM shutdown. Gas City should send SIGTERM to its
known foreground child, apply its configured grace period, and then kill that
exact child only. It must not call Omnigent's pidfile-based stop command.

## State and configuration isolation

Gas City must set all of the following on the supervised child:

| Variable/config | Required value | Source-verified behavior |
|---|---|---|
| `OMNIGENT_DATA_DIR` | `<city>/.gc/services/omnigent/data` | Moves DB, artifacts, logs, auth-token pointers, and detached-server sidecars from `~/.omnigent` (`host/local_server.py`, `process_logging.py`, `cli_auth.py`). |
| `OMNIGENT_CONFIG_HOME` | `<city>/.gc/services/omnigent/config` | Makes the global config path `<value>/config.yaml`; it does not move runtime data (`config.py`). |
| process cwd | Gas City-assigned rig/worktree | Omnigent also reads `.omnigent/config.yaml` under cwd and merges its `harness` map with global config (`config.py`). The integration disables project-local discovery by launching with an explicit generated `--config` and validating the assigned cwd. |
| `OMNIGENT_NO_UPDATE_CHECK` | `1` | Suppresses the detached package-index update lookup (`update_check.py`). |
| `OMNIGENT_DISABLE_TELEMETRY` | `true` | Disables product analytics (`telemetry/client.py`). |
| `DO_NOT_TRACK` | `1` | Independent defense-in-depth product-analytics disable (`telemetry/client.py`). |
| `OMNIGENT_TELEMETRY_ENABLED` | `0` | Keeps OpenTelemetry instrumentation disabled; this telemetry family is opt-in by default (`runtime/telemetry.py`). |
| `OMNIGENT_OTEL_CAPTURE_CONTENT` | `0` | Prevents message/tool bodies entering spans if telemetry is enabled accidentally (`runtime/telemetry.py`). |

The two telemetry implementations have different switches. Runtime tracing is
off by default, while product analytics has explicit opt-out switches. The
local integration sets every off switch and generates `telemetry: false` in
config. Update checking defaults to a background query of the configured
package index, ultimately `https://pypi.org/simple`; the integration always
sets `OMNIGENT_NO_UPDATE_CHECK=1`.

State directories and generated config must be owner-only (`0700` directories,
`0600` files). Omnigent expands `$VAR` references in agent auth configuration
at parse time. Gas City therefore stores only profile IDs and non-secret
metadata and never renders, logs, persists, or forwards resolved credential
values.

## Local server API used by Gas City

The canonical wire schema is the pinned repository's `openapi.json`; prose in
`omnigent/server/API.md` is supporting evidence only.

| Operation | Contract |
|---|---|
| readiness | `GET /health` on the configured loopback endpoint |
| discover harnesses | `GET /v1/harnesses` |
| register agents at startup | repeated `omnigent server --agent <contained-agent-path>`; writes are deliberately absent from the public agent API |
| discover bindable agents | `GET /v1/agents`, cursor-paginated; returns only built-in/operator-registered agents (`session_id IS NULL`) with opaque `ag_*` IDs |
| create session | `POST /v1/sessions` JSON with `agent_id`, `host_type:"external"`, explicit absolute `workspace`, optional initial items; returns a session snapshot with opaque `conv_*` ID |
| snapshot/resume | `GET /v1/sessions/{id}`; `404` means missing and must fail visibly |
| bind/recover | `PATCH /v1/sessions/{id}` for mutable runner affinity and metadata |
| send input | `POST /v1/sessions/{id}/events` with typed `message` data; accepted inputs return `202 {"queued":true}` |
| interrupt | same events endpoint with `type:"interrupt"`; returns `queued:false` and emits interruption/status events |
| stop without deleting | same events endpoint with `type:"stop_session"`; preserves transcript and permits later relaunch |
| live output | `GET /v1/sessions/{id}/stream` SSE |
| fork | `POST /v1/sessions/{source_id}/fork`; produces a new unbound conversation and copies history |
| delete | `DELETE /v1/sessions/{id}`; destructive and not part of normal Gas City stop |

The SSE stream is a live tail with no event-sequence replay. The reconnect
contract is: subscribe first, fetch the snapshot second, then deduplicate by
stable item ID. Events publish in order and the stream ends with `[DONE]`.
Gas City stores only the validated opaque conversation ID and reconstructs
display state from this contract. It never creates a fresh conversation when a
stored ID returns 404.

Session creation exposes Omnigent-managed hosts, sandboxes, and worktrees:
`host_type:"managed"`, `host_id`, and `git`. They are forbidden here. The
integration always uses `host_type:"external"`, passes the absolute workspace
already selected by Gas City, omits `git`, and rejects a response that reports
different placement.

## Interactive attachment contract

Omnigent has two distinct interactive shapes:

1. `omnigent claude` and `omnigent codex`, plus the `claude-native` and
   `codex-native` harnesses, mirror native TUIs through Omnigent-owned tmux.
   Their native dispatch code explicitly states that the wrappers attach to a
   tmux pane. They are incompatible with the MVP's no-nested-tmux rule when
   hosted inside Gas City's herdr/tmux pane.
2. The session API supports an interactive client over typed message POSTs,
   ordered SSE output, snapshots, and interrupts. Gas City's pane client uses
   this shape with the non-native `claude-sdk` and `codex` harnesses. The outer
   pane remains owned by herdr or tmux; Omnigent remains the only conversation
   owner.

Omnigent also exposes
`WS /v1/sessions/{session_id}/resources/terminals/{terminal_id}/attach`. Server
to client frames are binary PTY bytes. Client binary frames are raw input;
client text frames accept `{"type":"resize","cols":N,"rows":M}`. The
`read_only=true` query drops input and invokes `tmux attach -r`. This endpoint
is useful for Omnigent-created terminal resources but does not eliminate its
inner tmux dependency, so it is not the MVP attach seam.

Detach of the Gas City pane client closes only its HTTP/SSE connections. It
does not delete or stop the Omnigent conversation. Reattach validates the
stored `conv_*` ID, subscribes, fetches the snapshot, and resumes. Profile
changes apply only to new conversations; an existing conversation retains its
resolved execution identity.

## Harness, auth, and provider configuration

The reviewed revision supports both required harness families:

| Family | API-oriented harness | Native terminal harness |
|---|---|---|
| Claude Code | `claude-sdk` | `claude-native` / `omnigent claude` |
| Codex | `codex` | `codex-native` / `omnigent codex` |

`executor.auth` is a typed discriminated union (`omnigent/spec/types.py`,
`omnigent/spec/parser.py`):

- `type: api_key` with an environment reference and optional compatible
  `base_url`;
- `type: databricks` with a named Databricks profile;
- `type: provider` with a name under global `providers:` configuration.

This proves that a Claude-family harness is not coupled to Anthropic: a named
provider can supply an Anthropic-protocol-compatible backend, and Databricks
profiles are first-class. The server must receive auth references through its
private local config; Gas City must never read the referenced credential.

### Required profile compatibility extension

The pinned revision does **not** expose the required Gas City-facing profile
catalog (`id`, name, blurb, harness family, backend label, network class,
availability, active/fallback state), nor an ordered conversation-sticky chain
whose members may carry different auth mechanisms. Source-wide searches found
only:

- one `executor.auth` value per agent;
- one deprecated Databricks `executor.profile` value;
- `llm.fallback_models`, consumed by policy-LLM calls and sharing one
  connection/profile across candidates;
- smart model routing, which is dynamic selection rather than ordered
  auth/unavailability failover.

None satisfies the approved requirement directly. In particular,
`llm.fallback_models` is not cross-auth, not harness-profile discovery, and is
documented to share a single credential connection. The Gas City integration
therefore supplies a narrow local Omnigent compatibility layer: it registers
one Omnigent agent per opaque profile, persists the immutable ordered chain and
active index in Omnigent conversation labels, and invokes Omnigent's in-place
`switch-agent` operation only for typed terminal LLM authentication,
rate-limit, or backend-unavailable signals. The same `conv_*` conversation and
transcript remain authoritative. This layer performs no account ranking, cost
routing, prose interpretation, or Gas City work routing. A later reviewed
Omnigent revision with an equivalent native profile-chain API can replace it
after the contract tests prove parity.

## Local-only exclusions

The following source-visible Omnigent features are deliberately not used:

- managed hosts or managed sandboxes, including Kubernetes and Daytona;
- host and runner tunnels, collaboration/sharing URLs, or hosted control
  planes;
- `git` session creation and Omnigent-created worktrees;
- background/detached server ownership;
- native tmux wrappers in a Gas City pane;
- auto-install, upgrade, update checks, or unpinned executable discovery;
- ambient bulk credential forwarding;
- product analytics or OpenTelemetry export.

Explicit model traffic to the backend named by the selected profile is the
only allowed non-loopback network flow. Offline mock profiles may reach only a
loopback mock endpoint.

## Credential-free mock evidence

The pinned repository defines a credential-free mock lane in
`tests/e2e/conftest.py`: absent `--llm-api-key`, it starts
`tests/server/integration/mock_llm_server.py` on `127.0.0.1`, injects
`OPENAI_BASE_URL=<loopback>/v1`, and uses `mock-key`. The cheapest full harness
round trip is `tests/integration/test_smoke.py`, which creates a session,
posts a unique marker, and requires the same marker in the completed response.

The source-only audit first attempted the exact test in dependency-offline
mode:

```text
OMNIGENT_NO_UPDATE_CHECK=1 \
OMNIGENT_DISABLE_TELEMETRY=true \
OMNIGENT_TELEMETRY_ENABLED=0 \
DO_NOT_TRACK=1 \
uv run --no-config --offline --frozen pytest \
  tests/integration/test_smoke.py --integration \
  --harness open-responses --model mock-smoke -q
```

That source-only attempt did not execute because the clean checkout lacked the
pinned dependency set (`alembic==1.18.4` was absent from the local uv cache).
No curl installer, Omnigent setup command, global install, or credential was
used. After the static discovery was complete, the exact checkout was built in
its disposable `.venv` with `uv sync --no-config --frozen --group test`. The
build reported commit `2aba5079` and produced the digest recorded above.

The smoke proof then ran with an empty environment except for isolated
`HOME`, `OMNIGENT_CONFIG_HOME`, `OMNIGENT_DATA_DIR`, a minimal `PATH`, locale,
and the telemetry/update disable switches. macOS `sandbox-exec` denied every
non-loopback IP destination while permitting loopback TCP and Unix-domain
sockets (the latter is required by the harness process manager):

```text
sandbox-exec -p '
  (version 1)
  (allow default)
  (deny network-outbound
    (require-all
      (require-not (remote unix-socket))
      (require-not (remote ip "localhost:*"))))
' .venv/bin/pytest tests/integration/test_smoke.py \
    --integration --harness open-responses --model mock-smoke -q

Running 1 items in this shard
.                                                                        [100%]
1 passed in 4.64s
```

The sandbox profile was separately probed before the run: a loopback TCP
round trip and a Unix-socket round trip succeeded, while a connection to
`1.1.1.1:443` failed with `PermissionError: [Errno 1] Operation not permitted`.
No model credential was present. This is an executed server → runner → harness
→ mock-LLM → completed-response proof, not merely source evidence.

## Compatibility gaps and revisit triggers

1. Conversation-sticky ordered cross-auth profile failover is absent at the
   reviewed revision and is supplied by the local compatibility layer described
   above.
2. Agent write registration is a startup CLI operation rather than a public
   REST operation; the sidecar must restart to apply a changed agent catalog.
3. SSE sequence numbers are optional in the broad schema. Automatic failover
   requires a positive sequence number and fails closed when it is absent, so
   duplicate delivery cannot advance two profiles.

Any later Omnigent pin must repeat this audit, diff `openapi.json`, rerun the
offline contract suite, and explicitly re-evaluate telemetry/update defaults,
session reconnect semantics, auth expansion, and native tmux behavior.
