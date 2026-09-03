# NEXUS — portfolio material

Copy for a portfolio site. Written to be lifted directly.

---

## Project title

**NEXUS**

## Project category

Senior Software Engineering · Platform Engineering · Distributed Systems · DevOps
Automation

## One-line description

> A software delivery control plane that takes a commit through test, build, a real
> container image, a real Kubernetes deployment and observed health verification — and
> rolls it back when verification fails.

Alternatives, if a different emphasis suits the page:

> An internal developer platform that owns the whole delivery lifecycle: distributed job
> execution, real container builds, Kubernetes deployment, observed health, and automatic
> rollback.

> A control plane for software delivery — desired state, observed state, and the control
> loops that close the gap.

## Short portfolio description (~90 words)

NEXUS is an internal developer platform that owns software delivery end to end. It
receives repository events, plans pipelines as a DAG, and hands work to distributed
workers that claim jobs under a lease rather than being assigned them. Pipelines run real
container builds and record each image's digest, then deploy that exact image to
Kubernetes and verify health by probing the running application — never by trusting that
the apply succeeded. When a release fails verification, NEXUS rolls back through the same
path. PostgreSQL is the system of record; a transactional outbox keeps events and state
from diverging.

## Engineering challenge

The hard problems were the ones that only appear when things go wrong.

**Distributing work without a coordinator.** Assignment needs something that decides who
gets what, plus health checks on every worker and a rebalancing story for when one dies
mid-queue — a single point of failure and the hardest part of the system to reason about.
Workers instead claim jobs with a single `UPDATE ... FOR UPDATE SKIP LOCKED` statement.
There is no leader to elect and no partition map. Verified by 16 concurrent claimers
racing for 40 jobs, asserting nothing is ever claimed twice.

**Surviving worker death without a supervisor.** A claimed job carries a lease. A worker
that crashes, is killed, is partitioned, or is merely slow stops renewing it, and the
reconciler returns the job to the queue with its attempt count intact. Worker death is not
a special case — it is the absence of a heartbeat.

**Keeping state and events from diverging.** Publishing inside a handler has an
unavoidable window: commit then crash loses the event, publish then roll back invents
one, and neither is detectable afterwards. The event is written into an outbox table in
the same transaction as the state change, and relayed to NATS after commit.

**Refusing to assume a deployment worked.** There is no `DEPLOYING → HEALTHY` transition.
The only route to healthy runs through verification, which requires consecutive
successful probes against the running application inside a deadline. Every probe is
persisted, so the record shows *why* a deployment was considered healthy.

**A scheduling defect that cost 50 seconds a run.** Jobs only became claimable on the
next reconciler tick, so every dependency edge in the DAG cost a full interval. Runs took
about 60 seconds; the work in them took about nine. Workers now release dependents
directly on completion, and level-triggered reconciliation stays as the backstop rather
than doing the scheduling.

**Tenant isolation that a handler cannot forget.** Four endpoints accepted a well-formed
identifier without asking who owned it — a run id from another organisation returned its
jobs, a job id returned its build logs. Organisation scope now lives in the store, so the
omission is a compile error rather than a disclosure.

## My role

I designed and built NEXUS end to end — the architecture, the Go control plane, the
PostgreSQL schema, the Next.js console, the container and Kubernetes delivery path, the
observability, the security model, and the CI.

The decisions I care most about are the ones where I chose the less obvious option and
can say why. Using PostgreSQL as the queue rather than Redis, so enqueue is transactional
with the state change it belongs to. Letting workers claim rather than be assigned, so
there is no coordinator to fail. Modelling every lifecycle as a closed state machine with
an explicit transition table, so an illegal transition errors instead of quietly
corrupting history. Treating NATS as notifications rather than truth, so losing the broker
degrades the console and never the system.

I also found and fixed real defects by running the system rather than by reading it: a
relative Dockerfile path that docker resolves against the working directory rather than
the build context; a synthesised commit value that truncated to the same image tag on
every manual run, so builds silently overwrote each other and rollback had nothing
distinct to return to; a Kubernetes label rejected for containing `@`; a distroless image
refused by the kubelet because `runAsNonRoot` cannot verify a non-numeric user. Each of
those is written down in the documentation with the reasoning, because the reasoning is
the part worth keeping.

