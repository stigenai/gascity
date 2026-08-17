# Omnigent Remote Capsule Release Gates

This record defines the release evidence for remote Omnigent capsules. It
separates hermetic gates, local real-process gates, and operator-provisioned
infrastructure gates so a skipped external boundary is visible rather than
mistaken for a pass.

## Required automated gates

Run from the Gas City repository with CGO disabled on macOS:

```bash
CGO_ENABLED=0 go test -race -p 1 \
  ./internal/testutil/omnigentcapsule \
  ./internal/runtime/ssh ./internal/runtime/k8s ./internal/runtime/herdr

CGO_ENABLED=0 go test ./internal/testutil/omnigentcapsule -count=50

CGO_ENABLED=0 go test -p 1 ./internal/runtime/ssh ./internal/runtime/k8s \
  -run 'Capsule.*(Secret|Credential|Stage|Fault|State|Attach|Cleanup|Preflight)|Rejects.*(Literal|Unsafe|Tamper|Forged)' \
  -count=10

CGO_ENABLED=0 go test -tags=integration -p 1 \
  ./internal/runtime/ssh ./internal/runtime/k8s ./internal/runtime/herdr

GC_OMNIGENT_PERF_TEST=1 CGO_ENABLED=0 go test \
  ./internal/testutil/omnigentcapsule \
  -run '^TestReleaseGateStartupAndReconnectBudgets$' -count=5 -v
```

The fast repository gate and pre-push hook remain required. On a process-heavy
host, serialize them with `LOCAL_TEST_JOBS=1` rather than weakening or bypassing
the gate.

## Running shards on a GTR host

Use the SSH runner when a configured GTR host has more available CPU and
memory than the local workstation:

```bash
scripts/test-remote-shards gtr integration 12
scripts/test-remote-shards gtr full 12
```

The runner uses non-interactive SSH, synchronizes only tracked and non-ignored
workspace files, creates an isolated `/var/tmp/gascity-remote-tests.*`
workspace, invokes the canonical `scripts/test-local-parallel` entrypoint, and
retrieves every shard log before deleting the remote workspace. The final
`artifacts:` line names the retained local evidence directory. Omit the job
count to let the remote host select its machine-aware limit. Set
`GC_REMOTE_TEST_CGO_ENABLED=1` only when the target gate needs CGO.

The SSH alias must already resolve and authenticate with `BatchMode=yes`; the
runner never prompts for a password and never copies ignored credential files.
Validate its transport and cleanup behavior without a remote host by running
`scripts/test-remote-shards-test`.

## Performance budgets

The hermetic latency gate measures local persistence and lifecycle overhead,
not model inference or network latency:

| Operation | Samples | Budget |
| --- | ---: | ---: |
| New capsule state plus first start and terminal cleanup | 25 | 2 seconds total |
| Transport loss, reconnect, same-conversation completion | 100 | 2 seconds total |

The test records measured durations in verbose output, preserves the exact
conversation across every reconnect, and requires `AssertClean` after both
workloads. Run it only with `GC_OMNIGENT_PERF_TEST=1`; ordinary unit runs skip
the timing assertion to avoid treating an overloaded shared host as a product
regression.

Baseline recorded on 2026-08-17 (five consecutive local runs): capsule start
and cleanup took 5.7–7.3 ms total; disconnect/reconnect took 55.7–65.9 ms
total. These measurements are diagnostic context, not a reason to narrow the
budgets without evidence across supported hosts.

## Security and resource evidence

The SSH and Kubernetes suites must prove:

- credential sentinels never cross staging, launch commands, errors, or
  unselected profile projections;
- literal credentials, unsafe source paths, path traversal, symlink escape,
  tampered state, forged ownership, and foreign replacements fail closed;
- transport loss, response loss, server/host/harness/model faults, and volume
  loss preserve or fail the exact durable conversation as specified;
- teardown returns pods, network policies, process groups, tmux monitors,
  sockets, SSH/kubectl clients, staged catalogs, and durable state to their
  declared baseline;
- race and repeated lifecycle runs show no unbounded goroutine, listener,
  file-descriptor, process, or state-resource growth.

## Provisioned infrastructure gates

These tests intentionally skip unless an operator supplies an isolated target:

| Boundary | Opt-in and safety condition |
| --- | --- |
| Disposable Kind replacement/fault cleanup | `GC_K8S_FAULT_TEST=1` and current context name beginning with `kind-` |
| Live Kubernetes terminal attach | `GC_K8S_ATTACH_TEST_SESSION`, `GC_K8S_NAMESPACE=gc-attach-test-*`, and `GC_K8S_ATTACH_TEST_SHELL=1` |
| Live SSH terminal attach | `GC_SSH_ATTACH_TEST_ENDPOINT`, `GC_SSH_ATTACH_TEST_SESSION`, and `GC_SSH_ATTACH_TEST_SHELL=1` |
| Real Herdr plus Claude kind path | `GC_LIVE_ANTHROPIC_TEST=1` |

Release evidence must name each skipped boundary and its missing prerequisite.
Real Codex and multiple Claude-profile proofs are tracked separately by
`ga-xgv.7.4`.
