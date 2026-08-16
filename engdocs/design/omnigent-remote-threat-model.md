# Omnigent remote capsule threat model

**Status:** accepted security contract

**Issue:** `ga-xgv.1.4`

**Scope:** capsule-local Omnigent on Gas City Kubernetes and SSH runtimes,
including tmux/Herdr and the future generic remote TTY channel

## Overview

Gas City places and supervises a worker capsule. Inside that capsule, the pinned
Omnigent server and host communicate over a private Unix socket and launch the
selected Codex or Claude-compatible harness in the Gas City-assigned workspace.
Gas City owns placement, outer lifecycle, durable-state projection, credential
references, and operator authorization. Omnigent owns harness behavior, backend,
authentication use, tool policy, sandbox, conversation, and harness-specific
lifecycle.

The security goal is containment, not trust in agent output. Repository content,
prompts, model responses, tool output, and harness output may all be hostile. A
compromised harness is allowed to exercise only the workspace, credentials,
network destinations, and terminal controls explicitly granted to its selected
profile. It must not gain another session's state, a different profile's
credentials, the Kubernetes control plane, the controller host, or an
unauthorized operator channel.

## Threat model, trust boundaries, and assumptions

### Assets

| Asset | Required property |
|---|---|
| Codex and independent Claude profile credentials | confidential and isolated per profile; never copied to observable control-plane data |
| workspace and repository | integrity limited to the assigned worktree; no cross-session path access |
| Omnigent database, host identity, transcript, and artifacts | confidential, exact-session integrity, durable continuity |
| opaque conversation binding in Beads | integrity and availability; no silent replacement or cross-session reuse |
| profile catalog and pinned binaries/images | authenticity and integrity |
| policy request and answer | authenticated session/request binding, integrity, replay safety |
| Kubernetes namespace, PVC, Secret, pod, exec, and attach surfaces | least privilege and exact resource ownership |
| SSH host/account, state base, command channel, and remote files | host authenticity, path containment, command integrity, exact cleanup |
| tmux/Herdr PTY input and output | session isolation, operator authorization, ordering, and bounded retention |
| Gas City controller and Beads store | unavailable from the harness except through narrow typed operations |
| backend/model traffic and source content | explicit egress only; no unintended destinations or plaintext downgrade |

### Actors

- **Local operator:** trusted to configure a city, runtime, profiles, secret
  references, and retention. Operator mistakes must fail visibly and narrowly.
- **Remote viewer/operator:** authenticated but not automatically authorized for
  every city, session, input action, transcript, or secret-bearing diagnostic.
- **Gas City orchestrator:** trusted placement and lifecycle authority. A crash
  is expected; a compromised controller is outside the capsule's confidentiality
  boundary because it can authorize placement and secret references.
- **Runtime administrator:** Kubernetes cluster or SSH host administrator is
  trusted with host-level access. A malicious cluster node/root SSH administrator
  is out of scope for credential confidentiality, but isolation mistakes created
  by Gas City remain in scope.
- **Workspace author:** untrusted. Repository files, hooks, symlinks, archives,
  prompts, and build scripts are attacker-controlled input.
- **Harness/model/tool:** untrusted after launch. Output may contain terminal
  escapes, forged protocol text, secrets, paths, and adversarial instructions.
- **Network attacker:** cannot break correctly configured TLS or SSH host-key
  verification, but may observe, delay, replay, redirect, or terminate traffic.
- **Another capsule:** hostile tenant inside the same Kubernetes namespace or SSH
  host account boundary unless provider configuration grants stronger isolation.

### Trust boundaries

1. **Controller to runtime provider.** Typed Gas City launch data crosses into
   Kubernetes API calls or SSH commands. Secret values must resolve only after
   this boundary, never in generic runtime configuration or logs.
2. **Provider to capsule.** Immutable catalog/binary inputs, a dedicated durable
   state root, credential projections, and workspace are mounted with different
   permissions and lifetimes.
3. **Capsule supervisor to Omnigent.** A private Unix socket carries the typed
   API. The loopback listener is an Omnigent implementation detail inside one
   network namespace and is never published or proxied remotely.
4. **Omnigent to harness/backend.** The selected profile grants one credential
   set, sandbox/tool policy, and explicit egress class.
