---
title: Omnigent Capsule Test Harness
description: Deterministic fixtures and test-tier boundaries for remote Omnigent capsule work.
---

# Omnigent Capsule Test Harness

Remote Omnigent behavior crosses several expensive boundaries: profile and
conversation state, Kubernetes or SSH placement, tmux, a private Unix socket,
Herdr, model traffic, policy mail, and cleanup. Most regressions at those
boundaries are state-transition bugs, not failures of the real binaries. The
shared capsule fixture owns those transitions so tests can trigger them
synchronously without credentials, cloud access, subprocesses, listeners,
polling, or sleeps.

The fixture lives in `internal/testutil/omnigentcapsule`. It models observable
contracts only. It does not emulate Kubernetes, SSH, tmux, Herdr, or Omnigent
implementation details.

## What it provides

- Codex-shaped, primary Claude Code-shaped, and secondary Claude Code-shaped
  profiles. The two Claude profiles have distinct backend metadata so tests do
  not assume every Claude-compatible profile uses Anthropic, plus distinct
  non-secret auth-profile identities so fallback cannot accidentally reuse the
  first profile's authentication.
- Exact durable conversation identity across fixture reconstruction.
- Kubernetes and SSH resource census projections, including pods, policies,
  retained state, tmux sessions, process groups, private sockets, staged
  catalogs, and lifecycle-neutral Herdr viewers.
- Synchronous fault triggers for primary-profile rate limiting, policy mail,
  transport loss, durable-volume loss, capsule server crash, host crash,
  harness crash, and model endpoint loss.
- Ordered typed events and deterministic model-request records.
- Exact cleanup assertions. Closing a viewer never changes worker state;
  transient cleanup can retain durable state; terminal purge must return the
  complete census to zero.

The durable fixture JSON contains only test-owned prompts, opaque IDs, and
public profile metadata. It has no credential fields or non-loopback endpoint
configuration.

## Typical test

```go
fixture, err := omnigentcapsule.New(t.TempDir(), omnigentcapsule.Config{
    CapsuleID: "ga-example",
    Transport: omnigentcapsule.TransportKubernetes,
    ProfileID: omnigentcapsule.ProfileClaudePrimary,
})
if err != nil {
    t.Fatal(err)
}
started, err := fixture.Start()
if err != nil {
    t.Fatal(err)
}

fixture.Inject(omnigentcapsule.FaultPrimaryRateLimit)
result, err := fixture.Run("complete the assigned bead")
if err != nil {
    t.Fatal(err)
}
if result.ConversationID != started.ConversationID {
    t.Fatal("failover replaced the conversation")
}
```

Use `Events()` to assert transition order and `Census()` or `AssertClean()` to
assert resource ownership. Fault methods complete before returning, so tests
assert immediately; do not add sleeps or polling around them.

## Test-tier ownership

| Risk | Smallest owner |
|---|---|
| Conversation restart, profile failover, policy mail, fault ordering, fake resource census | Fast tests in `internal/testutil/omnigentcapsule` and the consuming domain package |
| Kubernetes object planning, fencing, retained PVC behavior | Deterministic tests in the Kubernetes runtime package |
| SSH argv, staging, secret projection, fenced cleanup | Deterministic tests in the SSH runtime package |
| Real isolated tmux process and socket cleanup | Integration-tagged SSH/tmux tests using the shared tmux guard |
| Real disposable Kubernetes replacement | Explicit integration profile with a pre-provisioned namespace |
| Real Codex or Claude inference | Explicit credentialed release profile only |

Do not multiply the full profile matrix across every real provider. The shared
fixture owns profile semantics; each real boundary keeps one composition proof
for the behavior unique to that boundary. Follow the repository testing policy
for deadlines, process cleanup, resource-ledger changes, and release evidence.

## Volume and transport loss

`FaultTransportLoss` makes terminal/model operations fail while preserving the
worker and conversation. `RestoreTransport` reconnects the same state.

`FaultVolumeLoss` removes the exact durable fixture record. `Restart` then
returns `ErrDurableStateLost`; it never creates a replacement conversation.
This is intentionally different from a normal runtime replacement, which
reopens the same retained state.

## Cleanup contract

`Cleanup(true)` removes transient runtime resources and retains one durable
state resource. `Cleanup(false)` is terminal purge and must make `AssertClean`
pass. A Herdr viewer is counted separately and never appears in the durable
worker snapshot, so viewer state cannot influence orchestrator reconciliation.
