'use client';

import Link from 'next/link';
import { useParams } from 'next/navigation';
import { useCallback, useEffect, useState, type ReactNode } from 'react';

import {
  Button,
  Empty,
  KeyValue,
  Ledger,
  LedgerRow,
  LoadingRows,
  Notice,
  PageHeader,
  Section,
  StateBadge,
  ui,
} from '@/components/ui';
import {
  api,
  ApiError,
  openStream,
  type Deployment,
  type Environment,
  type Project,
  type Run,
} from '@/lib/api';
import { ago, duration, shortSha } from '@/lib/format';

interface Response {
  project: Project;
  environments: Environment[];
  recent_runs: Run[];
  recent_deployments: Deployment[];
}

const RUN_COLUMNS = '64px minmax(200px,2fr) minmax(110px,1fr) 80px 88px 96px';
const ENV_COLUMNS = 'minmax(120px,1fr) minmax(140px,1.2fr) minmax(140px,1.2fr) 96px 96px';

export default function ProjectPage(): ReactNode {
  const params = useParams<{ projectId: string }>();
  const id = params.projectId;

  const [data, setData] = useState<Response | null>(null);
  const [error, setError] = useState<string | null>(null);
  const [triggering, setTriggering] = useState(false);
  const [notice, setNotice] = useState<string | null>(null);

  const load = useCallback(() => {
    api
      .get<Response>(`/api/v1/projects/${id}`)
      .then((res) => {
        setData(res);
        setError(null);
      })
      .catch((err: Error) => setError(err.message));
  }, [id]);

  useEffect(() => {
    load();
  }, [load]);

  useEffect(() => openStream(load, id), [load, id]);

  async function trigger() {
    setTriggering(true);
    setNotice(null);
    setError(null);
    try {
      const res = await api.post<{ number: number }>(`/api/v1/projects/${id}/runs`, {
        branch: data?.project.default_branch ?? 'main',
        message: 'Triggered from the console',
      });
      setNotice(`Run #${res.number} queued. Workers will claim its jobs shortly.`);
      load();
    } catch (err) {
      setError(err instanceof ApiError ? err.message : 'Could not trigger a run.');
    } finally {
      setTriggering(false);
    }
  }

  if (error && !data) {
    return (
      <>
        <PageHeader title="Project" />
        <Notice variant="error">{error}</Notice>
      </>
    );
  }

  const p = data?.project;

  return (
    <>
      <PageHeader
        title={p?.name ?? 'Project'}
        count={
          p ? (
            <span className={ui.mono}>
              {p.repository || 'no repository connected'}
              {p.default_branch ? ` · ${p.default_branch}` : ''}
            </span>
          ) : (
            'Loading'
          )
        }
        subtitle={p?.description}
        actions={
          <Button variant="primary" onClick={trigger} disabled={triggering || !p}>
            {triggering ? 'Triggering' : 'Run pipeline'}
          </Button>
        }
      />

      {error && <Notice variant="error">{error}</Notice>}
      {notice && <Notice>{notice}</Notice>}

      <Section label="Where it is deployed" meta={`${data?.environments.length ?? 0} environments`}>
        <Ledger
          columns={ENV_COLUMNS}
          head={['Environment', 'Desired', 'Actual', 'Sync', 'Health']}
        >
          {!data && <LoadingRows count={3} columns={5} />}
          {data?.environments.length === 0 && <Empty title="No environments" />}
          {data?.environments.map((env) => (
            <LedgerRow key={env.id} columns={ENV_COLUMNS}>
              <span className={ui.strong}>{env.name}</span>
              <span className={`${ui.mono} ${ui.truncate}`}>{env.revision ?? '—'}</span>
              <span
                className={`${ui.mono} ${ui.truncate}`}
                style={
                  env.actual_revision && env.actual_revision !== env.revision
                    ? { color: 'var(--state-drift)' }
                    : undefined
                }
              >
                {env.actual_revision ?? '—'}
              </span>
              <span>
                <StateBadge state={env.open_drift > 0 ? 'DRIFTED' : env.sync_status} />
              </span>
              <span className={ui.right}>
                <StateBadge state={env.health_status ?? env.state} />
              </span>
            </LedgerRow>
          ))}
        </Ledger>
      </Section>

      <Section label="Recent runs" meta={<Link href="/console/pipelines">All pipelines</Link>}>
        <Ledger
          columns={RUN_COLUMNS}
          head={['Run', 'Commit', 'Branch', 'Jobs', 'Duration', 'State']}
        >
          {!data && <LoadingRows count={5} columns={6} />}
          {data?.recent_runs.length === 0 && (
            <Empty title="No runs yet" hint="Use Run pipeline above to start one." />
          )}
          {data?.recent_runs.map((run) => (
            <Link key={run.id} href={`/console/pipelines/${run.id}`}>
              <LedgerRow columns={RUN_COLUMNS}>
                <span className={ui.mono}>#{run.number}</span>
                <span className={ui.truncate} title={run.commit_message}>
                  <span className={ui.mono}>{shortSha(run.commit_sha)}</span>{' '}
                  <span className={ui.dim}>{run.commit_message}</span>
                </span>
                <span className={`${ui.mono} ${ui.truncate}`}>
                  {run.pull_request ? `PR #${run.pull_request}` : run.branch}
                </span>
                <span className={ui.mono}>
                  {run.jobs_done}
                  <span className={ui.dim}>/{run.jobs_total}</span>
                </span>
                <span className={ui.mono}>{duration(run.duration_ms)}</span>
                <span className={ui.right}>
                  <StateBadge state={run.state} />
                </span>
              </LedgerRow>
            </Link>
          ))}
        </Ledger>
      </Section>

      <Section label="Recent deployments">
        <Ledger
          columns="minmax(120px,1fr) 100px minmax(140px,1.2fr) 90px 96px"
          head={['Environment', 'Release', 'Revision', 'When', 'State']}
        >
          {!data && <LoadingRows count={4} columns={5} />}
          {data?.recent_deployments.length === 0 && <Empty title="Nothing deployed yet" />}
          {data?.recent_deployments.map((d) => (
            <Link key={d.id} href={`/console/deployments/${d.id}`}>
              <LedgerRow columns="minmax(120px,1fr) 100px minmax(140px,1.2fr) 90px 96px">
                <span className={ui.strong}>{d.environment_name}</span>
                <span className={ui.mono}>{d.release_version}</span>
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

      <Section label="Configuration">
        <KeyValue
          entries={[
            ['Runtime', <span key="r" className={ui.mono}>{p?.runtime.toLowerCase() ?? '—'}</span>],
            ['Repository', <span key="repo" className={ui.mono}>{p?.repository || '—'}</span>],
            ['Default branch', <span key="b" className={ui.mono}>{p?.default_branch || '—'}</span>],
            ['Project id', <span key="i" className={ui.mono}>{p?.id ?? '—'}</span>],
            ['Created', <span key="c">{p ? ago(p.created_at) : '—'}</span>],
          ]}
        />
      </Section>
    </>
  );
}
