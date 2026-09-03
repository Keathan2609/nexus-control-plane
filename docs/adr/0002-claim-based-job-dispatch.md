# 2. Workers claim jobs; nothing assigns them

**Status:** Accepted

## Context

Several workers must execute jobs from one queue without ever running the same job twice.
The usual answers are a distributed lock, a leader that assigns work, or a broker with
exclusive consumers.

## Decision

Workers claim jobs with a single conditional statement:

```sql
UPDATE jobs SET state='RUNNING', worker_id=$1, lease_expires_at=now()+$2
WHERE id = (
  SELECT id FROM jobs WHERE state='QUEUED' AND run_at <= now()
  ORDER BY priority DESC, run_at, id
  FOR UPDATE SKIP LOCKED LIMIT 1
) RETURNING ...;
```

Ownership is a lease with an expiry, renewed by heartbeat.

## Consequences

`FOR UPDATE SKIP LOCKED` makes concurrent claims safe with no distributed lock, no leader
election and no external coordination. A worker that finds a row locked steps past it;
the loser of a race is never even delayed.

More importantly, **worker death stops being a special case**. A worker that crashes, is
killed, is partitioned or is stopped mid-job simply stops renewing its lease. There is no
supervisor to notice, no registry to keep consistent, and no difference between those
cases — all of them are the absence of a heartbeat, recovered by the same query that
handles any other retry.

The cost is that a dead worker's job is not recovered instantly. It waits out the
remainder of its lease, up to thirty seconds by default. That is a deliberate trade:
shortening the lease shortens recovery but increases the chance of reaping a healthy
worker that hit one slow database round trip. The heartbeat interval is a third of the
lease, so a live worker gets two chances to renew before expiry.

## Alternatives considered

**A distributed lock per job.** Adds a system that can fail, and needs its own liveness
story — which is the problem it was meant to solve.

**A leader that assigns work.** Needs leader election, and the leader becomes both a
bottleneck and a single point of failure for dispatch.

**Broker exclusive consumers.** Ties job ownership to a broker connection, so a broker
restart becomes a work-loss event.
