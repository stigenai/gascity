# Omnigent remote capsule acceptance matrix

**Status:** normative completion rubric

**Issue:** `ga-xgv.1.5`

**Applies to:** all implementation and release work under `ga-xgv`

## Contract

A remote Omnigent capsule is supported only when Gas City places the workspace,
durable state, typed credential references, pinned Omnigent server and host,
harness, and outer tmux session in the same Kubernetes pod or SSH execution
boundary. The Omnigent API remains private inside that boundary. Gas City owns
placement and outer lifecycle; Omnigent owns harness, model/backend, auth use,
tool policy, sandbox, conversation, and harness-specific lifecycle.

Every required row below must have a deterministic offline test. Rows marked
**live** additionally require an opt-in environment test. An unsupported
capability returns a typed, actionable error before mutation. Skips count only
when the required external fixture is absent and the test reports the exact
missing fixture; an available but failing fixture is a failure.

## Support matrix

| Surface | MVP support | Required observable result | Unsupported behavior |
|---|---|---|---|
| local tmux | yes | existing local attach remains compatible | no behavior regression |
| local Herdr | yes | ordinary lifecycle-neutral live viewer | no Omnigent-specific Herdr lifecycle |
| Kubernetes placement | yes, live | server, host, harness, state, secret projection, tmux, and attachment all run in one pod boundary | controller-local harness or published Omnigent API |
| SSH placement | yes, live | same composition under verified remote account and contained roots | shared unisolated account, shell interpolation, or controller-local fallback |
| hybrid local/remote routing | yes | selected runtime alone determines placement; local and remote sessions coexist | capability failure silently reroutes local |
| Codex profile | yes, live | isolated Codex home/auth and exact conversation resume | ambient controller credential copy |
| multiple Claude profiles | yes, live | independent auth homes/backends, non-secret blurbs, sticky ordered failover | one shared mutable Claude home or Anthropic-only backend assumption |
| policy mail | opt-in | typed request/reply binding only after profile enables it | default recipient, automatic verdict, prose-parsed policy |
| generic remote TTY | yes, live before full remote-city interaction claim | authorized read/input/interrupt/resize/detach bound to one current session incarnation | raw Omnigent proxy or provider-specific public tunnel |
| future Runtime Provider Protocol adapter | compatible by conformance | advertises and passes the same typed capabilities | provider name allowlist or built-in-specific semantic branch |
| Omnigent managed/remote host | no | explicit validation error | network host registration or hosted control plane |
| Daytona built-in | no | explicit unsupported runtime/capability result | invented Daytona-specific Omnigent config |
| implicit cross-runtime state migration | no | relocation-required error without mutation | create a fresh conversation at destination |

## Acceptance cases

IDs are stable references for tests, release evidence, and downstream Beads
notes. `O` means deterministic offline coverage. `L` means an opt-in live test is
also mandatory.

### Launch, pin, and placement

| ID | Level | Happy case | Edge case | Failure case | Cleanup | Observability |
|---|---|---|---|---|---|---|
| CAP-LAUNCH-01 | O+L | one command plan starts pinned server, host, harness, and attach client inside selected Place | concurrent starts converge on one conversation and one live supervisor | missing capability, pin, catalog, state, or credential fails before interaction | losing children and conversation are stopped by exact identity | status names provider, Place, profile ID/backend blurb, pin, and readiness without secrets |
| CAP-LAUNCH-02 | O | launch argv/env/mounts derive only from typed validated inputs | Unicode/long/hostile session and profile labels remain data | shell metacharacters or reserved-env override cannot alter commands | partial stage removed; durable state retained | errors identify invalid field, never echo secret-bearing values |
| CAP-LAUNCH-03 | O+L | readiness requires private socket plus server and exactly one host | slow bounded startup becomes pending, not absent | dead child, wrong protocol, or multiple hosts is unavailable | all child process groups stop even if socket disappeared | child exit category and redacted tail are inspectable |
| CAP-PIN-01 | O+L | executable/image, package version, commit, and API schema match the approved pin | platform-specific artifact digest selected deterministically | mismatch or update attempt stops launch | mismatched artifact never executes | explain/doctor show pin and digest fingerprint |

