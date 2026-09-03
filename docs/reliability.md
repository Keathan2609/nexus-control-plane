# Reliability

What NEXUS guarantees, how, and what it does not.

## Guarantees

| Property | Mechanism |
|---|---|
| A queued job is executed exactly once, or retried | Lease + `FOR UPDATE SKIP LOCKED` claim |
| A crashed worker loses no work | Lease expiry and reaping |
| A committed state change has its event | Transactional outbox |
| A duplicate webhook does not double-build | Unique index on `(provider, delivery_id)` |
| A duplicate event does not double-handle | `processed_events` guard |
| Two promotions cannot both own an environment | Partial unique index on live deployments |
| A run never reports success without running | Absent transition in the state machine |
| A deployment never reports healthy unverified | Absent transition in the state machine |
| An abandoned deployment is not left invisible | Reconciler stuck-deployment sweep |
| A preview environment does not outlive its PR | Reconciler expiry sweep |

## Failure modes

| Failure | Detected by | Response |
|---|---|---|
| Worker crashes mid-job | Lease expiry | Requeued, attempt preserved, backoff applied |
| Worker hangs | Lease expiry | Same path — a hung worker is a worker that stopped heartbeating |
| Worker network partition | Heartbeat returns `ErrNotOwner` | Worker abandons the job; another has it |
| Job exceeds its timeout | Worker context, and reconciler sweep | Attempt failed, retried or terminal |
| Step is genuinely broken | Attempt budget exhausted | `FAILED`, dependents `SKIPPED`, run `FAILED` |
| Database unavailable | Connection error | API fails readiness; workers stop claiming; nothing is lost |
| NATS unavailable | Publish error | Outbox retains rows and retries; execution unaffected |
| Control plane killed mid-deployment | Reconciler stuck sweep | Deployment `FAILED`, rollback available |
| Deployment applies but never serves | Health verification | `FAILED`, rollback offered |
| Someone edits a live workload | Reconciler `Observe` | Drift recorded and surfaced |
| Duplicate webhook delivery | Unique delivery id | Second insert is a no-op |
| Two API replicas relaying | `SKIP LOCKED` on the outbox | Each row published once |

## The reconciler is the backstop

Everything else is event-driven and therefore only as reliable as events reaching their
consumer. The reconciler assumes nothing arrives. Each loop reads the database, compares
what is true with what should be true, and corrects the difference.

This is a Kubernetes controller shape: **level-triggered, not edge-triggered.** It does
not care *why* the world diverged, only that it has. That is what turns a lost event, a
killed worker or a partial deployment from a permanent inconsistency into a delay of one
interval.

Loops run every ten seconds by default:

1. **Reap leases** — return jobs from workers that stopped heartbeating
2. **Enforce timeouts** — fail jobs past their declared deadline
3. **Advance runs** — release dependents; the safety net behind the worker fast path
4. **Recover stuck deployments** — fail deployments abandoned mid-flight
5. **Detect drift** — compare desired with observed
6. **Expire previews** — destroy pull-request environments past their expiry
7. **Mark lost workers** — cosmetic; their jobs are already recovered by loop 1
8. **Refresh gauges** — queue depth, live workers, outbox lag
9. **Prune** — bound the outbox and the idempotency guard

Each loop is called independently and its error logged rather than returned. A reconciler
that stopped because drift detection hit a transient error would also stop reaping
leases, which is the loop that recovers crashed workers.

Every loop is an idempotent conditional statement rather than a read-modify-write, so
running several reconcilers concurrently is safe — though one is enough.

## Patterns used, and where

**Context cancellation.** Every operation takes a context. A job's context carries its
timeout; cancelling it tears down the subprocess.

**Timeouts.** Per step, on HTTP reads, on health probes, on database pings, on shutdown
grace. `ReadHeaderTimeout` rather than `ReadTimeout` on the API, and no `WriteTimeout`,
because either would sever the SSE stream on a fixed schedule — that is the one
connection meant to stay open.

**Exponential backoff with jitter.** Job retries and outbox publishing. Jitter on job
retries specifically, because N jobs failing together on a shared dependency would
otherwise all retry at the same instant.

**Bounded retries.** `max_attempts` per job, enforced in SQL. Nothing retries forever.

**Graceful shutdown.** Two-phase in the worker: the claim loop stops immediately so no
new work is taken, while in-flight jobs get a grace window. That turns a rolling restart
from a burst of duplicated builds into a non-event.

