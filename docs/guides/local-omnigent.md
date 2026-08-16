---
title: Run Agents Through Local Omnigent
description: Pin a local Omnigent service, select Codex or Claude-compatible profiles, and use interactive herdr or tmux sessions without giving Omnigent control of Gas City placement.
---

Omnigent lets one Gas City workflow use different coding harnesses and
authentication profiles without changing its formula or beads. The integration
is opt-in and local: Gas City supervises one pinned Omnigent service for the
City, while external model traffic follows the profile you select. That service
runs Omnigent's loopback API and foreground local host together so sessions can
launch without giving Omnigent remote placement authority.

This guide assumes you know the [six Gas City primitives](/getting-started/how-gas-city-works).
The City is the local (root) pack; importing the Omnigent pack adds an Agent
provider and a private supervised service.

## Keep ownership clear

| Gas City owns | Omnigent owns |
|---|---|
| Formula execution, beads, convoys, and routing | Harness and model selection within the chosen profile |
| Rig and workspace placement | Harness authentication |
| Service supervision and restart | Tool and local sandbox policy |
| herdr or tmux pane lifecycle | Conversation transcript and harness lifecycle |
| Remote runtime configuration | Ordered auth/backend fallback inside one conversation |

Omnigent runs in local-config mode. It cannot create a remote host, Kubernetes
or Daytona sandbox, tunnel, repository clone, or worktree. Gas City rejects
those settings before work starts. A profile may still call its named external
model endpoint.

## Install and pin Omnigent

Install Omnigent separately from Gas City. Gas City does not download or update
it. The operator-owned catalog under
`<city>/.gc/services/omnigent/config/profiles.yaml` records all three identities
Gas City verifies on every start:

- the full reviewed Git commit;
- the package version reported by `omnigent --version`;
- the SHA-256 of the exact executable.

Start from `examples/omnigent/catalog.example.yaml` in the Gas City repository.
Copy its `agents/` directory with it. Each catalog `agent` points to a regular
Omnigent single-file YAML definition. Keep `name` plus `prompt` or
`instructions`, and omit `spec_version`; that key identifies the different
directory-bundle format and is rejected before startup. Keep the files
owner-readable only. Replace a digest only after reviewing the build for that
platform. A mismatch fails closed with an installation diagnostic.

The catalog contains profile IDs and display metadata, never credentials. An
environment list names only the variables Omnigent needs for that profile; Gas
City forwards those names explicitly and does not persist or display their
values. Provider secrets remain in the operator-owned Omnigent configuration.

## Import the pack and choose a profile

Place the example pack at `packs/omnigent` under the City, then add this to
`city.toml`:

```toml
[imports.omnigent]
source = "packs/omnigent"

[providers.omnigent-city]
base = "provider:omnigent"

[providers.omnigent-city.option_defaults]
profile = "offline-mock"

[[agent]]
name = "worker"
provider = "omnigent-city"
```

`offline-mock` is the credential-free default for deterministic local tests.
Change only `profile` to select `codex`, `claude-primary`, or
`claude-secondary`. The Agent topology, formula, beads, and routing stay the
same. Codex and Claude-compatible model outputs may differ; portability here
means the Gas City control contract stays the same.

The example's two Claude profiles use different Omnigent providers and auth
references. `claude-primary` declares `claude-secondary` as its ordered
fallback. Omnigent advances that chain only after a typed terminal
authentication, rate-limit, or backend-unavailable result. The same opaque
conversation ID and pane remain authoritative.

## Start, inspect, and attach

Start the City normally, then inspect the local boundary:

```bash
gc start
gc omnigent doctor --profile claude-primary
gc omnigent status --session "$GC_SESSION_ID"
```

`doctor` verifies the exact binary pin, local-only mode, private service,
profile availability, and named missing environment references. `status`
correlates the Gas City session and runtime view with its opaque Omnigent
conversation, active profile, fallback category, and pending policy request.
Neither command prints credential values or transcript content.

Agents are fully interactive in their existing visible runtime. herdr is the
preferred runtime; an isolated Gas City tmux socket is the fallback. Both send
keystrokes, multiline paste, Unicode, and interrupts to the same attachment—no
nested or default tmux server is used.

To attach directly to a known conversation:

```bash
gc omnigent attach --profile claude-primary --conversation conv_example
```

Detaching closes the pane connection, not the conversation. Reattaching uses
the persisted opaque ID. If that ID is missing, malformed, or belongs to a
different workspace, profile, or Gas City session, the attach fails visibly
instead of creating a replacement.

## Opt in to policy mail

Autonomous profiles create no policy mail. To allow a rare Omnigent tool or
auth policy question, add a Gas City mail identity to that profile's private
catalog entry:

```yaml
policy_mail_recipient: reviewer
```

The request mail contains a sanitized question, allowed actions, and stable
conversation/request IDs. Reply in the bound thread with strict JSON:

```json
{"request_id":"policy_example","action":"approve","text":"Reviewed explicitly"}
```

Gas City transports the explicit answer back to the same conversation exactly
once. It does not approve, deny, infer, rank, expire, or escalate a request.
Without a response, the request remains durably pending. Put any escalation
method in your pack's formula or order.

## Stop, disable, or roll back

Use `gc stop` for a clean stop. Gas City stops its panes, the supervised API,
the local host, and their exact child process groups, but preserves the
City-scoped Omnigent database and opaque conversation IDs.

To disable the integration, remove the import/provider selection and reload or
restart the City. To uninstall, stop first, remove the separately installed
binary, and remove the private service directory only if you intentionally want
to discard conversations.

An upgrade is an explicit stop, catalog/pin replacement, verification, and
start. Gas City never migrates credentials or conversations. If verification
fails, restore the previous catalog and exact binary pin; rollback reuses the
preserved database only when that version's compatibility checks pass.