### Durable identity, state, and continuity

| ID | Level | Happy case | Edge case | Failure case | Cleanup | Observability |
|---|---|---|---|---|---|---|
| CAP-ID-01 | O | canonical session key yields stable Kubernetes and SSH identity | sanitizer collisions remain distinct by 130-bit token/full digest | empty/invalid scope or digest mismatch fails closed | no foreign resource touched | exact session ID and non-secret digest fingerprint reported |
| CAP-ID-02 | O | alias/template/worktree rename preserves allocation | retry and concurrent ensure share one allocation with one attachment | changed provider scope requires explicit relocation | surplus Place only is removed | status distinguishes logical capsule from Place incarnation |
| CAP-STATE-01 | O+L | pod/SSH process replacement resumes exact bound conversation and transcript | controller crash adopts one valid resource idempotently | missing, multiple, invalid, or unresolvable bound state never creates fresh | invalid new Place is torn down; retained state untouched | missing, ambiguous, invalid, credential, and transport categories differ |
| CAP-STATE-02 | O | sleep/suspend/drain/city-stop detach or stop runtime while retaining state | archived/orphaned continuity-eligible session can resume | terminal closed session cannot wake | Place removal cannot invoke state deletion | state shows active/retained/orphan/ambiguous without content |
| CAP-STATE-03 | O+L | explicit reset clears active binding and next start creates one new conversation | losing concurrent creator stops its unused conversation and resumes winner | persistence failure stops unpersisted conversation | historical Omnigent records remain until purge | reset/new binding transition is auditable without ID leakage beyond authorized status |
| CAP-STATE-04 | O+L | purge of a closed, unattached, uniquely owned capsule deletes one allocation | repeating recorded successful purge is idempotent | open/live/ambiguous/missing-unrecorded target refuses purge | no broad selector/path deletion | purge audit includes session and provider resource fingerprints |

### Typed secrets and profiles

| ID | Level | Happy case | Edge case | Failure case | Cleanup | Observability |
|---|---|---|---|---|---|---|
| CAP-SECRET-01 | O+L | selected profile resolves one provider-native secret reference at provider edge | secret rotation applies to a new harness process without changing catalog identity | absent key, wrong type/owner/mode, or unavailable resolver fails before harness | ephemeral projection removed; provider-owned secret retained | availability is boolean/category only; canary never appears on any observable surface |
| CAP-SECRET-02 | O+L | Codex and two Claude profiles see isolated homes and credentials | Claude backend blurb may identify a non-Anthropic compatible backend | one profile cannot read another projection or inherit ambient controller auth | each process unmounts only its projection | discovery returns ID/display/blurb/harness/backend/network/chain only |
| CAP-PROFILE-01 | O+L | sticky failover advances exactly once on typed auth/rate/backend-unavailable event | concurrent duplicate event is idempotent; new conversation starts at first profile | prose, tool, policy, workspace, sandbox, or unknown failure cannot trigger failover | losing/old harness is stopped without deleting conversation | old/new profile IDs, blurbs, category, active index, time; no backend prose |
| CAP-PROFILE-02 | O | catalog and agent paths are contained, immutable, and digest-verified | stable non-secret profile blurb supports arbitrary backend wording | duplicate/cycle/cross-harness/missing/escaping/symlink entry fails validation | partial staged generation removed | doctor identifies profile and field, not file contents or env names |

### Kubernetes

| ID | Level | Happy case | Edge case | Failure case | Cleanup | Observability |
|---|---|---|---|---|---|---|
| CAP-K8S-01 | O+L | typed pod plan mounts workspace, immutable catalog, exact Secret keys, run root, and exclusive PVC separately | replacement waits for old attachment and reuses PVC | wrong PVC digest/owner or concurrent attachment fails closed | pod deletion uses UID/resourceVersion; PVC retained | pod UID, PVC fingerprint, phase and readiness shown |
| CAP-K8S-02 | O+L | pod is non-root, tokenless, unprivileged, seccomp-default, quota-bounded, and default-deny ingress | explicitly approved harness exception is surfaced in plan and separately tested | host path/network/PID, privileged, broad Secret, sudo, or API token rejected | NetworkPolicy and ephemeral projections removed with Place | doctor reports security posture without pod/Secret content |
| CAP-K8S-03 | O+L | offline denies all egress; external profile reaches only declared model endpoint/proxy | DNS/endpoint rotation within approved policy remains available | metadata/API/controller/private network access denied | network resources scoped and deleted exactly | denied destination category counted without sensitive URL/query |
| CAP-K8S-04 | O+L | exec/tmux attach targets exact pod UID and instance fingerprint | network disconnect reconnects to same live tmux session | stale pod name or replaced incarnation rejects channel | disconnect closes viewer only | connect/disconnect/action audit events identify fingerprints |

