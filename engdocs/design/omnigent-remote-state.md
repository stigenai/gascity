# Omnigent remote capsule state

**Status:** accepted lifecycle and retention contract

**Issue:** `ga-xgv.1.2`

**Scope:** remotely placed Omnigent workers on Kubernetes and SSH

## Decision

An Omnigent capsule has two independent lifetimes: the Gas City session and its
current runtime Place. Durable Omnigent state belongs to the session and survives
Place teardown or replacement. Ephemeral process state belongs to the Place and
is always recreated.

Gas City owns the durable-state allocation, attachment, retention, and deletion
policy. Omnigent owns the contents and schema of its database, host identity,
configuration, conversation records, artifacts, and logs. Gas City treats those
contents as opaque. Beads stores the session lifecycle and the opaque
`conv_*` binding; it does not duplicate the Omnigent database.

`Runtime.Teardown` means "remove this Place." It never means "delete the
session's durable Omnigent state." Permanent deletion is a separate explicit
purge operation.

## State inventory and owners

| State | Logical owner | Authoritative location | Lifetime | Gas City behavior |
|---|---|---|---|---|
| session lifecycle and opaque conversation binding | Gas City | Beads/DoltLite | through session retention | reads and updates typed session metadata; never infers a replacement conversation |
| Omnigent database | Omnigent | session durable root | through explicit purge | allocates storage and mounts it; never edits database contents |
| Omnigent host identity | Omnigent | session durable root | through explicit purge | preserves byte-for-byte across Place replacement |
| generated Omnigent config | Omnigent, from Gas City inputs | session durable root | through explicit purge | stages inputs separately and lets the pinned binary validate or regenerate config atomically |
| conversation artifacts | Omnigent | session durable root | through explicit purge | retains with the database and exposes no implicit download path |
| Omnigent service logs | Omnigent | session durable root, with configured bounded rotation | through explicit purge | preserves diagnostic history while enforcing provider-independent size/age limits |
| profile catalog | Gas City configuration | immutable staged input, outside the writable durable root | current launch generation | renders non-secret profile definitions and verifies their digest before launch |
| credential values | runtime secret facility or pre-provisioned SSH account | provider edge only | external secret policy | projects typed references for the launch; never persists values in Beads, catalog, logs, or runtime environment metadata |
| private Unix socket and run directory | capsule supervisor | Place-local ephemeral directory | current supervisor process | recreates with owner-only permissions and deletes on shutdown; never uses it for discovery |
| child processes and tmux server | Gas City capsule supervisor | provider process table and tmux socket | current Place | tracks exact child process groups; discovery uses live provider queries, not PID files |
| durable-state allocation metadata | Gas City runtime provider | provider-native resource metadata | through explicit purge | labels resources with canonical city/session identity and reconciles them from provider ground truth |

The **session durable root** is one exclusive writable allocation per canonical
Gas City session identity. It is never shared between sessions, even when two
sessions use the same agent template or authentication profile. Kubernetes uses
a deterministically labelled persistent volume claim. SSH uses a deterministic,
owner-only directory beneath an administrator-configured state base. Exact names
are defined by `ga-xgv.1.3`; neither provider accepts a path supplied by workspace
content.

## Lifecycle transition table

"Retain" below means that the allocation remains discoverable and unmodified;
it does not require a live Place. "Detach" means that the Place releases its
mount or directory handle before the Place is removed.

| Operation or event | Session state | Place/process action | Durable state | Conversation binding | Resume behavior |
|---|---|---|---|---|---|
| create session | `creating` to `active` | provision Place, attach exclusive state, stage catalog, start supervisor | create empty allocation if absent; fail if conflicting ownership exists | initially empty | create a conversation only after storage, pin, catalog, and credentials validate; persist its ID before accepting interaction |
| sleep | `active` to `asleep` | stop harness and capsule children; the provider may keep the Place | retain attached or safely detach | preserve | next wake resumes the exact stored ID when `wake_mode=resume`; fresh rules apply when configured |
| suspend | `active`/`asleep` to `suspended` | stop children, detach state, and permit Place teardown | retain | preserve | reprovision or adopt a Place, reattach the same allocation, then resume exactly |
| drain | `active` to `drained` | finish current work, stop children, and permit Place teardown | retain | preserve unless an already-committed fresh-wake transition clears it | qualifying wake resumes exact state; fresh reassignment starts a new binding |
| city stop | lifecycle state is unchanged | stop capsule children and all city-owned Places according to normal shutdown | retain | preserve | city restart reconciles state and resumes only on demand |
| explicit session reset | session identity is preserved; restart is queued | stop old children and rotate the active conversation generation | retain historical Omnigent records; do not delete allocation | clear through the existing atomic reset patch | next start creates a new conversation; old conversation is not selected implicitly |
| restart or crash recovery | state remains continuity-eligible | adopt a healthy matching Place or replace it; restart the supervisor | retain and reattach | preserve | exact resume; no creation fallback when a binding exists |
| Kubernetes pod replacement | state remains continuity-eligible | wait for exclusive PVC detach, create replacement pod, remount | retain on the same PVC | preserve | exact resume after ownership and database validation |
| SSH connection loss or host process replacement | state remains continuity-eligible | reconnect, verify account and directory ownership, restart supervisor | retain in the same remote directory | preserve | exact resume after host identity and database validation |
| Gas City orchestrator crash | state remains unchanged in Beads | on restart, query provider and reconcile Place/process truth | retain | preserve | adopt exactly one valid Place or fail on ambiguity; exact resume if a restart is needed |
| close session | `closed` (terminal) | stop children, detach state, teardown Place | retain as closed state | preserve for audit; never resume a closed session | none; reopening is prohibited |
| prune eligible session | `closed` (terminal) | same as close | retain as closed state | preserve for audit | none; prune does not purge |
| explicit purge | remains `closed` | require no live Place or children; refuse otherwise | delete the one verified allocation and all Omnigent contents | clear only after deletion succeeds | none; operation is idempotent after a recorded successful purge |
| orphan collection | session state unchanged unless normal reconciler marks it orphaned | stop only resources proven to belong to the session | retain by default | preserve | valid state may be adopted later; collection never purges |