5. **Harness to workspace.** Workspace content is untrusted data and executable
   code. It cannot select control paths, state roots, credentials, or provider
   resources.
6. **Operator to PTY/control plane.** Viewing, input, interrupt, resize, and
   detach are distinct authorized actions. The channel binds every action to one
   city, session, and current runtime incarnation.
7. **Omnigent policy to Gas City mail.** A typed, opt-in request crosses from the
   harness domain into persistent work. Answers are bound to sender, request,
   conversation, and advertised response schema.

### Assumptions and exclusions

- Kubernetes API authentication, node isolation, Secret encryption policy, SSH
  daemon security, and model-provider security are deployment responsibilities.
- Host root, Kubernetes cluster-admin, and a compromised Gas City controller can
  access projected credentials and state; protecting against those authorities
  requires a different confidential-computing design.
- The pinned Omnigent and harness binaries are third-party trusted computing
  base. Gas City verifies exact artifacts but cannot make a malicious approved
  binary safe.
- Agent modification of its assigned workspace is intended. A tool performing
  an operator-authorized destructive edit within that workspace is not itself an
  isolation failure.
- Remote Omnigent managed hosts, hosted control planes, Daytona, collaboration
  links, and a public Omnigent API are excluded architectures, not dormant
  options.

## Attack surface, mitigations, and attacker stories

### Credential projection and profile isolation

**Attack paths:** credential values leak through `runtime.Config.Env`, pod specs,
SSH argv, shell tracing, process listings, profile discovery, Beads metadata,
events, logs, errors, crash dumps, PTY output, or policy mail. A profile obtains
another profile's home directory or a broad Secret containing unrelated keys.
Workspace content overrides `HOME`, config paths, credential variables, or
catalog references. Multiple Claude profiles accidentally share mutable auth.

**Required controls:**

- runtime configuration carries typed secret references, never resolved values;
- Kubernetes projects only named Secret keys into an owner-only profile volume;
- SSH profiles use separate pre-provisioned owner-only credential directories or
  a provider-native reference resolver; secrets never appear in an SSH command;
- each launch receives only the selected profile's credential projection and a
  dedicated `HOME`/config root;
- catalog discovery exposes ID, display name, blurb, harness, backend, network
  class, availability, and fallback IDs only;
- centralized redaction covers environment values and names, auth headers,
  credential paths, URLs with userinfo/query secrets, backend bodies, and
  child-process errors before any log/event/status/TTY diagnostic;
- workspace-controlled environment cannot override reserved capsule variables;
- failover changes the Omnigent agent/profile binding atomically but does not
  co-mount all fallback credentials in the harness process.

**Residual risk:** a selected harness can exfiltrate the credentials it must use
unless the backend offers non-exportable workload identity. Profile isolation
limits the blast radius but cannot protect a credential from its authorized
consumer.

### Socket, loopback, and network exposure

**Attack paths:** the Omnigent server binds wildcard/LAN, a Service or SSH tunnel
publishes it, another container reaches loopback through a shared network
namespace, a stale socket is reused, or a workspace symlink redirects the socket.
The harness uses unrestricted egress for data exfiltration or reaches cloud
metadata, Kubernetes, internal services, or the controller.

**Required controls:**

- the compatibility client uses a Unix socket under the Place-local `0700` run
  root; socket mode is owner-only and peer identity is checked where supported;
- Omnigent may bind only a random loopback port inside the capsule; wildcard,
  configured remote URLs, host networking, Kubernetes Service/Ingress, SSH
  forwarding, and raw remote-city proxying are rejected;
- pod network policy and runtime configuration default-deny ingress and deny
  metadata, Kubernetes API, controller, private-network, and DNS rebinding paths;
- egress is explicit per profile: offline denies all; external-model permits only
  declared DNS/TLS destinations or an administrator-controlled egress proxy;
- service-account token automount is false and the capsule does not receive a
  kubeconfig;
- socket and run roots are outside the workspace and recreated after removing
  only the exact verified stale socket.

