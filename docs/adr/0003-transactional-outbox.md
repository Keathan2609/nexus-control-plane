# 3. Events are written through a transactional outbox

**Status:** Accepted

## Context

When a state change must announce itself, both obvious implementations are wrong.
Publishing inside the transaction means a broker timeout rolls back work that really
happened, or the publish succeeds and the acknowledgement is lost. Publishing after
committing means a crash in between loses the event permanently, with nothing anywhere
recording that it was owed.

Neither failure is detectable afterwards, which makes them worse than an outage.

## Decision

The event is inserted into an `outbox` table in the same transaction as the state change.
A relay drains that table to NATS afterwards, retrying with backoff.

`outbox.Write` takes a `pgx.Tx` rather than a connection pool, so it is not possible to
write an event that is not atomic with something else. That signature is the enforcement.

## Consequences

A committed state change always has its event, and an event never exists without its
state change. Both properties are structural rather than a matter of care.

The cost is at-least-once delivery: the relay can publish a row, fail to record that it
did, and publish it again. Consumers handle this with a `processed_events` guard whose
primary key makes the duplicate lose, recorded in the same transaction as the handler's
own writes. JetStream duplicate suppression on the event id is a second, weaker defence
— weaker because it depends on the broker behaving.

Event delivery also becomes asynchronous, which adds latency between a state change and
its notification. That is acceptable because events are notifications, not state: a
consumer that needs the truth reads Postgres.

## Alternatives considered

**Change data capture from the write-ahead log.** Stronger, and it removes the relay
entirely. Rejected as disproportionate — it needs logical replication, a connector, and
operational knowledge of both, to improve on a table and a loop.

**Two-phase commit between Postgres and NATS.** NATS does not support it, and XA across a
database and a broker is a well-known source of stuck transactions.
