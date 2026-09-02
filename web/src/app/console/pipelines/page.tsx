'use client';

import Link from 'next/link';
import { useCallback, useEffect, useState, type ReactNode } from 'react';

import {
  Button,
  Empty,
  Ledger,
  LedgerRow,
  LoadingRows,
  Notice,
  PageHeader,
  StateBadge,
  ui,
} from '@/components/ui';
import { api, openStream, type Page, type Run } from '@/lib/api';
import { ago, duration, shortSha } from '@/lib/format';

const COLUMNS =
  'minmax(140px,1.1fr) 64px minmax(200px,2.2fr) minmax(110px,0.9fr) 84px 92px 96px';

const FILTERS = [
  { label: 'All', value: '' },
  { label: 'Running', value: 'RUNNING' },
  { label: 'Failed', value: 'FAILED' },
  { label: 'Succeeded', value: 'SUCCEEDED' },
];

export default function PipelinesPage(): ReactNode {
  const [page, setPage] = useState<Page<Run> | null>(null);
  const [filter, setFilter] = useState('');
  const [error, setError] = useState<string | null>(null);

  const load = useCallback(() => {
    const params = new URLSearchParams({ limit: '40' });
    if (filter) params.set('state', filter);
    api
      .get<Page<Run>>(`/api/v1/runs?${params.toString()}`)
      .then((res) => {
        setPage(res);
        setError(null);
      })
      .catch((err: Error) => setError(err.message));
  }, [filter]);

  useEffect(() => {
    load();
  }, [load]);

  useEffect(() => openStream(load), [load]);

  return (
    <>
      <PageHeader
        title="Pipelines"
        count={page ? `${page.data.length} runs${page.has_more ? ' shown' : ''}` : 'Loading'}
        actions={FILTERS.map((f) => (
          <Button
            key={f.value}
            variant={filter === f.value ? 'primary' : 'default'}
            onClick={() => setFilter(f.value)}
          >
            {f.label}
          </Button>
        ))}
      />

      {error && <Notice variant="error">{error}</Notice>}

      <Ledger
        columns={COLUMNS}
        head={['Project', 'Run', 'Commit', 'Branch', 'Jobs', 'Duration', 'State']}
      >
        {!page && <LoadingRows count={10} columns={7} />}
        {page?.data.length === 0 && (
          <Empty
            title="No runs match"
            hint={filter ? 'Clear the filter to see every run.' : 'Trigger a pipeline from a project.'}
          />
        )}
        {page?.data.map((run) => (
          <Link key={run.id} href={`/console/pipelines/${run.id}`}>
            <LedgerRow columns={COLUMNS}>
              <span className={ui.strong}>{run.project_name}</span>
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
              <span className={ui.mono} title={ago(run.created_at)}>
                {duration(run.duration_ms)}
              </span>
              <span className={ui.right}>
                <StateBadge state={run.state} />
              </span>
            </LedgerRow>
          </Link>
        ))}
      </Ledger>
    </>
  );
}
