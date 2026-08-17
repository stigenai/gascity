# Omnigent remote capsules

**Status:** accepted execution boundary; implementation tracked by `ga-xgv`

**Depends on:** completed local integration `ga-ou2`

**Omnigent pin:** `2aba5079d4d3a2a84d8c9927884fc4b8ce0eeecc`

## Decision

Gas City remote worker placement and remote-city administration are separate
surfaces. The first Omnigent remote milestone extends **worker placement** to
Gas City's Kubernetes and SSH runtimes. It does not make the private local
Omnigent service reachable through the remote-city HTTP control plane.

For a remotely placed worker, Gas City creates or adopts the execution place,
assigns the workspace, projects durable state and credential references, and
launches the visible outer tmux session. The command in that session runs a
capsule-local Omnigent server and local host on loopback, connects through a
private Unix socket, and renders the existing interactive attachment client.
Omnigent therefore remains local relative to the place where its harness runs.

```text
Gas City controller
  |
  +-- Runtime.Provision: Kubernetes pod or SSH host
  |       |
  |       +-- Gas City-assigned workspace and durable state
  |       +-- outer tmux session
  |               |
  |               +-- capsule-local attachment process
  |                       |
  |                       +-- pinned Omnigent server (loopback)
  |                       +-- pinned Omnigent host (same place)
  |                       +-- private Unix socket
  |                       +-- Codex or configured Claude profile
  |
  +-- Beads session projection: opaque Omnigent conversation ID only
```

An operator selecting a city through `--city-url`, `--context`, or the matching
environment variables uses a different trust boundary. That client may use
typed remote-city API operations. It may not obtain a raw proxy to a private
workspace service. Generic remote terminal control is brokered by Gas City at
`/v0/city/{cityName}/session/{id}/terminal`: bounded snapshots use an opaque
conditional reconnect cursor, while typed mutations send literal bytes,
logical keys, resize, interrupt, or clean detach. The same capability serves
every attachable harness; it is not an Omnigent-specific public tunnel.

## Why the split is required

The local Omnigent composition has three properties that must remain true:

1. The service endpoint is private and loopback-derived. `LocalServiceProxy`
   adds an internal-request header and deliberately refuses remote clients.
2. The Omnigent host launches the harness in the same filesystem namespace as
   the assigned workspace. Moving only the attachment client would execute the
   harness on the controller and defeat remote placement.
3. Gas City is the only placement and outer-lifecycle authority. Connecting an
   Omnigent host to a networked Omnigent server would reintroduce the remote
   Omnigent control plane explicitly excluded by `ga-ou2`.

Passing the controller's private socket, publishing its HTTP equivalent, or
using `omnigent host --server <remote-url>` are therefore not compatibility
shortcuts. They are different architectures and are rejected.

## Surface and capability matrix

| Operation | Local city | Remotely placed worker | Remote-city client |
|---|---|---|---|
| select runtime place | local tmux or Herdr | Gas City Kubernetes or SSH runtime | typed controller request only |
| start Omnigent server and host | city-scoped workspace service | capsule-local supervisor in the place | not allowed |
| access private Omnigent API | loopback city service proxy | private capsule Unix socket | not allowed |
| create or resume conversation | `gc omnigent attach` | capsule-local attach mode | through worker lifecycle, not raw Omnigent API |
| interactive terminal from controller host | tmux or Herdr attach | Kubernetes exec/tmux or SSH/tmux | authenticated typed terminal broker; no provider endpoint disclosure |
| read service status | local typed status and doctor | controller observation of the place | generic typed remote service/session status |
| durable work and conversation binding | Beads/DoltLite | same authoritative store | typed controller API |
| Omnigent database and artifacts | city service state | durable state owned by the Gas City session | never downloaded implicitly |

The remote capsule needs the following Gas City runtime facts. Downstream tasks
define the final types and provider implementations:

- `Runtime` provisions and reopens the place and discovers it by session name.
- `Place.Exec` drives tmux and performs factual health checks.
- `Place.Stage` supplies non-secret catalog and agent definitions.
- the tmux `Transport` supplies input, output, interrupt, and controller-local
  interactive attachment;
- durable capsule state outlives a transient place replacement;
- typed secret references resolve at the provider edge without placing secret
  values in `runtime.Config.Env`.