`archived` and `orphaned` sessions follow the same rule as `suspended`: retain
state and preserve the binding while the existing session model considers them
continuity-eligible. A terminal `closed` session cannot be woken even while its
state is retained.

## Create, resume, and reset protocol

Capsule startup is ordered so a partially successful launch cannot manufacture
or lose conversation identity:

1. Resolve one canonical session and one provider runtime.
2. Discover or allocate exactly one durable root labelled for that session.
3. Attach it exclusively and verify owner, permissions, host identity, database,
   pinned executable, catalog digest, and required secret references.
4. Create a private Place-local run directory and socket.
5. Start the pinned Omnigent server and host as exact child process groups.
6. If Beads contains a conversation binding, ask Omnigent to resolve that exact
   ID. Any missing or mismatched result fails the launch.
7. If the binding is empty, create one conversation, atomically bind the returned
   ID, then expose the attachment. If another contender won the binding race,
   stop the losing new conversation and resolve the winner exactly.

`wake_mode=resume`, runtime restart, provider reconnection, pod replacement, and
orchestrator recovery all follow this protocol without clearing the binding.
`wake_mode=fresh`, a committed config-drift transition, or `gc session reset`
uses the existing session lifecycle patch to clear the active binding before the
next launch. Clearing the binding authorizes creation of one new conversation;
it does not authorize deletion of previous records.

## Fail-closed state loss

A non-empty conversation binding is a continuity claim. The following conditions
fail closed and leave the binding intact:

- no durable allocation is found;
- multiple allocations claim the same session;
- the allocation exists but cannot be attached exclusively;
- database, host identity, owner, permissions, or canonical labels do not match;
- Omnigent cannot resolve the bound conversation from that state;
- the profile catalog digest or pinned executable does not match the launch plan;
- a required secret reference cannot be resolved.

None of these failures may create a new allocation, conversation, profile,
workspace, or local fallback. Status must distinguish missing state, ambiguous
state, invalid state, unavailable credentials, and transport failure. Recovery
is an operator action: restore the expected state, explicitly reset the session
to abandon continuity, or close and purge it.

When the binding is empty, an existing non-empty durable database is not evidence
that a particular conversation should be selected. Omnigent may create a fresh
conversation only after the startup checks pass. Gas City never guesses from the
database contents.

## Discovery and reconciliation

Discovery is deterministic and provider-native:

- start from open and retained session beads, then derive the canonical capsule
  identity;
- Kubernetes lists resources by the Gas City city/session labels and verifies
  owner references, PVC identity, and attachment state;
- SSH computes the state directory from the configured base plus canonical
  identity, then verifies the remote account, directory owner, mode, and embedded
  Omnigent host identity;
- runtime liveness comes from Kubernetes objects, remote process tables, and
  exact tmux socket/session queries;
- Omnigent liveness comes from the private socket and protocol health check.

No PID file, mounted/status marker, completion sentinel, or ad hoc state file is
created. Provider resource metadata, Beads, the process table, tmux, and the
Omnigent protocol are the sources of truth.

Reconciliation classifies each resource as exactly one of:

- **active:** uniquely matches an open session and its current Place;
- **retained:** uniquely matches a known non-purged session but has no live Place;
- **orphan process:** has verified session ownership but no valid desired Place;
- **orphan allocation:** has verified canonical ownership but no corresponding
  session bead;
- **ambiguous or foreign:** missing, conflicting, or unverifiable ownership.

The collector may stop an orphan process after proving exact ownership. It keeps
retained and orphan allocations. Ambiguous or foreign resources are reported and
never modified automatically.

## Garbage collection and purge

Retention is intentionally conservative because session close and prune are
workflow operations, not data-destruction requests. Automatic reconciliation
never deletes Omnigent durable state.

`gc session purge <identity>` is the eventual destructive surface. It is valid
only for a uniquely resolved, terminal `closed` session. The implementation must:

1. resolve the session and canonical allocation without accepting arbitrary
   provider names or filesystem paths;
2. prove there are no live Places, tmux sessions, supervisor children, or storage
   attachments for that session;
3. verify the allocation's city/session ownership and reject ambiguity;
4. delete only that allocation using the provider API;
5. record successful purge in Beads and clear the opaque conversation binding;
6. return success if that same recorded purge is repeated.

Deletion failure leaves the binding and non-purged metadata intact so retry is
safe. A missing allocation without a prior successful purge is reported as state
loss, not silently treated as success. Bulk retention policy, if added later,
must select already-closed sessions and invoke the same verified purge operation;
it gets no separate deletion path.

## Consequences for provider APIs

The runtime seam needs an explicit durable-state capability separate from Place
provisioning and teardown. Kubernetes and SSH must both prove the shape before a
shared interface is accepted. At minimum, provider implementations need typed
operations equivalent to discover/ensure, attach, detach, inspect, and purge,
all keyed by canonical capsule identity. Secret projection is a separate typed
capability and never becomes a bag of environment values.

Place teardown remains safe for ordinary runtimes because it cannot reach the
durable-state deletion operation. This also makes controller crash recovery
idempotent: reconciliation can repeat discovery, adoption, detach, teardown, and
startup without consuming conversation continuity or deleting state.
