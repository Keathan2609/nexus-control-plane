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
  StateBadge,
  ui,
} from '@/components/ui';
import { api, openStream, type Deployment, type Page } from '@/lib/api';
import { ago, duration, shortSha } from '@/lib/format';

const COLUMNS = 'minmax(130px,1fr) minmax(110px,0.8fr) minmax(150px,1.2fr) 110px 92px 84px 100px';

export default function DeploymentsPage(): ReactNode {
  const [page, setPage] = useState<Page<Deployment> | null>(null);
  const [error, setError] = useState<string | null>(null);

  const load = useCallback(() => {
    api
      .get<Page<Deployment>>('/api/v1/deployments?limit=50')
      .then((res) => {
        setPage(res);
        setError(null);
      })
      .catch((err: Error) => setError(err.message));
  }, []);

  useEffect(() => {
    load();
  }, [load]);

  useEffect(() => openStream(load), [load]);

  return (
    <>
      <PageHeader
        title="Deployments"
        count={page ? `${page.data.length} deployments` : 'Loading'}
        subtitle="Every deployment records the release it carried, the revision it applied, and the health checks that verified it."
      />

      {error && <Notice variant="error">{error}</Notice>}

      <Ledger
        columns={COLUMNS}
        head={['Environment', 'Release', 'Revision', 'Sync', 'Duration', 'When', 'State']}
      >
        {!page && <LoadingRows count={8} columns={7} />}
        {page?.data.length === 0 && <Empty title="Nothing deployed yet" />}
        {page?.data.map((d) => (
          <Link key={d.id} href={`/console/deployments/${d.id}`}>
            <LedgerRow columns={COLUMNS}>
              <span className={ui.strong}>
                {d.environment_name}
                {d.is_rollback && (
                  <span className={ui.dim} style={{ fontWeight: 400 }}>
                    {' '}
                    · rollback
                  </span>
                )}
              </span>
              <span className={ui.mono}>
                {d.release_version}
                <br />
                <span className={ui.dim}>{shortSha(d.commit_sha)}</span>
              </span>
              <span className={`${ui.mono} ${ui.truncate}`}>{d.desired_revision}</span>
              <span>
                <StateBadge state={d.sync_status} />
              </span>
              <span className={ui.mono}>{duration(d.duration_ms)}</span>
              <span className={ui.mono}>{ago(d.requested_at)}</span>
              <span className={ui.right}>
                <StateBadge state={d.state} />
              </span>
            </LedgerRow>
          </Link>
        ))}
      </Ledger>
    </>
  );
}
