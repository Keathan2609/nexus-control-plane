'use client';

import Link from 'next/link';
import { useEffect, useState, type ReactNode } from 'react';

import {
  Empty,
  Ledger,
  LedgerRow,
  LoadingRows,
  Notice,
  PageHeader,
  StateBadge,
  ui,
} from '@/components/ui';
import { api, type Page, type Project } from '@/lib/api';
import { ago, percent } from '@/lib/format';

const COLUMNS = 'minmax(160px,1.2fr) minmax(160px,1.3fr) 80px 90px minmax(130px,1fr) 100px 96px';

export default function ProjectsPage(): ReactNode {
  const [page, setPage] = useState<Page<Project> | null>(null);
  const [error, setError] = useState<string | null>(null);

  useEffect(() => {
    api
      .get<Page<Project>>('/api/v1/projects?limit=50')
      .then(setPage)
      .catch((err: Error) => setError(err.message));
  }, []);

  return (
    <>
      <PageHeader
        title="Projects"
        count={page ? `${page.data.length} projects` : 'Loading'}
        subtitle="Each project tracks one repository and delivers it through its own environments."
      />

      {error && <Notice variant="error">{error}</Notice>}

      <Ledger
        columns={COLUMNS}
        head={['Project', 'Repository', 'Runtime', 'Success', 'Production', 'Last run', 'Health']}
      >
        {!page && <LoadingRows count={4} columns={7} />}
        {page?.data.length === 0 && (
          <Empty title="No projects" hint="Import a repository to start delivering it." />
        )}
        {page?.data.map((p) => (
          <Link key={p.id} href={`/console/projects/${p.id}`}>
            <LedgerRow columns={COLUMNS}>
              <span>
                <span className={ui.strong}>{p.name}</span>
                <br />
                <span className={ui.dim} style={{ fontSize: 'var(--text-xs)' }}>
                  {p.description}
                </span>
              </span>
              <span className={`${ui.mono} ${ui.truncate}`}>{p.repository || '—'}</span>
              <span className={ui.mono}>{p.runtime.toLowerCase()}</span>
              <span className={ui.mono}>{percent(p.success_rate)}</span>
              <span className={`${ui.mono} ${ui.truncate}`}>{p.production_revision ?? '—'}</span>
              <span className={ui.mono}>
                {p.last_run_number ? `#${p.last_run_number}` : '—'}
                <br />
                <span className={ui.dim}>{p.last_run_at ? ago(p.last_run_at) : ''}</span>
              </span>
              <span className={ui.right}>
                <StateBadge state={p.production_health ?? 'UNKNOWN'} />
              </span>
            </LedgerRow>
          </Link>
        ))}
      </Ledger>
    </>
  );
}
