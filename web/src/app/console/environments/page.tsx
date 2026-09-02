'use client';

import { useCallback, useEffect, useState, type ReactNode } from 'react';

import {
  Button,
  Empty,
  Ledger,
  LedgerRow,
  LoadingRows,
  Notice,
  PageHeader,
  Section,
  StateBadge,
  ui,
} from '@/components/ui';
import { api, ApiError, openStream, type Environment } from '@/lib/api';
import { ago, timestamp } from '@/lib/format';

/**
 * Environments are grouped by kind rather than listed flat.
 *
 * Production and a pull-request preview are not the same class of thing, and sorting
 * them into one alphabetical list buries the environment that matters underneath twenty
 * that do not.
 */
const KIND_ORDER = ['PRODUCTION', 'STAGING', 'DEVELOPMENT', 'PREVIEW'];

const COLUMNS =
  'minmax(140px,1fr) minmax(130px,1fr) minmax(140px,1.1fr) minmax(140px,1.1fr) 110px 96px 110px';

export default function EnvironmentsPage(): ReactNode {
  const [items, setItems] = useState<Environment[] | null>(null);
  const [error, setError] = useState<string | null>(null);
  const [busy, setBusy] = useState<string | null>(null);
  const [notice, setNotice] = useState<string | null>(null);

  const load = useCallback(() => {
    api
      .get<{ data: Environment[] }>('/api/v1/environments')
      .then((res) => {
        setItems(res.data);
        setError(null);
      })
      .catch((err: Error) => setError(err.message));
  }, []);

  useEffect(() => {
    load();
  }, [load]);

  useEffect(() => openStream(load), [load]);

  async function rollback(env: Environment) {
    setBusy(env.id);
    setNotice(null);
    setError(null);
    try {
      await api.post(`/api/v1/environments/${env.id}/rollback`, {
        reason: `rolled back from the console`,
      });
      setNotice(`Rolled ${env.name} back to its last healthy release.`);
      load();
    } catch (err) {
      setError(
        err instanceof ApiError
          ? err.message
          : `Could not roll ${env.name} back.`,
      );
    } finally {
      setBusy(null);
    }
  }

  const grouped = KIND_ORDER.map((kind) => ({
    kind,
    rows: (items ?? []).filter((e) => e.kind === kind),
  })).filter((g) => g.rows.length > 0 || !items);

  return (
    <>
      <PageHeader
        title="Environments"
        count={items ? `${items.length} environments` : 'Loading'}
        subtitle="Desired revision is what NEXUS applied. Actual revision is what it observes running. A difference between them is drift."
      />

      {error && <Notice variant="error">{error}</Notice>}
      {notice && <Notice>{notice}</Notice>}

      {!items && <LoadingRows count={6} columns={7} />}

      {items &&
        grouped.map((group) => (
          <Section
            key={group.kind}
            label={group.kind.toLowerCase()}
            meta={`${group.rows.length} environment${group.rows.length === 1 ? '' : 's'}`}
          >
            <Ledger
              columns={COLUMNS}
              head={['Environment', 'Project', 'Desired', 'Actual', 'Sync', 'Health', '']}
            >
              {group.rows.length === 0 && <Empty title="None" />}
              {group.rows.map((env) => (
                <LedgerRow key={env.id} columns={COLUMNS}>
                  <span>
                    <span className={ui.strong}>{env.name}</span>
                    <br />
                    <span className={ui.dim} style={{ fontSize: 'var(--text-2xs)' }}>
                      {env.namespace}
                    </span>
                  </span>
                  <span className={ui.truncate}>{env.project_name}</span>
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
                  <span>
                    <StateBadge state={env.health_status ?? env.state} />
                  </span>
                  <span className={ui.right}>
                    {env.revision ? (
                      <Button
                        variant="danger"
                        disabled={busy === env.id}
                        onClick={() => rollback(env)}
                        title="Deploy the last healthy release into this environment"
                      >
                        {busy === env.id ? 'Rolling back' : 'Roll back'}
                      </Button>
                    ) : (
                      <span className={ui.dim} style={{ fontSize: 'var(--text-2xs)' }}>
                        never deployed
                      </span>
                    )}
                  </span>

                  {env.kind === 'PREVIEW' && (
                    <span
                      style={{
                        gridColumn: '1 / -1',
                        fontSize: 'var(--text-xs)',
                        color: 'var(--text-tertiary)',
                        paddingTop: 'var(--space-2)',
                      }}
                    >
                      Created from pull request #{env.pull_request} on{' '}
                      <span className={ui.mono}>{env.source_branch}</span>
                      {env.expires_at && (
                        <>
                          {' · expires '}
                          <span title={timestamp(env.expires_at)}>{ago(env.expires_at)}</span>
                        </>
                      )}
                    </span>
                  )}
                </LedgerRow>
              ))}
            </Ledger>
          </Section>
        ))}
    </>
  );
}
