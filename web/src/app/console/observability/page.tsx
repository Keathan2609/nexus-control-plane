'use client';

import { useCallback, useEffect, useState, type ReactNode } from 'react';

import {
  CodeBlock,
  KeyValue,
  Notice,
  PageHeader,
  Reading,
  Readout,
  Section,
  ui,
} from '@/components/ui';
import { api, type Overview } from '@/lib/api';
import { durationSeconds, percent } from '@/lib/format';

/**
 * Observability, deliberately not a second Grafana.
 *
 * Rebuilding dashboards in the console would produce a worse Grafana that also has to be
 * maintained. What belongs here is the small set of numbers an operator wants without
 * leaving the tool, the names of the series NEXUS exports so they can be queried
 * properly, and a link to the place that does this job well.
 */
export default function ObservabilityPage(): ReactNode {
  const [data, setData] = useState<Overview | null>(null);
  const [error, setError] = useState<string | null>(null);

  const load = useCallback(() => {
    api
      .get<Overview>('/api/v1/overview')
      .then((res) => {
        setData(res);
        setError(null);
      })
      .catch((err: Error) => setError(err.message));
  }, []);

  useEffect(() => {
    load();
    const timer = window.setInterval(load, 10_000);
    return () => window.clearInterval(timer);
  }, [load]);

  const m = data?.metrics;

  return (
    <>
      <PageHeader
        title="Observability"
        count="NEXUS instrumented as itself"
        subtitle="These are the control plane's own signals. Workload telemetry belongs to the workloads; what is here is how the delivery system is behaving."
      />

      {error && <Notice variant="error">{error}</Notice>}

      <Readout>
        <Reading
          label="Success rate"
          value={m ? percent(m.success_rate) : '—'}
          meta="pipeline runs, 7d"
          state={m && m.success_rate < 80 ? 'FAILED' : 'SUCCEEDED'}
        />
        <Reading label="p50 run" value={m ? durationSeconds(m.median_run_seconds) : '—'} meta="7d" />
        <Reading
          label="p50 deploy"
          value={m ? durationSeconds(m.median_deploy_seconds) : '—'}
          meta="request to healthy"
        />
        <Reading label="Queue depth" value={m?.queue_depth ?? '—'} />
        <Reading label="Jobs running" value={m?.jobs_running ?? '—'} />
        <Reading
          label="Outbox lag"
          value={m?.outbox_pending ?? '—'}
          meta="unpublished events"
          state={m && m.outbox_pending > 200 ? 'DEGRADED' : undefined}
        />
        <Reading
          label="Open drift"
          value={m?.open_drift ?? '—'}
          state={m && m.open_drift > 0 ? 'DRIFTED' : undefined}
        />
        <Reading
          label="Incidents"
          value={m?.open_incidents ?? '—'}
          state={m && m.open_incidents > 0 ? 'FAILED' : undefined}
        />
      </Readout>

      <Section label="Exported series" meta="scraped from /metrics on each component">
        <KeyValue
          entries={[
            [
              'Pipelines',
              <span key="a" className={ui.mono}>
                nexus_pipeline_runs_total · nexus_pipeline_duration_seconds
              </span>,
            ],
            [
              'Jobs',
              <span key="b" className={ui.mono}>
                nexus_jobs_total · nexus_job_duration_seconds · nexus_job_retries_total
              </span>,
            ],
            [
              'Execution plane',
              <span key="c" className={ui.mono}>
                nexus_queue_depth · nexus_worker_jobs_active · nexus_workers_active ·
                nexus_leases_reaped_total
              </span>,
            ],
            [
              'Delivery',
              <span key="d" className={ui.mono}>
                nexus_deployments_total · nexus_deployment_duration_seconds ·
                nexus_health_checks_total · nexus_rollbacks_total · nexus_drift_detected_total
              </span>,
            ],
            [
              'Eventing',
              <span key="e" className={ui.mono}>
                nexus_events_published_total · nexus_outbox_pending ·
                nexus_outbox_publish_failures_total · nexus_webhooks_total
              </span>,
            ],
            [
              'HTTP',
              <span key="f" className={ui.mono}>
                nexus_http_requests_total · nexus_http_request_duration_seconds ·
                nexus_http_requests_in_flight
              </span>,
            ],
          ]}
        />
      </Section>

      <Section label="Useful queries" meta="paste into Prometheus">
        <CodeBlock>{`# Pipeline success rate over the last hour
sum(rate(nexus_pipeline_runs_total{state="SUCCEEDED"}[1h]))
  / sum(rate(nexus_pipeline_runs_total[1h]))

# p95 job duration by step type
histogram_quantile(0.95,
  sum by (le, type) (rate(nexus_job_duration_seconds_bucket[5m])))

# Queue depth trending up means workers cannot keep pace
nexus_queue_depth{state="QUEUED"}

# Outbox growing means the event transport is unreachable
nexus_outbox_pending

# Jobs recovered from workers that died mid-execution
increase(nexus_leases_reaped_total[1h])

# Deployments that failed verification
sum(rate(nexus_deployments_total{state="FAILED"}[1h]))`}</CodeBlock>
      </Section>

      <Section label="Where to look">
        <KeyValue
          entries={[
            [
              'Dashboards',
              <a key="a" className={ui.mono} href="http://localhost:3300" target="_blank" rel="noreferrer">
                Grafana — localhost:3300
              </a>,
            ],
            [
              'Metrics',
              <a key="b" className={ui.mono} href="http://localhost:9091" target="_blank" rel="noreferrer">
                Prometheus — localhost:9091
              </a>,
            ],
            [
              'Traces',
              <a key="c" className={ui.mono} href="http://localhost:16687" target="_blank" rel="noreferrer">
                Jaeger — localhost:16687
              </a>,
            ],
            [
              'Traced flow',
              <span key="d" className={ui.dim}>
                webhook → run created → jobs scheduled → worker execution → deployment →
                health verification, joined by one correlation id
              </span>,
            ],
          ]}
        />
      </Section>
    </>
  );
}
