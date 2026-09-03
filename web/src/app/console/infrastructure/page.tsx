'use client';

import { useCallback, useEffect, useState, type ReactNode } from 'react';

import {
  Empty,
  KeyValue,
  Ledger,
  LedgerRow,
  LoadingRows,
  Notice,
  PageHeader,
  Reading,
  Readout,
  Section,
  StateBadge,
  ui,
} from '@/components/ui';
import { api, type Overview } from '@/lib/api';
import { ago, timestamp } from '@/lib/format';

const COLUMNS = 'minmax(180px,1.4fr) minmax(140px,1fr) 90px 100px 100px 96px';

/**
 * The execution plane and the platform it runs on.
 *
 * Worker liveness here is derived from the last heartbeat rather than read from the
 * state column, so a worker that was killed without deregistering is shown as gone
 * rather than as active — which matches what the scheduler believes about it.
 */
export default function InfrastructurePage(): ReactNode {
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
    const timer = window.setInterval(load, 5000);
    return () => window.clearInterval(timer);
  }, [load]);

  const m = data?.metrics;
  const workers = data?.workers ?? [];
  const capacity = workers.filter((w) => w.live).reduce((sum, w) => sum + w.concurrency, 0);
  const inUse = workers.filter((w) => w.live).reduce((sum, w) => sum + w.active_jobs, 0);

  return (
    <>
      <PageHeader
        title="Infrastructure"
        count={`${workers.filter((w) => w.live).length} live workers · ${capacity} slots`}
        subtitle="Workers claim jobs; they are never assigned them. A worker that stops heartbeating has its jobs returned to the queue by the reconciler."
      />

      {error && <Notice variant="error">{error}</Notice>}

      <Readout>
        <Reading label="Queue depth" value={m?.queue_depth ?? '—'} meta="claimable jobs" />
        <Reading label="Executing" value={m?.jobs_running ?? '—'} meta="jobs in flight" />
        <Reading label="Capacity" value={inUse} of={capacity} meta="slots in use" />
        <Reading
          label="Live workers"
          value={m?.workers_live ?? '—'}
          state={m && m.workers_live === 0 ? 'FAILED' : 'SUCCEEDED'}
        />
        <Reading
          label="Outbox"
          value={m?.outbox_pending ?? '—'}
          meta="awaiting publication"
          state={m && m.outbox_pending > 200 ? 'DEGRADED' : undefined}
        />
      </Readout>

      {m?.workers_live === 0 && (
        <div style={{ marginBottom: 'var(--space-8)' }}>
          <Notice variant="warn">
            No worker is heartbeating. Queued jobs will sit in the queue until one starts.
            Run <span className={ui.mono}>nexus-worker</span> to give the control plane
            somewhere to execute.
          </Notice>
        </div>
      )}

      <Section label="Worker fleet" meta={`${workers.length} registered`}>
        <Ledger
          columns={COLUMNS}
          head={['Worker', 'Host', 'Slots', 'Active', 'Last seen', 'State']}
        >
          {!data && <LoadingRows count={3} columns={6} />}
          {data && workers.length === 0 && (
            <Empty
              title="No workers have registered"
              hint="Workers register on start and are recovered by lease expiry if they stop."
            />
          )}
          {workers.map((w) => (
            <LedgerRow key={w.id} columns={COLUMNS}>
              <span className={`${ui.mono} ${ui.truncate}`}>{w.id}</span>
              <span className={`${ui.truncate} ${ui.dim}`}>{w.hostname}</span>
              <span className={ui.mono}>{w.concurrency}</span>
              <span className={ui.mono}>{w.active_jobs}</span>
              <span className={ui.mono} title={timestamp(w.last_heartbeat_at)}>
                {ago(w.last_heartbeat_at)}
              </span>
              <span className={ui.right}>
                <StateBadge state={w.live ? 'ACTIVE' : 'LOST'} />
              </span>
            </LedgerRow>
          ))}
        </Ledger>
      </Section>

      <Section label="Platform">
        <KeyValue
          entries={[
            [
              'Deploy provider',
              <span key="p" className={ui.mono}>
                {data?.deploy_provider ?? '—'}
                <span className={ui.dim}>
                  {data?.deploy_provider === 'kubernetes'
                    ? '  manifests applied to a cluster, rollout awaited, node port probed'
                    : data?.deploy_provider === 'docker'
                      ? '  containers on the local daemon, published port probed'
                      : ''}
                </span>
              </span>,
            ],
            ['Control plane', <span key="a" className={ui.mono}>nexus-api</span>],
            ['Execution plane', <span key="b" className={ui.mono}>nexus-worker</span>],
            ['Control loops', <span key="c" className={ui.mono}>nexus-reconciler</span>],
            ['System of record', <span key="d" className={ui.mono}>PostgreSQL 16</span>],
            ['Event transport', <span key="e" className={ui.mono}>NATS JetStream</span>],
            [
              'Metrics',
              <a key="f" className={ui.mono} href="http://localhost:9091" target="_blank" rel="noreferrer">
                Prometheus :9091
              </a>,
            ],
            [
              'Dashboards',
              <a key="g" className={ui.mono} href="http://localhost:3300" target="_blank" rel="noreferrer">
                Grafana :3300
              </a>,
            ],
            [
              'Traces',
              <a key="h" className={ui.mono} href="http://localhost:16687" target="_blank" rel="noreferrer">
                Jaeger :16687
              </a>,
            ],
          ]}
        />
      </Section>
    </>
  );
}
