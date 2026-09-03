# 1. PostgreSQL is the only system of record

**Status:** Accepted

## Context

A delivery control plane holds pipeline runs, job state, lease ownership and deployment
history, and it also needs to notify other components when those change. The obvious
shape is a database plus a message broker, and the tempting shape is to let the broker
hold the work queue — that is what brokers are for.

## Decision

PostgreSQL holds every authoritative fact. NATS carries notifications only, and no
component reads state from it.

## Consequences

The job queue is a table with a partial index, not a NATS stream. Losing NATS entirely
degrades realtime updates and cross-component fan-out; it never loses a pipeline run,
because the scheduler polls Postgres and would pick the work up regardless.

A design where the queue holds the work and the database holds a copy is a design with
two sources of truth and a permanent reconciliation problem — one that only shows up
under partial failure, which is exactly when it is hardest to debug.

The cost is that dispatch is a database query rather than a push. At the scale a delivery
platform operates at — tens of jobs per second, not hundreds of thousands — a partial
index over the claimable backlog is comfortably fast, and it buys transactional
consistency between claiming a job and recording that it was claimed.

## Alternatives considered

**NATS JetStream as the work queue.** Genuinely good at this, and it would remove the
polling. Rejected because job state would then live in two places: the stream, and the
`jobs` table the console reads. Keeping them consistent across worker crashes and
redeliveries is the problem this decision avoids entirely.

**Redis.** The same objection, plus a third piece of infrastructure for something
Postgres already does correctly.
