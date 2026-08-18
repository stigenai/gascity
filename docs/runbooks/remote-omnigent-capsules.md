---
title: Operate Remote Omnigent Capsules
description: Run pinned Omnigent profiles on Kubernetes or SSH, inspect retained state, attach through Herdr, and recover without losing conversations.
---

Remote Omnigent capsules let Gas City place a Codex or Claude-compatible Agent on Kubernetes or SSH while preserving its conversation across pod or process replacement. Gas City remains the placement and lifecycle authority. Omnigent runs in local-config mode inside the selected place and owns the harness, model, authentication, and conversation lifecycle.

Start with [Run Agents Through Local Omnigent](/guides/local-omnigent) to create the pinned profile catalog and import the Omnigent pack. This runbook covers the remote runtime and recovery steps.

## Prepare the runtime

The executable named by `<city>/.gc/services/omnigent/config/profiles.yaml` must already exist in the Kubernetes image or on the SSH host. Its version, reviewed commit, and SHA-256 must match the catalog exactly. A mismatch stops the session before a harness starts.

Each catalog profile names one or more `secret_references`. Give every profile a distinct reference ID and backing credential. Gas City passes only the reference metadata during planning. Kubernetes resolves a Secret; SSH resolves an owner-readable path already present on the host.

For Kubernetes, create the namespace, scoped service account/RBAC, NetworkPolicy, and profile Secrets before starting the city. Keep each authentication home or token in its own Secret:

```bash
kubectl --context <context> -n <namespace> create secret generic claude-primary-auth \
  --from-file=.credentials.json=/secure/claude-primary/.credentials.json

kubectl --context <context> -n <namespace> create secret generic claude-secondary-auth \
  --from-file=.credentials.json=/secure/claude-secondary/.credentials.json
```

For SSH, provision separate paths under an administrator-owned secret root. The remote account must own them; directories must not be group- or world-writable, and files must be owner-readable only.

```bash
ssh agent@worker.example 'test -r /srv/gascity/secrets/claude-primary && \
  test -r /srv/gascity/secrets/claude-secondary'
```

Do not place credential values in `city.toml`, provider `env`, the profile catalog, shell arguments, or workspace files.

## Configure one profile per Agent

Provider aliases select profiles without changing formulas or beads. This example supplies both Kubernetes and SSH sources so the same Agent definition can run under either remote runtime:

```toml
[providers.omnigent-claude-primary]
base = "provider:omnigent"

[providers.omnigent-claude-primary.option_defaults]
profile = "claude-primary"

[[agent]]
name = "remote-reviewer"
provider = "omnigent-claude-primary"

[[agent.secret]]
id = "claude-primary-home"
environment = "CLAUDE_CONFIG_DIR"
mount_path = "/run/gascity/omnigent/credentials/claude-primary"

[agent.secret.kubernetes]
name = "claude-primary-auth"
key = ".credentials.json"

[agent.secret.ssh]
path = "/srv/gascity/secrets/claude-primary"
```

The `id` must match the selected catalog profile's `secret_references` entry. Repeat the provider and Agent block for `claude-secondary`, using a different ID, Secret, mount path, and SSH path. The profile's display name, blurb, harness, and backend remain non-secret status metadata, so a Claude-compatible backend does not need to identify itself as Anthropic.

Select Kubernetes city-wide:

```toml
[session]
provider = "k8s"

[session.k8s]
context = "<context>"
namespace = "<namespace>"
image = "<registry>/gascity-agent:<release>"
cpu_request = "500m"
mem_request = "1Gi"
cpu_limit = "2"
mem_limit = "4Gi"
```

The released `gascity-agent` image contains the pinned runtime tools. Leave
`prebaked` at its default `false` so Gas City stages the selected city. Set it
to `true` only for an image produced by `gc build-image` with that city baked
in.

Or select an SSH host:

```toml
[session]
provider = "ssh:agent@worker.example"
```

`hybrid` may route selected session names to Kubernetes while keeping other sessions local. A remote Omnigent capsule is materialized only after the hybrid runtime reports a remote route; local routes continue to use the city-local Omnigent service.

## Start and verify

Start the city and check the selected profile before assigning work:

```bash
gc start
gc omnigent doctor --profile claude-primary
gc session list
gc omnigent state inspect
```

A healthy remote session has one Gas City session bead, one provider-owned durable allocation, and at most one live place. Kubernetes mounts the durable state independently from the pod. SSH keeps it beneath the configured remote state root. Pod deletion, SSH disconnect, city stop, sleep, suspend, and runtime replacement retain the allocation.

Use Herdr for a fully interactive view without changing the worker lifecycle:

```bash
gc session view list
gc session view open <city>/<session-id>
gc session view attach <city>/<session-id>
```

Closing the viewer does not stop the worker:

```bash
gc session view close <city>/<session-id>
```

The viewer uses Gas City's typed terminal channel. It never exposes the capsule's private Omnigent socket, credential paths, provider connection details, or conversation database.

## Read the state report

`gc omnigent state inspect` performs fresh provider reads and does not mutate state. Its actions have these meanings:

