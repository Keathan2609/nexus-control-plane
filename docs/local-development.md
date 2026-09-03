# Local development

## Requirements

Docker Desktop, Go 1.27+, Node 20+. No cloud account, no paid infrastructure, nothing
else.

## Starting

```powershell
.\scripts\dev.ps1 up      # Windows
```

```bash
./scripts/dev up          # macOS, Linux, Git Bash, WSL
```

That brings up the backing services, waits for them to report healthy, builds the five
binaries, applies migrations, seeds the demonstration estate, and starts the API, two
workers, the reconciler and the console.

There is a script rather than a Makefile because `make` on Windows means installing a
toolchain that exists only to run five commands.

| Command | Does |
|---|---|
| `up` | Everything above |
| `down` | Stop the processes, keep the data |
| `nuke` | Stop everything and delete the volumes |
| `seed` | Re-seed the demonstration estate |
| `build` | Build the five binaries into `./bin` |
| `test` | Unit, integration and frontend checks |
| `logs` | Tail the local process logs |
| `status` | What is running |

## Ports

Every host port is offset from its default so this stack coexists with other projects on
the same machine.

| Service | Host port | Default it avoids |
|---|---|---|
| Console | 3100 | 3000 |
| API | 8080 | |
| API metrics | 9095 | 9090 |
| Worker metrics | 9096, 9098 | |
| Reconciler metrics | 9097 | |
| PostgreSQL | 5436 | 5432 |
| NATS | 4222, 8222 | |
| Prometheus | 9091 | 9090 |
| Grafana | 3300 | 3000 |
| Jaeger | 16687, 4319 | 16686, 4318 |

## Configuration

Everything is environment variables, read once at startup into an immutable struct and
validated before any connection is opened.

| Variable | Default | Notes |
|---|---|---|
| `NEXUS_DATABASE_URL` | `postgres://nexus:nexus@localhost:5436/nexus?sslmode=disable` | |
| `NEXUS_NATS_URL` | `nats://localhost:4222` | Optional; absence degrades realtime only |
| `NEXUS_HTTP_ADDR` | `:8080` | |
| `NEXUS_METRICS_ADDR` | `:9090` | Set per process locally to avoid collisions |
| `NEXUS_ENVIRONMENT` | `development` | `production` tightens secret validation |
| `NEXUS_LOG_LEVEL` | `info` | |
| `NEXUS_LOG_FORMAT` | `json` | `text` is easier to read locally |
| `NEXUS_AUTH_JWT_SECRET` | dev fallback | **Required, ≥32 bytes, in production** |
| `NEXUS_WEBHOOK_SECRET` | empty | **Required in production** |
| `NEXUS_WORKER_CONCURRENCY` | `4` | |
| `NEXUS_WORKER_LEASE_SECONDS` | `30` | Minimum 5; heartbeat is a third of this |
| `NEXUS_OTEL_ENDPOINT` | empty | Unset means tracing is a no-op |
| `NEXUS_AUTO_MIGRATE` | `true` | The Helm chart sets this false and uses a Job |

## Working on the backend

```bash
go build ./...
go test ./...                                     # fast, no dependencies
go test -tags=integration ./test/integration/...  # needs Postgres and NATS
gofmt -l . && go vet ./...
```

Integration tests are behind a build tag so `go test ./...` stays fast, and they use a
separate `nexus_test` database. That separation is deliberate: integration tests leave
rows behind by design — that is what makes them exercise the same indexes a populated
system uses — and mixing that residue into the development database would make the
console show numbers that are quietly wrong.

Create it once:

```bash
docker exec nexus-postgres psql -U nexus -d postgres -c "CREATE DATABASE nexus_test OWNER nexus;"
```

### Adding a migration

Add a numbered file to `internal/store/migrations/`, goose format with `-- +goose Up` and
`-- +goose Down`. Migrations are embedded into the binary, so a container image cannot
be built with a binary and a mismatched schema.

They are applied at startup by default. goose takes an advisory lock for the duration, so
several API replicas starting at once cannot apply the same migration twice.

## Working on the console

```bash
cd web
npm run dev        # port 3100
npm run typecheck
npm run lint
```

The console is a browser client for the API. It holds no data of its own and proxies
nothing — every request goes straight to the control plane, so authorisation is enforced
in exactly one place.

The design system lives in `src/styles/tokens.css`. The rules it obeys are written at the
top of that file; the short version is that colour means state, structure is carried by
hairlines and alignment rather than by boxes, and every radius is zero.

## Working on the CLI

```bash
go build -o bin/nexus ./cmd/nexus
./bin/nexus --help
```

The CLI consumes the same REST API as the console and holds no privileged access — no
database connection, no direct queue access, no bypass. Anything it can do, the console
can do.

## Demonstrating failure

The interesting behaviour is in the failure paths, and they are reproducible on demand.

**A worker dying mid-job.** Kill a worker process while it holds a job. Its lease expires
within thirty seconds, the reconciler requeues the job with its attempt count intact, and
the other worker picks it up.

```bash
./scripts/dev status          # find the worker pid
kill <pid>
./bin/nexus pipeline get <run-id>   # the job returns to QUEUED, then runs again
```

**A deployment that never becomes healthy.** The demo services take
`FAIL_HEALTHCHECK=true` and return 503 from `/healthz` immediately. The apply succeeds,
verification never passes, the deployment is recorded `FAILED` with every failed probe
retained, and rollback becomes available.

**A service that degrades after going healthy.** `FAIL_AFTER=30s` serves normally and
then starts failing. This is the harder case: the deployment verifies healthy and
degrades afterwards, which is what the reconciler has to catch rather than the verifier.

**Drift.** The `NoopApplier` exposes `SetObserved`, which changes what `Observe` reports
without touching what was applied. The reconciler notices on its next cycle, records a
drift row, marks the deployment `DRIFTED` and emits `drift.detected`.

## Troubleshooting

**The API exits immediately with a bind error.** Something else holds the metrics port.
Set `NEXUS_METRICS_ADDR` to a free port — `scripts/dev` already does this per process.

**The console shows zeros after re-seeding.** `--reset` creates a new organisation, and
the JWT in the browser still names the old one. Sign out and back in. Every query is
organisation-scoped, so this is the scoping working rather than a bug.

**Docker commands hang on Windows.** Docker Desktop's UI can be running while
`com.docker.service` is stopped. Starting that service needs administrator rights;
restart Docker Desktop as administrator.

**Integration tests hang rather than fail.** A test that abandons an open transaction
keeps its pooled connection checked out, and `pgxpool.Close` then blocks forever. Use the
`beginTx` helper, which registers the rollback with `t.Cleanup`.

**A pipeline sits in QUEUED.** No worker is heartbeating. `./bin/nexus status` reports the
live worker count, and the console says so explicitly on the Infrastructure page.