**Connection lifecycle.** Pool bounded at sixteen connections with a thirty-minute
lifetime, so N replicas cannot exhaust `max_connections`.

**Health endpoints.** `/healthz` deliberately touches nothing — a liveness probe that
checks the database restarts the API during a database blip, which is precisely when
restarting it helps least. `/readyz` does check the database, because that is what
readiness means.

**Dead-letter handling.** Outbox rows that keep failing accumulate `attempts` and
`last_error` and back off toward five minutes. They are never dropped.

## Patterns deliberately not used

**Circuit breakers.** There is no call path where one would help. The database is the
system of record — breaking the circuit to it means refusing to work, which is what
connection failure already does. The applier is called once per deployment, not in a hot
loop.

**Distributed locks.** `SKIP LOCKED` and partial unique indexes cover every case that
would otherwise need one, without adding a second system that can fail.

**Leader election.** The reconciler is safe at n>1 because its loops are idempotent
conditional statements. Election would add a failure mode to avoid a problem that does
not exist.

**A saga coordinator.** Deployment is a linear state machine with a compensating action
(rollback) that is itself an ordinary deployment. A coordinator would be ceremony.

## Defects found by running it

Every one of these was found by executing the system against real infrastructure, not by
reading the code. They are recorded because the failure modes are more instructive than
the fixes.

**Non-monotonic ULIDs broke `ORDER BY id`.** Identifiers used `crypto/rand` entropy
directly, so IDs minted inside the same millisecond ordered arbitrarily. Every list in
the console sorts by id, so "most recent first" silently stopped being true precisely
when the system was busiest. A monotonic entropy source fixed it; a test asserts the
property.

**Run numbers collided.** The counter row meant "next number to hand out", but the
seeder consumed number 1 and left the counter at 1, so the next allocation duplicated it
and hit the unique index. The schema caught it, which is the constraint doing its job.

**A leaked transaction hung the whole test package.** A test that failed an assertion
left a transaction open, its pooled connection checked out, and `pgxpool.Close` then
blocked forever — one leaked transaction hanging every other test rather than failing
one. Transactions in tests now register their rollback with `t.Cleanup`.

**The scheduler depended on the reconciler tick.** Nothing advanced a run when a job
finished, so each dependency wave waited for the next 10-second cycle: a nine-step
pipeline of one-second steps took roughly 60 seconds for about 10 seconds of work.
Workers now call `Advance` immediately after recording a result, which brought the same
pipeline to about 9 seconds. The reconciler remains the safety net, not the scheduler.

**Compose ordering was implicit.** The stack started the API before migrations had run.
Expressing it as `service_completed_successfully` on a one-shot seed job made the
ordering a constraint rather than a race.

**Distroless images have no shell for a healthcheck.** A container healthcheck cannot be
a command line when the image contains no shell, no `wget` and no `curl`. The binary
probes itself through a `-healthcheck` flag instead, which adds nothing to the image and
exercises the address the server is actually bound to.

**A relative `--file` resolved against the wrong directory.** Docker resolves it against
the process working directory, not the build context, so a worker running from the
repository root looked for a Dockerfile at the root and failed. It is now joined to the
context.

**Every manual build produced the same image tag.** A synthesised commit such as
`manual0903072400` truncates to `manual0` for every manual run ever made, so each build
overwrote the previous image under the same tag and rollback had nothing distinct to
return to. Only a plausible hex hash is truncated now.

**Kubernetes rejected the revision as a label.** A revision reads as `r21@a84fc12`, and
`@` is exactly the character a label value forbids — every apply was rejected. The label
is sanitised and the exact revision travels in an annotation, which `Observe` reads, so
drift comparison stays byte-exact.

**`runAsNonRoot` could not verify a named user.** The kubelet must confirm the user is
not root and cannot do that from the string `nonroot` that a distroless image declares;
it refuses the container with `CreateContainerConfigError`. The UID is now numeric, which
makes the guarantee checkable rather than nominal.

## Known weaknesses

- **The rate limiter is per-process.** N replicas allow N times the rate. It exists to
  stop one client saturating one replica, not to enforce a global quota.
- **`Execute` runs in a goroutine tied to one replica.** If that replica dies, the
  deployment is stuck until the reconciler's stuck-deployment sweep fails it. The window
  is bounded but real; a durable deployment-execution queue would close it.
- **Log lines are written one row per line.** Fine at demonstration volume; a build
  producing 100k lines would want batching or object storage.
- **No backpressure on the queue.** A burst of triggers is accepted and queued. Depth is
  observable, but nothing rejects or sheds.
