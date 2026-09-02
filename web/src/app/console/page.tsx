'use client';

import Link from 'next/link';
import { useCallback, useEffect, useState, type ReactNode } from 'react';

import {
  Empty,
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
import { api, openStream, type Overview } from '@/lib/api';
import { ago, duration, durationSeconds, percent, shortSha } from '@/lib/format';

const RUN_COLUMNS = 'minmax(150px,1.2fr) 68px minmax(200px,2fr) 110px 90px 96px';
const DEPLOY_COLUMNS = 'minmax(150px,1.2fr) minmax(110px,1fr) minmax(140px,1.2fr) 110px 96px';

export default function OverviewPage(): ReactNode {
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
  }, [load]);

  // Refresh on any control-plane event rather than polling on a short interval: the
  // dashboard is quiet most of the time and then changes all at once.
  useEffect(() => openStream(load), [load]);

  if (error) {
    return (
      <>
        <PageHeader title="Overview" />
        <Notice variant="error">{error}</Notice>
      </>
    );
  }

  const m = data?.metrics;
  const failing = (m?.runs_failed_last_24h ?? 0) > 0;

  return (
    <>
      <PageHeader
        title="Overview"
        count={
          m
            ? `${m.projects} projects · ${m.environments} environments · ${m.runs_last_24h} runs in the last 24h`
            : 'Loading'
        }
      />

      <Readout>
        <Reading
          label="Success rate"
          value={m ? percent(m.success_rate) : '—'}
          meta="last 7 days"
          state={m && m.success_rate < 80 ? 'FAILED' : 'SUCCEEDED'}
        />
        <Reading label="Runs" value={m?.runs_last_24h ?? '—'} meta="last 24h" />
        <Reading
          label="Failed"
          value={m?.runs_failed_last_24h ?? '—'}
          meta="last 24h"
          state={failing ? 'FAILED' : undefined}
        />
        <Reading label="In flight" value={m?.runs_running ?? '—'} meta="pipeline runs" />
        <Reading
          label="Median run"
          value={m ? durationSeconds(m.median_run_seconds) : '—'}
          meta="last 7 days"
        />
        <Reading
          label="Median deploy"
          value={m ? durationSeconds(m.median_deploy_seconds) : '—'}
          meta="request to healthy"
        />
        <Reading label="Deployments" value={m?.deployments_last_24h ?? '—'} meta="last 24h" />
        <Reading
          label="Drift"
          value={m?.open_drift ?? '—'}
          meta="unresolved"
          state={m && m.open_drift > 0 ? 'DRIFTED' : undefined}
        />
      </Readout>

      <Section
        label="Recent runs"
        meta={<Link href="/console/pipelines">All pipelines</Link>}
      >
        <Ledger
          columns={RUN_COLUMNS}
          head={['Project', 'Run', 'Commit', 'Branch', 'Duration', 'State']}
        >
          {!data && <LoadingRows count={6} columns={6} />}
          {data?.recent_runs?.length === 0 && (
            <Empty title="No runs yet" hint="Trigger a pipeline from a project to see it here." />
          )}
          {data?.recent_runs?.map((run) => (
            <Link key={run.id} href={`/console/pipelines/${run.id}`}>
              <LedgerRow columns={RUN_COLUMNS}>
                <span className={ui.strong}>{run.project_name}</span>
                <span className={ui.mono}>#{run.number}</span>
                <span className={ui.truncate} title={run.commit_message}>
                  <span className={ui.mono}>{shortSha(run.commit_sha)}</span>{' '}
                  <span className={ui.dim}>{run.commit_message}</span>
                </span>
                <span className={`${ui.mono} ${ui.truncate}`}>{run.branch}</span>
                <span className={ui.mono}>{duration(run.duration_ms)}</span>
                <span className={ui.right}>
                  <StateBadge state={run.state} />
                </span>
              </LedgerRow>
            </Link>
          ))}
        </Ledger>
      </Section>

      <Section
        label="Environments"
        meta={<Link href="/console/environments">All environments</Link>}
      >
        <EnvironmentGrid data={data} />
      </Section>

      <Section
        label="Recent deployments"
        meta={<Link href="/console/deployments">All deployments</Link>}
      >
        <Ledger
          columns={DEPLOY_COLUMNS}
          head={['Environment', 'Release', 'Revision', 'When', 'State']}
        >
          {!data && <LoadingRows count={4} columns={5} />}
          {data?.recent_deployments?.length === 0 && (
            <Empty title="Nothing deployed yet" />
          )}
          {data?.recent_deployments?.map((d) => (
            <Link key={d.id} href={`/console/deployments/${d.id}`}>
              <LedgerRow columns={DEPLOY_COLUMNS}>
                <span className={ui.strong}>{d.environment_name}</span>
                <span className={ui.mono}>
                  {d.release_version}
                  {d.is_rollback && <span className={ui.dim}> · rollback</span>}
                </span>
                <span className={`${ui.mono} ${ui.truncate}`}>{d.desired_revision}</span>
                <span className={ui.mono}>{ago(d.requested_at)}</span>
                <span className={ui.right}>
                  <StateBadge state={d.state} />
                </span>
              </LedgerRow>
            </Link>
          ))}
        </Ledger>
      </Section>

      <Section label="Worker fleet" meta={<Link href="/console/infrastructure">Infrastructure</Link>}>
        <Ledger
          columns="minmax(140px,1fr) minmax(140px,1fr) 100px 100px 96px"
          head={['Worker', 'Host', 'Capacity', 'Last seen', 'State']}
        >
          {!data && <LoadingRows count={3} columns={5} />}
          {data?.workers?.length === 0 && (
            <Empty
              title="No workers registered"
              hint="Start nexus-worker to give the control plane something to execute jobs on."
            />
          )}
          {data?.workers?.map((w) => (
            <LedgerRow key={w.id} columns="minmax(140px,1fr) minmax(140px,1fr) 100px 100px 96px">
              <span className={`${ui.mono} ${ui.truncate}`}>{w.id}</span>
              <span className={`${ui.truncate} ${ui.dim}`}>{w.hostname}</span>
              <span className={ui.mono}>
                {w.active_jobs}
                <span className={ui.dim}>/{w.concurrency}</span>
              </span>
              <span className={ui.mono}>{ago(w.last_heartbeat_at)}</span>
              <span className={ui.right}>
                <StateBadge state={w.live ? 'ACTIVE' : 'LOST'} />
              </span>
            </LedgerRow>
          ))}
        </Ledger>
      </Section>
    </>
  );
}