**Residual risk:** hostname allowlists depend on DNS and provider endpoint
behavior. Strong deployments use an authenticated egress proxy and namespace or
node isolation. A same-UID hostile process on a shared SSH account can attack a
Unix socket, so supported SSH deployments require an exclusive account or
equivalent OS isolation.

### Workspace, staging, durable state, and artifacts

**Attack paths:** `..`, absolute paths, symlinks, hard links, tar entries, device
files, bind mounts, or rename races escape staging. Workspace content writes the
state root or replaces the pinned executable/catalog. One session mounts another
session's PVC/directory. Logs or artifacts disclose source, prompts, transcripts,
or secrets after close.

**Required controls:**

- use the canonical capsule identity and full-digest ownership metadata from
  `omnigent-remote-identity.md`; name match alone grants no authority;
- workspace, immutable staged inputs, durable state, run root, and credentials
  are separate mounts/directories with the minimum write permissions;
- resolve and verify containment, owner, type, mode, links, and mount identity at
  the provider edge; archive extraction rejects links and special files;
- Kubernetes PVC access is exclusive to the one session; replacement waits for
  detach. SSH state requires a deterministic owner-only directory and rejects
  unexpected symlinks/mounts;
- pinned executable/image and catalog content digests are verified before every
  start, with no auto-install or auto-update;
- durable logs are bounded and redacted; remote status returns aggregates and
  non-secret identifiers rather than raw database, artifact, or log content;
- close/prune retain state and explicit purge verifies terminal state, no live
  attachments, and exact ownership before deleting one allocation.

**Residual risk:** retained transcripts and source remain sensitive until purge;
storage encryption, snapshot/backup policy, and administrator access are runtime
deployment concerns. Content intentionally rendered to a terminal is visible to
authorized viewers.

### Kubernetes control plane

**Attack paths:** label collision adopts or deletes a foreign pod/PVC; malicious
annotations inject commands; broad RBAC allows Secret listing or cross-namespace
exec; init containers or images run privileged; shared service account tokens or
host mounts escape the capsule; replacement mounts state concurrently.

**Required controls:**

- fixed typed pod/PVC builders; no shell fragments derived from names, labels,
  annotations, workspace content, or secrets;
- names plus exact annotations/full digest and owner references must all match;
- namespace-scoped least-privilege RBAC for the Gas City runtime identity;
- no service-account token in capsule pods, no privileged mode, host PID/network,
  hostPath, added capabilities, or writable root filesystem unless a separately
  reviewed harness requirement proves necessary;
- non-root UID/GID, seccomp runtime default, read-only immutable mounts, explicit
  resource limits, and an exclusive durable-state mount;
- Kubernetes exec/attach targets the exact pod UID and current instance-token
  fingerprint, not a reusable pod name alone;
- deletion uses UID/resourceVersion preconditions and never broad selectors.

**Residual risk:** current generic Kubernetes behavior that copies a broad
`claude-credentials` Secret and grants passwordless sudo is incompatible with
remote capsule hardening. Capsule support must use the new typed secret and
security-context path; it may not inherit those defaults.

### SSH command and host boundary

**Attack paths:** host-key bypass enables interception; string-built commands
inject through session/profile/path values; secrets appear in argv; agent
forwarding or ambient SSH environment grants extra credentials; a shared account
reads another capsule; cleanup kills unrelated processes or deletes a broad path.

**Required controls:**

- strict host-key verification with a configured known-hosts source; no
  `StrictHostKeyChecking=no` fallback;
- batch mode, no agent/X11/port forwarding, no remote environment acceptance,
  and bounded connect/keepalive timeouts;
- execute a fixed remote helper with a versioned typed payload over stdin, or use
  positional argv escaped by one audited encoder; never interpolate shell text;
- stage non-secret inputs atomically and by digest; credentials are
  pre-provisioned or resolved by reference on the host;
- require an exclusive remote account or per-session OS sandbox, owner-only
  workspace/state/run roots, and configured absolute bases;
- process discovery proves session ID plus instance token from live process
  environment; cleanup signals exact process groups and the exact tmux socket;
- purge accepts only a derived capsule identity and refuses arbitrary paths.

**Residual risk:** remote root and the selected account can observe process
memory and files. Shell startup files under that account are privileged config
and must be administrator-controlled.

### Harness output, logs, and terminal control

