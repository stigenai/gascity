# Omnigent remote capsule identity

**Status:** accepted identity contract

**Issue:** `ga-xgv.1.3`

## Decision

The Gas City session bead ID is the durable logical identity of an Omnigent
capsule. Display aliases, agent templates, runtime session names, worktree paths,
runtime incarnations, and Omnigent conversation IDs are attributes of that
capsule; none may replace or rename it.

Every physical provider identifier is derived from a typed capsule key:

```text
CapsuleKey {
  version:    1
  city_scope: provider-configured city scope
  session_id: exact Gas City session bead ID
}
```

`city_scope` is an immutable provider namespace, not a display name or local
city path. For Kubernetes it is the configured cluster plus namespace binding.
For SSH it is the configured host/account plus state base. Providers must reject
an empty or mutable scope when durable capsules are enabled. The canonical byte
encoding is version, NUL, city scope, NUL, and session ID as UTF-8. Invalid UTF-8,
NUL, an empty component, or an unsupported version is rejected.

## Logical and physical mapping

The implementation exposes one tested identity constructor. Callers do not
independently sanitize or concatenate user input.

| Concern | Canonical value | Physical representation |
|---|---|---|
| capsule logical key | `(version, city_scope, session_id)` | typed value passed between lifecycle and runtime boundaries |
| collision token | SHA-256 of canonical key bytes | first 26 lowercase Base32 characters, giving 130 bits; full digest retained for ownership verification |
| human hint | session ID | lowercase DNS/path-safe prefix, trimmed to 20 bytes, never used for equality |
| resource stem | logical key | `gco-<hint>-<token>` when the hint is non-empty, otherwise `gco-<token>`; maximum 51 bytes |
| runtime Place | current session incarnation | existing runtime name plus `instance_token`; replaceable and not durable identity |
| workspace mount | runtime contract | `/workspace`; selected rig workdir remains a contained path beneath it |
| durable state mount | capsule key | `/var/lib/gascity/omnigent`; a separate volume or bind mount, never beneath workspace |
| run directory | current Place | `/run/gascity/omnigent/<token>` with mode `0700` |
| private socket | current Place | `<run-directory>/service.sock`; Unix path length checked before launch |
| outer tmux session | current Place | existing Gas City runtime session name with exact instance-token ownership fence |
| Omnigent host identity | capsule key | durable Omnigent-owned record inside the state root, verified against the full digest |
| conversation | opaque Omnigent ID | Beads `session_key`/`conv_*` binding plus Omnigent database record; never part of a resource name |
| Kubernetes pod | Place incarnation | current provider pod name; labelled with full capsule digest, session ID when label-safe, and instance-token fingerprint |
| Kubernetes PVC | capsule key | resource stem; annotated with full digest, exact session ID, identity version, and city scope fingerprint |
| SSH workspace | Place contract | provider-configured workspace base plus a provider-owned Place component |
| SSH durable directory | capsule key | `<state-base>/omnigent/v1/<token>`; mode `0700`, exact remote account owner |
| staged catalog | launch generation | immutable file beneath a Place-local staging root, named by content digest |

The human hint is diagnostic only. Two keys with the same hint remain distinct
because their collision tokens differ. A token collision, full-digest mismatch,
or resource whose annotations disagree with its derived name is an identity
conflict and fails closed.

## Path containment

All roots come from trusted provider configuration. Joining follows these rules:

1. Derive only fixed literals and the Base32 token from the canonical key.
2. Join with provider-native path functions.
3. clean and resolve the configured base and final parent, including symlinks on
   SSH hosts;
4. require the final path to be a strict descendant of the configured base;
5. create with restrictive permissions and verify owner and mode after creation;
6. reject pre-existing symlinks, non-directories, hard-link surprises, mounts,
   or a different full identity digest.

Workspace content, aliases, profile names, conversation IDs, repository names,
and environment values never contribute a path component. Archive extraction
and staging separately reject absolute paths, `..`, device files, links escaping
the destination, and writes into the state or run roots.

## Stability and mutation rules

| Event | Identity result |
|---|---|
| session alias rename | no change; presentation changes only |
| agent template rename or pack update | no change; launch plan may change after normal config-drift handling |
| runtime retry | same capsule key; new Place incarnation and instance token |
| concurrent creation | contenders derive the same key; provider-native create-if-absent plus full-digest compare selects one allocation; only one may attach |
| worktree path changes | same capsule and state; next Place stages or mounts the requested workspace only after containment validation |
| repository or rig rename | no state rename; workdir projection changes explicitly |
| city display-name rename | no change when the provider city scope is unchanged |
| Kubernetes pod replacement | same PVC and capsule key; new pod/Place incarnation |
| SSH reconnect | same remote directory and capsule key |
| provider config changes namespace, host/account, or state base | a different city scope; startup fails with relocation required |
| Kubernetes to SSH or SSH to Kubernetes migration | not implicit; requires an approved offline relocation that preserves the exact capsule key and verifies a content manifest |
| closed session followed by a newly created session with the same alias | new bead ID, therefore a different capsule key and allocation |
| explicit purge then retry | same key remains addressable as purged metadata, but no allocation may be recreated for the closed session |

A stored `instance_token` fences the current runtime incarnation but is
deliberately excluded from durable identity. A stored conversation ID proves
continuity inside the capsule but is deliberately excluded from provider
identity. This prevents ordinary restart and reset behavior from moving or
renaming storage.

## Relocation contract

Cross-scope migration is offline and explicit. The initial implementation may
reject it. Any later implementation must:

1. stop and fence every Place for the session;
2. lock the session against wake and purge;
3. snapshot the source allocation and create a typed manifest containing the
   full capsule digest, identity version, pinned Omnigent version, and hashes of
   all regular files without secret values;
4. copy into a new empty destination through provider-owned transport;
5. verify the manifest, permissions, Omnigent host identity, and exact bound
   conversation before changing the provider binding;
6. retain the source until one successful destination resume is recorded;
7. purge the source only through the normal ownership-verified purge path.

Failure before the provider-binding commit leaves the source authoritative.
Failure after commit does not fall back automatically; reconciliation reports
the incomplete relocation and requires recovery. Live dual attachment is never
allowed.

## Required tests

Identity tests are table-driven and use the same constructor for Kubernetes and
SSH. They cover:

- empty, Unicode, punctuation-only, very long, case-distinct, and separator-rich
  session IDs and city scopes;
- deterministic output across processes and supported operating systems;
- DNS-1123 length and character compliance for resource stems;
- path containment against `..`, absolute paths, symlinks, hard links, and
  malicious workspace/catalog entries;
- distinct keys that sanitize to the same human hint;
- full-digest mismatch and simulated short-token collision;
- alias/template/city display rename stability;
- new bead identity after close, retry reuse, concurrent ensure and exclusive
  attachment;
- worktree and rig-path changes without state relocation;
- pod replacement and SSH reconnect retaining the same allocation;
- cross-scope and cross-runtime changes failing with relocation required;
- interrupted relocation before and after the provider-binding commit.

Provider conformance tests assert that discovery returns a resource only after
both the derived key and provider metadata match. A name match alone never grants
adoption, attachment, cleanup, or purge authority.
