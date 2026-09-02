'use client';

import { useEffect, useState, type ReactNode } from 'react';

import { Empty, Ledger, LedgerRow, LoadingRows, Notice, PageHeader, ui } from '@/components/ui';
import { api, type AuditEntry, type Page } from '@/lib/api';
import { actionLabel, ago, timestamp } from '@/lib/format';

const COLUMNS = 'minmax(150px,1fr) minmax(170px,1.2fr) minmax(180px,1.6fr) minmax(120px,1fr) 96px';

export default function AuditPage(): ReactNode {
  const [page, setPage] = useState<Page<AuditEntry> | null>(null);
  const [error, setError] = useState<string | null>(null);

  useEffect(() => {
    api
      .get<Page<AuditEntry>>('/api/v1/audit?limit=100')
      .then(setPage)
      .catch((err: Error) => setError(err.message));
  }, []);

  return (
    <>
      <PageHeader
        title="Audit"
        count={page ? `${page.data.length} events` : 'Loading'}
        subtitle="Append-only. The application role holds INSERT and SELECT on this table and nothing else, so entries cannot be amended or removed after the fact."
      />

      {error && <Notice variant="error">{error}</Notice>}

      <Ledger columns={COLUMNS} head={['Actor', 'Action', 'Resource', 'Detail', 'When']}>
        {!page && <LoadingRows count={12} columns={5} />}
        {page?.data.length === 0 && <Empty title="No audit events recorded" />}
        {page?.data.map((entry) => (
          <LedgerRow key={entry.id} columns={COLUMNS}>
            <span>
              <span className={ui.strong}>{entry.actor_label}</span>
              <br />
              <span className={ui.dim} style={{ fontSize: 'var(--text-2xs)' }}>
                {entry.actor_type}
              </span>
            </span>
            <span>{actionLabel(entry.action)}</span>
            <span className={`${ui.mono} ${ui.truncate}`} title={entry.resource_id}>
              {entry.resource_id}
            </span>
            <span className={`${ui.dim} ${ui.truncate}`}>{summarise(entry.metadata)}</span>
            <span className={`${ui.mono} ${ui.right}`} title={timestamp(entry.occurred_at)}>
              {ago(entry.occurred_at)}
            </span>
          </LedgerRow>
        ))}
      </Ledger>
    </>
  );
}

function summarise(metadata: Record<string, unknown>): string {
  const entries = Object.entries(metadata ?? {});
  if (entries.length === 0) return '—';
  return entries.map(([k, v]) => `${k}=${String(v)}`).join(' · ');
}