**Attack paths:** output forges structured status/failover/policy events, embeds
terminal escapes or clickable credential URLs, floods memory/storage, or causes
an observer to send input to the wrong reincarnation. A detached viewer changes
desired lifecycle. Transcript APIs disclose history without authorization.

**Required controls:**

- parse only typed Omnigent protocol frames from the authenticated private
  channel; never parse model prose or terminal text as control events;
- bound frame, line, stream-buffer, log, and replay sizes; preserve ordering and
  surface truncation explicitly;
- sanitize terminal control sequences for non-PTY logs/status while preserving
  raw bytes only on an authorized interactive PTY path;
- remote TTY authorization is checked at open and on every control action; bind
  the channel to city, session bead ID, pod UID/SSH host, instance token, tmux
  socket/session, principal, and expiry;
- viewing and detach are lifecycle-neutral; input, interrupt, and resize require
  explicit action grants; disconnect releases only the viewer;
- audit connect/disconnect/control actions with secret-free fingerprints;
- transcript/status APIs apply the same session authorization and never return
  credentials, agent specs, raw errors, or unbounded artifacts.

**Residual risk:** an authorized interactive viewer sees source and conversation
content and can submit agent input. Authorization must therefore be narrower than
general city read access.

### Policy mail

**Attack paths:** forged output creates a policy request, replay produces
duplicate mail, an answer crosses conversations, an attacker impersonates the
configured recipient, or malformed/oversized content reaches the mail store.

**Required controls:** policy mail is disabled unless the profile explicitly
opts in. Only a typed Omnigent policy event can create a request. The durable
record binds conversation, profile, request, mail, recipient, allowed actions,
and an idempotency key. Replies require the exact authorized sender and strict,
bounded JSON schema. Unknown, canceled, expired, duplicate-with-different-answer,
or cross-session responses fail visibly. No default approval, denial, recipient,
or timeout verdict exists.

**Residual risk:** a compromised harness can ask misleading questions within
the configured policy schema; the human/agent recipient remains responsible for
the answer.

### Lifecycle, cleanup, and denial of service

**Attack paths:** controller crash leaves tmux, server, host, monitor, or harness
processes; deleted socket prevents cleanup; bare `tmux kill-server` kills personal
sessions; stale resources are adopted; rapid retries create process/pod/PVC
storms; output or artifacts exhaust disk; purge races restart.

**Required controls:**

- register cleanup after temp/run roots exist so LIFO ordering preserves the
  socket while socket-based shutdown runs;
- capture exact server/monitor/harness PIDs and process groups at spawn and use
  direct signals as the guaranteed cleanup path even when the socket is gone;
- tmux cleanup always names the verified city/capsule socket and session; set
  `exit-empty`/`destroy-unattached` where compatible as defense in depth;
- provider cleanup uses pod UID/resourceVersion or SSH process identity plus
  instance-token fences; never a name, selector, or process substring alone;
- reconciliation is idempotent, rate-limited, backoff-bounded, and adopts at most
  one fully verified resource; ambiguity stops mutation;
- resource limits, quotas, bounded logs/artifacts, PVC capacity checks, and
  circuit breakers surface exhaustion without retry storms;
- purge locks the terminal closed session, proves no live Place/attachment, and
  commits deletion before clearing its conversation binding.

**Residual risk:** hard host loss can leave unreachable retained state or
processes until the runtime administrator restores access. Gas City reports
uncertainty and does not assume absence.

## Security invariants

The implementation is not releasable unless all are true:

1. No public, wildcard, remotely configured, Service-backed, Ingress-backed, or
   SSH-forwarded Omnigent listener exists.
2. No credential literal or credential identifier/path appears in runtime env
   metadata, Kubernetes/SSH command text, Beads, events, logs, status, doctor,
   profile discovery, policy mail, or release artifacts.
3. Every provider path is derived beneath a configured trusted base and verified
   against links, ownership, mode, and canonical capsule digest.
4. Network ingress is denied and profile egress is explicit; capsule access to
   metadata, Kubernetes API, controller, and private networks is denied.
5. Start, adopt, attach, cleanup, and purge require exact canonical identity plus
   current incarnation evidence. No broad kill or delete operation exists.
