# 5. A deployment is healthy only when observed to be

**Status:** Accepted

## Context

Most deployment tooling reports success when the apply returns zero. A container can be
`Running` while the process inside it is deadlocked, out of file descriptors, or unable
to reach its database — all states in which "the deployment succeeded" is false.

## Decision

There is no `DEPLOYING -> HEALTHY` transition. The only route to `HEALTHY` runs through
`VERIFYING`, which requires consecutive successful HTTP probes inside a deadline. Every
probe is persisted.

## Consequences

The state machine enforces this rather than a convention: an attempt to move a deployment
straight from `DEPLOYING` to `HEALTHY` returns an error, and a test asserts it.

Verification requires *consecutive* passes, not a cumulative count. A single failure
resets the streak, so a target alternating between serving and refusing connections is
never accepted — which is exactly the state a half-broken deployment is in.

Probes are stored rather than collapsed into a boolean, so the console can show why a
deployment was considered healthy. "It is healthy" is a claim; the rows are the evidence
for it.

This also makes rollback meaningful. A rollback runs through the same verification, so a
rollback that does not come up healthy is reported as a failed rollback rather than
assumed to have worked because the release used to.

The cost is latency — a deployment takes at least `RequiredPasses × Interval` longer to
report success — and the possibility of a false negative on a service that is genuinely
slow to warm up. `MaxAttempts` and `Timeout` bound that.

The HTTP prober disables connection reuse, because a pooled connection can succeed
against a process that has already stopped accepting new ones, which would let a dying
service keep passing its health check.

## Alternatives considered

**Trust the orchestrator's readiness probe.** Reasonable, and complementary, but it makes
NEXUS unable to report *why* something is unhealthy, and ties verification to Kubernetes
specifically rather than to the applier interface.

**Verify asynchronously and report optimistically.** Would let a deployment be reported
successful and then quietly marked failed — the behaviour this decision exists to avoid.