### SSH

| ID | Level | Happy case | Edge case | Failure case | Cleanup | Observability |
|---|---|---|---|---|---|---|
| CAP-SSH-01 | O+L | strict host key, batch mode, fixed helper protocol, prerequisites, and contained bases validate | reconnect/keepalive resumes same directory and tmux session | host-key change, shared account, missing helper/tmux/pin, or bad owner/mode stops before launch | no remote mutation after failed preflight | doctor names missing prerequisite/category without command or credential disclosure |
| CAP-SSH-02 | O+L | non-secret staging is atomic and digest-verified through fixed typed payload | interrupted stage retries without exposing half-generation | path/link/mount escape or command metacharacter remains inert data | incomplete generation removed, last valid retained | stage digest/generation and redacted byte counts shown |
| CAP-SSH-03 | O+L | state and profile homes are owner-only and independent of workspace | process restart and connection loss preserve exact conversation | absent/multiple/wrong-owner state with binding fails closed | exact process groups/tmux socket stopped; state retained | remote host/account fingerprint and liveness category shown |
| CAP-SSH-04 | O+L | viewer attach, input, interrupt, resize, detach and reconnect are fully interactive | slow reader/backpressure and terminal resize races are bounded | wrong host/session/incarnation/grant rejects action | disconnect never kills worker; stop kills only owned process tree | ordered audit events and bounded transcript follow generic TTY schema |

### Herdr, tmux, and generic remote TTY

| ID | Level | Happy case | Edge case | Failure case | Cleanup | Observability |
|---|---|---|---|---|---|---|
| CAP-VIEW-01 | O+L | tmux and Herdr show the same authoritative outer worker session and full interactive stream | multiple viewers attach/detach independently | viewer implementation cannot create a second harness or change desired lifecycle | closing viewer resources preserves worker | fleet view joins city, session, runtime, profile blurb and state |
| CAP-VIEW-02 | O+L | generic TTY grant authorizes distinct read/input/interrupt/resize actions | grant expiry/revocation and incarnation replacement terminate safely | read-only user cannot input; cross-city/session/replay rejected | channel buffers and port-forward/exec resources close on disconnect | principal, session, incarnation, action and result are audited |
| CAP-VIEW-03 | O | malformed/secret-bearing/oversized output is bounded and redacted on status/log paths | raw authorized PTY preserves required interactive bytes and ordering | terminal text never becomes typed control/failover/policy event | slow/stuck viewers are dropped independently | truncation/backpressure is explicit, not silent |
| CAP-TMUX-01 | O | test/runtime cleanup targets only known socket/session and direct process groups | socket removed before cleanup still terminates captured server/monitor PIDs | bare/default `tmux kill-server` is statically and dynamically forbidden | no matching process remains; personal tmux is unaffected | cleanup failure names owned target fingerprints |

### Policy, hybrid routing, and provider protocol

| ID | Level | Happy case | Edge case | Failure case | Cleanup | Observability |
|---|---|---|---|---|---|---|
| CAP-POLICY-01 | O+L | opted-in typed policy event produces one bound mail and exact authorized answer | crash/replay reuses idempotency key and mail binding | disabled, wrong sender/request/conversation/action/schema/size fails visibly | canceled request cannot later resolve | sanitized request state, mail ID, recipient identity and answer hash only |
| CAP-HYBRID-01 | O+L | simultaneous local and remote sessions use their configured runtime and state | remote capability loss affects only selected remote session | no local fallback, fresh conversation, or profile fallback on placement failure | cleanup follows actual selected provider only | status shows selection provenance and missing capability |
| CAP-RPP-01 | O | a future adapter advertises staging, durable state, typed secrets, exec, interaction, and exact cleanup capabilities | extra provider features do not alter core semantics | absent required capability rejects before provision | conformance fixture proves no leaked resource | capability report is typed and provider-neutral |