6. A stored conversation binding with missing or unverifiable state fails closed
   and never creates a replacement.
7. Remote attachment is generic Gas City TTY, authorized per session and action,
   incarnation-bound, audited, bounded, and lifecycle-neutral.
8. Workspace/model/harness output cannot become a control message without typed
   protocol validation and explicit authorization.

## Security test plan

| Area | Required tests |
|---|---|
| secret leakage | canary credentials across success and every failure path; scan argv, env metadata, pod JSON, SSH transcripts, process list, logs, events, Beads, status/doctor JSON, mail, PTY diagnostics, archives, SBOM, and image layers |
| profile isolation | independent Codex and multiple Claude homes/secrets; selected process sees one profile only; failover does not expose the other credential to the old harness |
| socket/listener | enumerate listeners inside/outside capsule; reject wildcard/remote URL/Service/Ingress/forwarding; stale socket and wrong-owner socket fail safely |
| egress | offline profile cannot reach DNS, Internet, metadata, API, controller, or private ranges; external profile reaches only declared backend through policy |
| containment | traversal, absolute paths, symlink/hard-link/mount races, malicious tar entries, device files, workspace attempts to replace catalog/binary/state/socket |
| identity/isolation | sanitizer collisions, wrong digest/labels/UID/instance token, cross-session PVC and SSH directory, concurrent create/attach/purge, alias and worktree changes |
| Kubernetes | fake-client manifest assertions plus kind/live tests for RBAC, token absence, security context, NetworkPolicy, Secret key projection, PVC exclusivity, pod replacement, UID-precondition cleanup |
| SSH | hostile names and payloads, strict host-key failure, disabled forwarding, no secrets in command/process list, shared-account rejection, disconnect/reconnect, direct PID/process-group cleanup |
| protocol/output | malformed, forged, oversized, reordered, duplicated, and secret-bearing frames; terminal escape handling; bounded replay/backpressure and disconnect races |
| remote TTY | unauthorized read/input/interrupt/resize, expired/replayed grants, wrong incarnation, cross-city/session confusion, slow reader, abrupt disconnect, viewer-neutral lifecycle |
| policy mail | opt-in gate, forged prose rejection, wrong sender/request/conversation, replay/idempotency, schema/size limits, cancellation and crash recovery |
| cleanup/DoS | controller crash, socket deletion, tmux tempdir cleanup ordering, server and monitor PID fallback, pod/SSH orphan collection, retry storm bounds, log/disk quotas |
| continuity | lost/multiple/invalid state with a stored conversation fails closed; replacement resumes exact conversation; explicit reset alone authorizes fresh creation |

Tests use canary values, hermetic listeners, fake provider clients, and isolated
tmux sockets by default. Opt-in live tests use dedicated Kubernetes namespaces
and SSH accounts and record only redacted evidence. Test cleanup asserts that no
matching process, tmux server/monitor, pod, attachment, temporary Secret, or
ephemeral run directory remains. Durable allocations remain unless the test
explicitly exercises verified purge.

## Severity calibration

- **Critical:** cross-tenant arbitrary command execution through the controller;
  unauthenticated remote TTY input across cities; broad cleanup/purge that can
  destroy unrelated host or cluster resources; release artifact compromise.
- **High:** another capsule reads credentials or durable state; a public
  Omnigent listener permits harness control; path escape writes outside the
  assigned roots; Kubernetes/SSH injection gains host or namespace authority;
  conversation continuity silently switches to attacker-controlled state.
- **Medium:** authorized city reader obtains transcript beyond its session grant;
  credentials leak only to restricted operator logs; missing bounds permit one
  capsule to exhaust its namespace/host allocation; policy replay creates a
  duplicate but non-authorizing request.
- **Low:** non-secret profile blurb or backend label disclosure; a contained
  orphan process with no credential/state crossover; diagnostics expose a
  session alias already visible to the same authorized operator.

Severity is reduced when exploitation requires cluster-admin, remote root, or a
compromised Gas City controller because those actors already hold equivalent
authority. It is not reduced for attacks originating in workspace content,
model output, harness output, another ordinary capsule, or a read-only remote
viewer: those are explicit untrusted boundaries.
