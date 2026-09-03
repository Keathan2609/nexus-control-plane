# Observability

A delivery platform that cannot answer "how long do deployments take, and how often do
they fail" is not observable, whatever it exposes about the workloads it deploys.
Everything here is about NEXUS itself.

## Metrics

Prometheus format on each component's `/metrics`, served on a separate port from the API
so the scrape endpoint is not exposed through the public ingress.

### Pipeline execution

| Metric | Type | Labels | Answers |
|---|---|---|---|
| `nexus_pipeline_runs_total` | counter | project, trigger, state | Success rate, and what triggers work |
| `nexus_pipeline_duration_seconds` | histogram | project, state | Is delivery getting slower? |
| `nexus_jobs_total` | counter | type, state | Which step types fail |
| `nexus_job_duration_seconds` | histogram | type, state | Where the critical path goes |
| `nexus_job_retries_total` | counter | type, reason | Which steps are flaky, and why |

### Execution plane

| Metric | Type | Answers |
|---|---|---|
| `nexus_queue_depth` | gauge | Are workers keeping pace? |
| `nexus_worker_jobs_active` | gauge | Per-worker utilisation |
| `nexus_workers_active` | gauge | Is the fleet there? |
| `nexus_leases_reaped_total` | counter | How often workers are dying |

`nexus_leases_reaped_total` is the one to alert on. A steady trickle means workers are
being killed — by OOM, by preemption, by a rollout that is too aggressive — and each one
costs a retried job.

### Delivery

| Metric | Type | Answers |
|---|---|---|
| `nexus_deployments_total` | counter | Deployment success rate per environment |
| `nexus_deployment_duration_seconds` | histogram | Request-to-healthy latency |
| `nexus_health_checks_total` | counter | How often verification fails |
| `nexus_rollbacks_total` | counter | Rollback frequency, manual versus automatic |
| `nexus_drift_detected_total` | counter | How often reality diverges from intent |

### Eventing

| Metric | Type | Answers |
|---|---|---|
| `nexus_events_published_total` | counter | Event throughput by type |
| `nexus_outbox_pending` | gauge | Is the transport reachable? |
| `nexus_outbox_publish_failures_total` | counter | Publish failure rate |
| `nexus_webhooks_total` | counter | Inbound deliveries by outcome |

`nexus_outbox_pending` growing steadily is the single clearest signal that NATS is
unreachable. Execution continues regardless; realtime updates do not.

### Histogram buckets

Buckets are chosen per instrument rather than using the Prometheus defaults, which top
out at ten seconds. That is useless for a deployment that takes four minutes: every real
observation lands in `+Inf` and the histogram can no longer answer what a p95 deployment
costs.

| Instrument | Range |
|---|---|
| HTTP | 5ms → 10s |
| Jobs | 0.5s → 30m |
| Pipelines | 5s → 1h |
| Deployments | 1s → 10m |

## Useful queries

```promql
# Pipeline success rate over the last hour
sum(rate(nexus_pipeline_runs_total{state="SUCCEEDED"}[1h]))
  / sum(rate(nexus_pipeline_runs_total[1h]))

# p95 job duration by step type — where the critical path actually goes
histogram_quantile(0.95,
  sum by (le, type) (rate(nexus_job_duration_seconds_bucket[5m])))

# Queue depth trending up means workers cannot keep pace
nexus_queue_depth{state="QUEUED"}

# Workers dying: each reap is a job that had to be retried
increase(nexus_leases_reaped_total[1h])

# Outbox growing means the event transport is unreachable
nexus_outbox_pending

# Deployments that applied but failed verification
sum(rate(nexus_deployments_total{state="FAILED"}[1h]))

# Retry rate by step type — which steps are flaky
sum by (type) (rate(nexus_job_retries_total[30m]))
```

## Tracing

OpenTelemetry, exported over OTLP/HTTP to whatever collector is configured. When
`NEXUS_OTEL_ENDPOINT` is unset the tracer is a no-op, so a missing collector never stops
the control plane from starting.

The flow worth tracing is not a single request. It is:

```
webhook received
  └─ run created
      ├─ job scheduled
      │   └─ worker execution (a different process, seconds later)
      ├─ job scheduled
      │   └─ worker execution
      └─ deployment requested
          ├─ desired state rendered
          ├─ applied
          └─ health verification (probe, probe, probe)
```

Those steps happen in different processes minutes apart, so trace context travels in the
event envelope rather than in an HTTP header, stitched together by the correlation id.

The sampler is `ParentBased(TraceIDRatioBased)`. Once a run is sampled, every span
belonging to it is sampled too, so a sampled trace is never missing its middle.

Attributes are constructed through helpers — `AttrRun`, `AttrJob`, `AttrDeployment` — so
the same key is never spelled two ways.

## Logs

Structured JSON via `log/slog`. Field names are constants, because a field spelled
`jobID` in one place and `job_id` in another is a field you cannot search on.

```json
{
  "time": "2026-09-03T01:29:44.182Z",
  "level": "INFO",
  "service": "nexus-worker",
  "msg": "job succeeded",
  "job_id": "job_01m1j78sh4q9wpq3bx5aaered2",
  "pipeline_run_id": "run_01m1j78sh4q9wpq3bx5aaered2",
  "worker_id": "wkr_local_1",
  "step": "test",
  "attempt": 1,
  "duration_ms": 2104
}
```

Every request carries `request_id` and `correlation_id` from the middleware, and a
context logger is threaded through so handlers deeper in a request log with those fields
already attached.

The handler carries a redaction list as a backstop: attributes whose key looks like a
credential are replaced with `[redacted]` regardless of what a caller passes.

## Local stack

| Tool | URL | Purpose |
|---|---|---|
| Prometheus | http://localhost:9091 | Scrapes every component |
| Grafana | http://localhost:3300 | Dashboards, anonymous access locally |
| Jaeger | http://localhost:16687 | Traces |

Prometheus scrapes the API on 9095, workers on 9096 and 9098, and the reconciler on 9097
when running the local stack via `scripts/dev`.

## The console

`/console/observability` deliberately does not rebuild Grafana. Doing so would produce a
worse Grafana that also has to be maintained. What it shows is the small set of numbers an
operator wants without leaving the tool, the names of the exported series so they can be
queried properly, and links to the tools that do this job well.