/**
 * Environments as a row of readings rather than a table.
 *
 * The question this answers is "what is running where, and is it healthy" — four values
 * per environment. A table would give each of those a column heading it does not need.
 */
function EnvironmentGrid({ data }: { data: Overview | null }): ReactNode {
  if (!data) return <LoadingRows count={2} columns={4} />;
  if (data.environments.length === 0) {
    return <Empty title="No environments" hint="Create a project to provision its environments." />;
  }

  return (
    <div
      style={{
        display: 'grid',
        gridTemplateColumns: 'repeat(auto-fill, minmax(260px, 1fr))',
        borderTop: '1px solid var(--rule-strong)',
      }}
    >
      {data.environments.map((env) => (
        <Link
          key={env.id}
          href={`/console/environments`}
          style={{
            padding: 'var(--space-4)',
            borderRight: '1px solid var(--rule)',
            borderBottom: '1px solid var(--rule)',
            display: 'flex',
            flexDirection: 'column',
            gap: 'var(--space-2)',
          }}
        >
          <span style={{ display: 'flex', justifyContent: 'space-between', gap: 'var(--space-2)' }}>
            <span className={ui.strong}>{env.name}</span>
            <StateBadge state={env.health_status ?? env.state} />
          </span>
          <span className={`${ui.mono} ${ui.dim} ${ui.truncate}`}>{env.project_name}</span>
          <span className={ui.mono} style={{ color: 'var(--text-primary)' }}>
            {env.revision ?? '—'}
          </span>
          <span className={`${ui.mono} ${ui.dim}`}>
            {env.deployed_at ? `deployed ${ago(env.deployed_at)}` : 'never deployed'}
            {env.open_drift > 0 && (
              <span style={{ color: 'var(--state-drift)' }}> · drift detected</span>
            )}
          </span>
        </Link>
      ))}
    </div>
  );
}
