# Performance and behaviour under load

Every number here was measured on the machine described below, with the harness in
[`test/load`](../test/load). Nothing is extrapolated, and nothing claims a capacity that
was not observed. Reproduce any of it with the commands shown.

## What is being asked

Not "how fast is NEXUS". The question that matters for a control plane is what happens
to work that arrives faster than it can be executed. There are three possible answers,
and only one of them is acceptable:

1. It becomes queue depth, and execution stays bounded. ✅
2. It is refused, explicitly, with a status the client can act on. ✅
3. It becomes concurrent work until the machine runs out of something. ❌

NEXUS is built for the first two. The measurements below are chosen to show which one
actually happens.

## Measurement environment

| | |
|---|---|
| Host | Windows 11, 16 logical CPUs |
| Go | 1.27.0 |
| PostgreSQL | 16.14 (container), pool capped at 16 connections |
| NATS | 2.10.29 (container), JetStream enabled |
| API replicas | 1 |
| Workers | 1 and 4, at 4 slots each |
| Deploy provider | kubernetes (kind cluster `nexus-local`) |

A single laptop with one API replica. Read every figure in that light: they demonstrate
the *shape* of the system's behaviour, not the ceiling of the design.

## Read path

```powershell
go run ./test/load -scenario=api -requests=5000  -concurrency=50
go run ./test/load -scenario=api -requests=10000 -concurrency=200
```

| Concurrency | Requests | Throughput | p50 | p95 | p99 | Failures |
|---:|---:|---:|---:|---:|---:|---:|
| 50 | 5,000 | 2,221 req/s | 20.4 ms | 36.6 ms | 59.5 ms | 0 |
| 200 | 10,000 | 2,799 req/s | 69.1 ms | 91.2 ms | 112.1 ms | 0 |

Quadrupling concurrency moved p50 from 20 ms to 69 ms and left throughput roughly flat.
That is the correct shape and it is not an accident: the connection pool is capped at 16,
so beyond a certain point additional callers wait for a connection instead of adding
load to Postgres. Latency absorbs the excess. Nothing fails.

The alternative — an unbounded pool — would push the contention into Postgres, where it
becomes everyone's problem rather than the caller's.

These runs were taken with the limiter disabled (`NEXUS_API_RATE_LIMIT=0`) to measure
capacity rather than policy.

## Rate limiting

With the default limiter (50 requests/second, burst 200, keyed per principal):

```
2,000 requests, 50 concurrent callers, one credential
    200 ok             219
    429 rate limited  1,781
    0 failures
```

All 50 callers share one bucket because they share one principal, which is the intent:
the limiter exists so that one client cannot saturate a replica, not to meter individual
connections. Every refused request got a 429 and a `Retry-After`. No request hung and
none failed.

The limiter is **per process**. Two API replicas allow twice the configured rate. This
is a deliberate trade — a shared counter would mean Redis, and a new dependency and
failure mode for a guarantee NEXUS does not need — but it is a real limitation and it
is listed as one.

`X-Forwarded-For` is ignored unless `NEXUS_TRUST_PROXY_HEADERS=true`. It is
client-controlled, so believing it unconditionally lets a caller land every request in a
fresh bucket; the login limiter in particular then stops slowing password guessing at
all. That was a real defect, and there is a test that fails if it returns.

## Write path: what a burst actually does

```powershell
go run ./test/load -scenario=pipeline -requests=300 -concurrency=30
```

```
300 pipeline submissions accepted in 5.27s   (all 202 Accepted, 0 failures)
p50 314 ms   p95 1.12 s   p99 1.68 s
queue depth   26 → 317
```

Three hundred submissions produced **queue depth of 317, not 300 concurrent builds**.
Each request writes a run and its jobs in one transaction and returns; nothing executes
inside the request.

Sampling the database while a worker chewed through the backlog:

```
time      queued  running  worker slots
12:00:50    408      4        4/4
12:01:00    484      4        4/4
12:01:11    520      4        4/4
12:01:25    618      4        4/4
12:01:35    664      4        4/4
```

Queue depth climbed past 600. Jobs actually running stayed at **exactly 4** — the
worker's slot count — for the entire window. That is the property the whole execution
model exists to provide.

## Horizontal scaling

Same backlog, workers added, completions counted over 30 seconds:

| Workers | Slots | Jobs completed / 30s | Concurrent jobs observed |
|---:|---:|---:|---:|
| 1 | 4 | 101 | exactly 4 |
| 4 | 16 | 341 | exactly 16 |

**3.38× the throughput for 4× the workers.** Close to linear, and the shortfall is
expected: every worker polls the same table, so added workers add claim contention as
well as capacity.

Scaling is possible because workers **claim** work rather than being assigned it. Adding
a worker requires no coordinator, no rebalancing and no configuration change anywhere
else — it starts, registers, and begins claiming. There is no leader to elect and no
partition map to redistribute.

The claim is a single statement:

```sql
UPDATE jobs SET state = 'RUNNING', worker_id = $1, ...
 WHERE id = (SELECT id FROM jobs
              WHERE state = 'QUEUED' AND run_at <= now()
              ORDER BY priority DESC, run_at, id
              FOR UPDATE SKIP LOCKED
              LIMIT 1)
RETURNING ...
```

`FOR UPDATE SKIP LOCKED` is what makes this safe: a row another worker is claiming is
skipped rather than waited on, so N workers claim N distinct jobs concurrently without
blocking one another. `TestConcurrentClaimsNeverDuplicateAJob` runs 16 concurrent
claimers against 40 jobs and asserts no job is ever handed out twice.

## Where the limits actually are

Honestly, in order of which is hit first:

1. **Worker slots** — the intended bottleneck. Raise with more workers or more slots per
   worker. This is the one that should bind.
2. **Database connection pool** (16 per process). Above ~200 concurrent API callers,
   latency is dominated by waiting for a connection.
3. **PostgreSQL itself.** Every claim is a write against one hot table. At high worker
   counts this becomes the ceiling, and it is a single-instance ceiling — NEXUS has no
   sharding.
4. **The outbox relay**, single-threaded per replica, draining in batches of 128 on a
   1-second tick. Sustained event production above roughly that rate grows the outbox.
   Nothing is lost, but the console falls behind.

NATS is not on this list. It carries notifications, not truth; when it is down the
control plane keeps working and events accumulate in the outbox until it returns.

## What has not been demonstrated

- **No multi-replica API test.** One process, one machine. Per-process rate limiting in
  particular would behave differently across replicas.
- **No sustained soak.** The longest run here is seconds, not hours. Nothing is claimed
  about memory behaviour over days.
- **No large-cluster deployment throughput.** Deployments were exercised against a
  single-node kind cluster.
- **Nothing at internet scale.** These are laptop numbers from a single API process. A
  figure like "2,799 req/s" is a measurement of this setup, not a capacity claim.

## Reproducing

Bring the stack up ([local development](local-development.md)), then:

```powershell
go run ./test/load -scenario=api      -requests=5000 -concurrency=50
go run ./test/load -scenario=pipeline -requests=300  -concurrency=30
go run ./test/load -scenario=mixed    -duration=30s  -concurrency=40
go run ./test/load -scenario=stream   -requests=200  -concurrency=50
```

The harness exits non-zero if any response was neither a success nor deliberate
backpressure. A 429 or a 503 is the system working; anything else is a failure and is
reported as one.

Watch execution stay bounded while the queue grows:

```powershell
docker exec nexus-postgres psql -U nexus -d nexus -c `
  "SELECT state, count(*) FROM jobs GROUP BY state ORDER BY 2 DESC;"
```