### Fault, race, performance, and release

| ID | Level | Happy case | Edge case | Failure case | Cleanup | Observability |
|---|---|---|---|---|---|---|
| CAP-FAULT-01 | O+L | retries recover from bounded transient provider faults without identity change | faults at every launch/attach/bind/detach stage remain idempotent | permanent and ambiguous faults stop mutation and surface category | no ephemeral resource/process leaks | deterministic trace correlates session and incarnation fingerprints |
| CAP-RACE-01 | O | concurrent start/reset/replace/close/purge/view operations serialize or fail safely | race detector and repeated stress preserve one binding/allocation | no split conversation, double mount, stale attach, or purge/wake race | post-test provider census is clean except expected retained state | conflict metrics/errors are bounded and typed |
| CAP-PERF-01 | O+L | warm attach and replacement stay within documented budgets | slow backend/harness is separated from provider readiness budget | timeout never reports success or launches a replacement conversation | timeout cleans partial ephemeral state | timings split provision, stage, service, host, harness and TTY phases |
| CAP-RELEASE-01 | O | binaries/images use exact Omnigent pin, checksums, SBOM and attestations | multi-platform variants map to explicit digests | unpinned, dirty, unattested or canary-bearing artifact cannot publish | failed release leaves no partial public tag/assets | manifest links source commits, tests and artifact digests |

## Test tiers and evidence

### Offline required on every implementation change

- unit and table tests for identity, plans, validation, redaction, protocol
  codecs, state transitions, cleanup targets, and error categories;
- fake Kubernetes and scripted SSH provider contract tests covering every call,
  rollback, retry, and injected failure boundary;
- one centralized generated-canary audit captures provider-facing output,
  process arguments, public status/CLI JSON, logs, crash errors, fingerprint
  diagnostics, metrics/events, and Beads metadata. It rejects secret values,
  auth environment names, credential paths, sensitive provider state paths,
  policy content, transcripts, and user input while separately proving the
  provider-confined launch plan retains only the reference metadata required to
  project credentials;
- hermetic pinned-Omnigent double for server/host/conversation/stream/failover and
  policy behavior;
- isolated real tmux tests using unique sockets plus direct PID/process-group
  leak census;
- race/stress cases for bind, start, replace, reset, viewer, close, and purge;
- static secret-canary scan across all captured observable surfaces;
- provider-neutral Runtime Provider Protocol conformance suite.

### Opt-in live required before release

- disposable Kubernetes namespace with enforceable NetworkPolicy, scoped RBAC,
  temporary per-profile Secrets, PVC replacement, interactive attach, injected
  pod/controller failure, and exact namespace resource census;
- dedicated SSH fixture with pinned host key, exclusive account, disposable
  workspace/state bases, independent profile homes, disconnect/reconnect,
  interactive attach, process/tmux leak census, and verified purge;
- real Codex session first, followed by at least two real Claude profiles using
  independent authentication and backend blurbs, proving exact resume and sticky
  failover without credential crossover;
- local and remote Herdr/tmux fleet view with input, interrupt, resize, detach,
  reconnect, and multiple viewers;
- opt-in policy request/reply round trip plus disabled-policy rejection;
- final artifact execution from the release binaries/images rather than the
  developer checkout.

Live evidence records fixture versions, source commits, artifact digests, test
IDs, timings, and redacted results. Secret values, environment names, credential
paths, raw backend errors, source content, transcripts, and user input are never
included.

## Completion rule

Downstream epics may close individual tasks with their scoped tests, but
`ga-xgv` closes only when every MVP row is implemented, every `O` case passes,
every available `L` fixture passes, unavailable live fixtures are explicitly
reported, no security invariant from `omnigent-remote-threat-model.md` is
violated, release artifacts are produced, and the StigenAI fork branch is
committed and pushed.