## Core engineering

- **Distributed workers** — claim-based dispatch, bounded slots, leases and heartbeats,
  graceful drain
- **Scheduler** — DAG planning by Kahn's algorithm with deterministic wave ordering, and
  a fast path that releases dependents on completion
- **State machines** — closed sets and explicit transition tables for runs, jobs and
  deployments
- **Transactional outbox** — events written with the state change, relayed with backoff
- **Idempotency** — unique delivery ids and a `processed_events` guard, so at-least-once
  delivery is safe
- **Deployment lifecycle** — desired state, apply, rollout, observed verification, drift
  detection, rollback
- **Kubernetes** — a real `kind` cluster, images loaded rather than pulled, node-port
  health probes, hardened pod security
- **Observability** — Prometheus metrics, OpenTelemetry traces correlated from webhook to
  probe, structured logs with redaction
- **Security** — server-side RBAC, organisation scoping in the store, secret references
  rather than secret values, Argon2id, signed webhooks
- **Rollback** — a deployment of an earlier release through the same apply and the same
  verification

## Key technical decisions

- PostgreSQL as the queue — transactional enqueue, and a queue that can be joined,
  indexed and inspected
- Claim, don't assign — no coordinator, no leader election, no rebalancing
- Transactional outbox — the only way to keep a state change and its event consistent
- Level-triggered reconciliation — read the world and correct it, regardless of why it
  diverged
- Health observed, never assumed — consecutive probes against the application, retained
  as evidence
- Immutable image tags derived from the commit — "roll back to the previous image" needs
  a well-defined answer
- Keyset pagination over monotonic ULIDs — offset paging over an actively written table
  skips and repeats rows
- An ImageBuilder boundary — domain code never shells out to Docker, so the container
  step is testable without a daemon
- Bounded everything — worker slots, connection pool, request bodies, page sizes,
  deployment admission, SSE subscribers
- Tenant scope in the store, not the handler — make the unsafe version impossible to write

## Reliability

A worker that dies stops renewing its lease. The reconciler reaps expired leases and
returns those jobs to the queue with their attempt counts intact, so another worker
continues where the first stopped. Nothing supervises workers, and nothing needs to.

The reconciler is level-triggered: each pass reads actual state and corrects it without
caring why it diverged. It reaps leases, enforces timeouts, fails deployments orphaned by
a dead API replica, detects drift between desired and observed revision, and expires
preview environments. It is a safety net, not the scheduler — the fast path releases work
directly, and the reconciler exists so that a missed event costs one interval rather than
a stuck run.

Duplicate events are absorbed by unique delivery ids and an idempotency guard. A NATS
outage leaves events in the outbox to be relayed later; PostgreSQL remains the source of
truth throughout.

## Scalability

Concurrency is bounded deliberately. A pipeline submission writes a run and its jobs and
returns; nothing executes inside the request. Workers expose a fixed number of slots and
claim work only when a slot is free, so a burst becomes queue depth rather than processes.

Measured: 300 concurrent submissions were all accepted while queue depth grew from 26 to
317, and with 664 jobs queued the number actually running stayed at exactly 4 — the
worker's slot count. Adding workers scales it: 4 slots completed 101 jobs in 30 seconds,
16 slots completed 341, roughly 3.4× for 4× the workers.

Under sustained overload, queue latency rises before execution capacity is exhausted. The
API sheds explicitly rather than accepting work it cannot do: 429 for rate limiting, 503
with `Retry-After` when deployment admission is at capacity.

## Security

NEXUS stores secret *references* — a backend and a key — rather than secret values, so a
database disclosure yields pointers and there is no application encryption key to manage
or leak. Passwords are Argon2id with per-password salts; API tokens are stored as SHA-256
and the plaintext is shown once.

Authorisation is enforced server-side on every route, and organisation scoping lives in
the read store so a handler cannot forget it — a valid identifier belonging to another
organisation returns 404, indistinguishable from one that does not exist. Requests bind to
explicit DTOs and the decoder rejects unknown fields, so server-managed columns cannot be
set from a request body. Webhooks are verified with constant-time HMAC comparison.

