# Architecture decision records

One file per decision that was genuinely contested — where a competent engineer could
reasonably have chosen otherwise, and where knowing why we did not matters later.

Decisions that were obvious are not recorded. A directory of thirty ADRs saying "we used
PostgreSQL" is a directory nobody reads.

| | Decision | Status |
|---|---|---|
| [0001](0001-postgres-as-system-of-record.md) | PostgreSQL is the only system of record | Accepted |
| [0002](0002-claim-based-job-dispatch.md) | Workers claim jobs; nothing assigns them | Accepted |
| [0003](0003-transactional-outbox.md) | Events are written through a transactional outbox | Accepted |
| [0004](0004-own-the-workflow-engine.md) | NEXUS owns its workflow engine | Accepted |
| [0005](0005-health-is-verified.md) | A deployment is healthy only when observed to be | Accepted |
| [0006](0006-sse-over-websockets.md) | Realtime updates use SSE, not WebSockets | Accepted |
| [0007](0007-keyset-pagination.md) | List endpoints page by keyset, not offset | Accepted |
