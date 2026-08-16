# Local Omnigent example

This opt-in pack keeps Gas City in charge of orchestration, workspaces,
services, and visible herdr/tmux panes while a pinned local Omnigent process
owns the selected harness, model, authentication, tool/sandbox policy, and
conversation lifecycle.

The default `offline-mock` profile is intended for a credential-free loopback
provider named `gascity-offline-mock`. The other profiles may make external
model API calls: `codex` uses the operator's local Codex configuration, while
`claude-primary` and `claude-secondary` refer to distinct Omnigent providers
and authentication environments. No credential belongs in this directory.
Configure provider secret references only in the operator-owned Omnigent
configuration.

1. Copy `catalog.example.yaml` and `agents/` into
   `<city>/.gc/services/omnigent/config/`, preserving owner-only permissions.
   Each catalog `agent` is a regular Omnigent single-file YAML definition: keep
   `name` plus `prompt` or `instructions`, and do not add `spec_version`.
2. Verify that `executable`, commit, package version, and SHA-256 identify the
   exact installed binary. A different platform build needs its own reviewed
   digest; do not weaken the pin.
3. Import `pack.toml`, keep the default profile for offline testing, or set the
   standard provider option `profile` to `codex`, `claude-primary`, or
   `claude-secondary`. The Gas City agent/formula graph does not change.
4. Start with `gc start`, inspect with `gc omnigent doctor`, and attach to the
   same durable conversation with `gc omnigent attach --conversation <id>`.
   Herdr is the preferred visible runtime; configured tmux is the fallback.

`claude-primary` has the ordered sticky fallback `claude-secondary`. Omnigent
may advance that chain only for its typed auth/rate-limit/backend-unavailable
signals; it does not reroute Gas City work.

Use `gc stop` to stop city sessions and supervised services. To disable the
integration, remove the pack import/provider selection and run `gc reload` (or
restart the city). Conversation data remains under the city-scoped Omnigent
service directory so re-enabling resumes exact opaque IDs. Uninstall only
after stopping the city; remove the separately installed binary and private
city service directory explicitly if conversation retention is not wanted.