Containers are multi-stage and distroless where a shell is not required, running as
non-root numeric UIDs; pods drop all capabilities, disallow privilege escalation and mount
a read-only root filesystem. CI pins every action to a commit SHA, runs on least-privilege
permissions, and scans dependencies, containers and the full git history for secrets on
every push.

At the time of writing: gitleaks clean across all history, govulncheck reports no called
vulnerabilities, npm audit reports none, and Trivy reports no HIGH or CRITICAL findings in
either image.

## Tech stack

**Backend** Go 1.27, chi, pgx v5, goose, NATS JetStream, OpenTelemetry, Prometheus
**Data** PostgreSQL 16
**Frontend** Next.js 15, React 19, TypeScript, CSS Modules
**Delivery** Docker, BuildKit, Kubernetes, kind, Helm, Argo CD, OpenTofu, GitHub Actions
**Observability** Prometheus, Grafana, Jaeger, OpenTelemetry Collector

## GitHub

https://github.com/Keathan2609/nexus-control-plane-new

## Demo

The whole system runs locally on Docker Desktop with no cloud account:

```bash
docker compose -f deployments/docker/docker-compose.yml up -d --build
```

That starts PostgreSQL, NATS, Prometheus, Grafana and Jaeger, migrates and seeds a
demonstration estate, then runs the API, two workers, the reconciler and the console at
**http://localhost:3100**. Two workers rather than one, so job claiming is genuinely
contended and killing one demonstrates lease recovery while the other keeps working.

For the real Kubernetes path, `.\scripts\kind.ps1 bootstrap` creates a `kind` cluster
named `nexus-local`. Pipelines then build a real image, load it into the cluster, apply a
Deployment and Service, wait for the rollout, and probe the application over its node port
before recording it healthy.

Three demonstrations exercise real code paths rather than simulations:

| | |
|---|---|
| `./scripts/demo pipeline` | A run from checkout to promotion, including a real container build |
| `./scripts/demo failure` | A release that fails verification, and the rollback that recovers it |
| `./scripts/demo kill-worker` | A worker killed mid-job; the lease expires and another worker finishes it |

---

## GitHub social preview

GitHub renders the social preview at **1280 × 640**, and crops to roughly 1200 × 600 on
some surfaces, so keep everything meaningful inside a centred 1200 × 600 safe area. It is
displayed at about half size in a link unfurl, which is the constraint that matters: at
640 × 320 anything below roughly 28px is unreadable.

No image is committed. Generating one here would mean inventing a visual identity in a
format nobody could edit afterwards; this is the specification to hand to whatever tool
you prefer.

### Recommended layout

```
┌──────────────────────────────────────────────────────────────┐
│                                                              │
│   NEXUS                                                      │   ← wordmark, large
│   Software delivery control plane                            │   ← subtitle, ~40% opacity
│                                                              │
│   Source → Pipeline → Build → Image → Deploy → Verify        │   ← the lifecycle strip
│                                    ↓ fails                   │
│                                 Rollback                     │
│                                                              │
│   Go · PostgreSQL · NATS · Kubernetes                        │   ← small, bottom
│                                                              │
└──────────────────────────────────────────────────────────────┘
```

### Content

| | |
|---|---|
| **Title** | `NEXUS` |
| **Subtitle** | `Software delivery control plane` |
| **Supporting line** | `Source → Pipeline → Build → Image → Deploy → Verify → Rollback` |
| **Footer** | `Go · PostgreSQL · NATS · Kubernetes` |

Alternative subtitles, if a different emphasis suits:

- `Distributed pipelines. Real deployments. Observed health.`
- `An internal developer platform that owns the whole lifecycle`

### Visual direction

Reuse the console's existing identity rather than inventing one — a near-black ground,
one restrained accent, monospaced type for the lifecycle strip and a light sans for the
wordmark. That way the preview and the screenshots read as the same product.

The lifecycle strip is the whole idea in one line and should be the second thing the eye
lands on. The branch down to **Rollback** is what distinguishes this from a CI badge, so
it is worth the space even though it costs symmetry.

Avoid: gradient meshes, floating 3D shapes, generic cloud iconography, and stock
"DevOps infinity loop" imagery. They say nothing and they date badly.

### Applying it

Repository → **Settings** → **General** → **Social preview** → *Upload an image*.