The concrete handoff is `runtime.CapsuleLaunchConfig`. It is created only after
the attachment plan and provider-owned durable allocation agree on the capsule
key. It carries a shell-free command, opaque state/catalog resource IDs, fixed
in-capsule mount paths, private socket path, and catalog digest. The Kubernetes
pod projection keeps the existing single agent container and outer tmux
session, mounts the exclusive PVC, a memory-backed run directory, and a
required read-only catalog ConfigMap separately, and gates readiness on both
tmux and the supervisor socket. A nil capsule plan leaves ordinary pod manifests
on the existing path.

Kubernetes and SSH provide the two concrete implementations required before a
provider-neutral state or secret seam is accepted. Daytona remains a possible
future Runtime Provider Protocol implementation; it is not a current built-in
runtime and adds no Omnigent configuration surface.

## Ownership and trust boundaries

| Concern | Authority | Remote source of truth |
|---|---|---|
| runtime selection and placement | Gas City | configured Runtime plus live provider state |
| workspace path | Gas City | provisioned Place |
| outer terminal | Gas City tmux Transport | live tmux session in the Place |
| service and harness child processes | capsule-local Gas City supervisor | exact child process groups |
| harness, model/backend, auth use, tool policy, sandbox | Omnigent | selected local profile and conversation |
| credential material | runtime secret store or pre-provisioned SSH host | provider edge; values are never controller domain data |
| work, session, and opaque conversation binding | Gas City | Beads/DoltLite session projection |
| transcript, active profile, failover labels | Omnigent | durable Omnigent conversation state |
| remote operator authorization | Gas City control plane | typed API authentication and grants |
| viewer attachment | tmux or lifecycle-neutral Herdr viewer | live Transport attachment; never desired lifecycle |

Workspace content is untrusted. It cannot choose the state root, socket,
profile catalog, executable, Kubernetes secret reference, SSH credential home,
or remote endpoint. A remote operator is also outside the capsule trust
boundary: authorization to inspect or control a Gas City session does not imply
direct access to Omnigent internals or credential-bearing state.

## Required failure behavior

The following cases fail explicitly and never fall back to another location:

- `gc omnigent attach` with a selected remote city fails at local-context
  resolution before looking up a controller-local service.
- `Client.LocalServiceProxy` on a remote client returns an error without issuing
  an HTTP request.
- a remotely placed worker without capsule-local Omnigent does not contact the
  controller's loopback service and does not run its harness on the controller.
- an unavailable private socket is reported as unavailable, not as an absent
  conversation and not as permission to create a replacement conversation.
- a runtime without required staging, durable-state, secret-reference, or
  interactive capabilities reports the missing capability.
- a runtime without `runtime.TerminalProvider` returns the typed
  `terminal-unsupported` capability error; no API path falls back to a raw
  provider attach or exposes provider connection details.

Provider transport errors, missing state, missing credentials, pin mismatch,
and authorization failures remain distinguishable. No path silently selects a
local city, a fresh conversation, a different profile, or an Omnigent managed
host.

## Executable evidence

The boundary is guarded at both entry points:

- `internal/api.TestClientLocalServiceProxyRejectsRemoteCityWithoutRequest`
  constructs a real remote city client and proves the private proxy rejects it
  before any request reaches the server.
- `cmd/gc.TestOmnigentAttachRejectsRemoteCityBeforeLocalServiceLookup` selects a
  named remote context and proves attachment fails during local-city resolution
  rather than falling through to controller or service discovery.
- existing `TestResolveContext_RemoteGatedByDefault` proves commands using the
  local-only resolver never downgrade a selected remote target to filesystem
  discovery.

These tests protect the current negative contract. Capsule-local attachment,
durable state, secret projection, Kubernetes and SSH execution, Herdr viewing,
and generic remote TTY each receive positive contract and live tests in their
own dependency-ordered `ga-xgv` tasks.

## Delivery boundaries

The worker-placement MVP is complete only when Kubernetes and SSH both run the
pinned server, local host, and harness inside their Gas City-selected place;
retain exact conversation state across supported replacement; and attach
through the authoritative outer tmux session. Remote-city status and TTY work
remain generic Gas City control-plane capabilities and must not weaken the
private service boundary while they are developed.

The lifecycle table, canonical capsule identity, detailed threat model, and
test matrix are owned respectively by `ga-xgv.1.2` through `ga-xgv.1.5`. This
document fixes the placement/control distinction those decisions build on.