| Action | Meaning | Operator response |
| --- | --- | --- |
| `retained` | A known session owns the allocation. | No action; retention is expected. |
| `retained_orphan` | Provider state has no matching durable session fact. | Investigate interrupted provisioning; Gas City does not delete it automatically. |
| `missing` | A tracked session has no matching provider allocation. | Restore the expected state or explicitly abandon continuity. |
| `conflict` | Ownership is ambiguous, changed, or state reappeared after purge. | Stop and repair provider metadata; never force a broad deletion. |
| `would_purge` | A dry-run found one closed, non-live, explicitly targeted allocation. | Review, then run the same command without `--dry-run`. |
| `purged` | The exact allocation was deleted and completion recorded. | No further action. |
| `purge_recorded` | A prior purge already completed or absence was confirmed. | No further action unless state reappears. |

Inspect a remotely supervised city through its normal Gas City context:

```bash
gc --context production omnigent state inspect
```

Remote reads never fall back to local city files. Purge uses the same write grant and authorization boundary as other remote city mutations.

## Recover continuity failures

When a session has an existing conversation binding, Gas City never guesses a replacement conversation. Missing state, duplicate allocations, pin mismatch, unavailable credentials, invalid ownership, or an unresolved conversation fail closed.

Use this recovery order:

1. Run `gc omnigent state inspect` and `gc session logs <session-id>`.
2. Restore the expected PVC, SSH directory, credential reference, executable, or catalog pin.
3. Wake the same session and verify that the original conversation resumes.
4. If continuity is intentionally abandoned, run `gc session reset <session-id>` to start a fresh conversation under the same Gas City session identity.
5. Close and purge only when the retained conversation is no longer needed.

An unavailable provider or transport is not evidence that state is gone. Do not reset while Kubernetes, SSH, or the secret store is merely unreachable.

Common refusal errors preserve the same safety boundary:

| Error | Recovery |
| --- | --- |
| `capsule state control unsupported` | Select the SSH or Kubernetes runtime that owns the session, then retry through that city context. |
| `capsule state not tracked` | Resolve the durable session ID with `gc session list`; never substitute a provider path or PVC name. |
| `capsule state purge requires closed session` | Close the session only after its conversation is no longer needed. |
| `capsule state purge is blocked by live runtime` | Stop the matching worker and confirm it is no longer live before retrying the dry-run. |
| `capsule state conflict` | Compare the session fact with provider ownership metadata and repair the mismatch before any deletion. |
| `runtime unavailable` | Restore Kubernetes or SSH reachability. Do not treat an unavailable inventory as an empty inventory. |

## Rotate credentials

Keep the logical secret-reference `id` stable while rotating its provider
source. Suspend the affected session, create a new Kubernetes Secret or
owner-only SSH path, update the matching `[agent.secret]` source, and run
`gc omnigent doctor --profile <profile>`. Wake the same session and verify a
two-turn exchange before revoking the old credential. Restarting is required
because a harness may cache credentials even when Kubernetes updates a mounted
Secret.

Gas City records the reference ID and mount destination, never the credential
value. Rotate each Claude profile independently so a failed rotation cannot
silently fall back to another profile's authentication.

## Back up conversation state

Suspend the affected sessions before taking a provider-native snapshot. On
Kubernetes, snapshot the Gas City-owned PVC selected by
`gc-capsule-state=true` and verify its city/session annotations. On SSH, back
up the matching allocation beneath `/var/lib/gascity/omnigent` while
preserving ownership and modes. Store backups with the same controls as source
code and conversation transcripts; credential projections live outside this
state and require a separate secret-store backup policy.

Restore the allocation to its original provider identity before waking the
session. Run `gc omnigent state inspect` first: a restored allocation that no
longer matches its durable session fact is a `conflict`, not a candidate for
automatic adoption.

## Close and purge

Closing a session stops its place but deliberately retains Omnigent state:

```bash
gc session close <session-id>
gc omnigent state purge <session-id> --dry-run
```

The dry-run repeats every safety read but records no authorization and changes no provider resource. If the report identifies exactly the intended closed, non-live allocation, purge it:

```bash
gc omnigent state purge <session-id>
```

Purge refuses open or live sessions, changed ownership, ambiguous inventory, foreign-city state, and arbitrary provider paths. Authorization is recorded before deletion, so an interrupted request can be retried safely. Gas City never runs bulk or age-based deletion of retained Omnigent conversations.

## Upgrade or roll back

Treat Gas City, the Omnigent executable, the catalog, and the Kubernetes image as one reviewed release set.

1. Stop affected sessions or the city cleanly.
2. Install the new pinned executable on every SSH target or publish the new Kubernetes image.
3. Update the catalog commit, package version, executable path, and SHA-256 together.
4. Run `gc omnigent doctor` and start one canary profile.
5. Verify two-turn conversation continuity, Herdr attachment, state inspection, and process/resource cleanup before expanding rollout.

To roll back, restore the previous Gas City binary, image or SSH executable, and matching catalog as one set. Keep durable state untouched. If the older Omnigent version rejects the existing database, stop and restore a compatible state backup rather than creating a new conversation over it.

## Current boundaries

- Kubernetes and SSH are the built-in remote capsule runtimes. Daytona requires a separate runtime provider.
- Omnigent remote configuration and managed-host placement are not used. Gas City owns remote placement and passes a local catalog into each capsule.
- Remote-city clients can inspect Gas City status and use the typed terminal surface; they cannot proxy the private Omnigent API or download its database implicitly.
- Automatic reconciliation may stop a proven orphan process, but it retains durable allocations until an explicit, session-scoped purge.
- Policy mail remains opt-in. Profiles without a policy recipient run autonomously and create no policy requests.
