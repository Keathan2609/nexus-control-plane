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
  Notice,
  PageHeader,
  Reading,
  Readout,
  Section,
  StateBadge,
  ui,
} from '@/components/ui';
import { api, ApiError, type Deployment, type HealthCheck } from '@/lib/api';
import { ago, bytes, duration, shortSha, timestamp } from '@/lib/format';

interface Response {
  deployment: Deployment;
  health_checks: HealthCheck[];
}

const CHECK_COLUMNS = '60px 90px 90px 90px minmax(160px,1fr) 110px';

export default function DeploymentPage(): ReactNode {
  const params = useParams<{ deploymentId: string }>();
  const id = params.deploymentId;

  const [data, setData] = useState<Response | null>(null);
  const [error, setError] = useState<string | null>(null);
  const [busy, setBusy] = useState(false);
  const [notice, setNotice] = useState<string | null>(null);

  const load = useCallback(() => {
    api
      .get<Response>(`/api/v1/deployments/${id}`)
      .then((res) => {
        setData(res);
        setError(null);
      })
      .catch((err: Error) => setError(err.message));
  }, [id]);

  useEffect(() => {
    load();
  }, [load]);

  async function rollback() {
    if (!data) return;
    setBusy(true);
    setNotice(null);
    setError(null);
    try {
      await api.post(`/api/v1/environments/${data.deployment.environment_id}/rollback`, {
        reason: `rolled back from deployment #${data.deployment.number}`,
      });
      setNotice('Rollback completed and verified healthy.');
      load();
    } catch (err) {
      setError(err instanceof ApiError ? err.message : 'Rollback failed.');
    } finally {
      setBusy(false);
    }
  }

  if (error && !data) {
    return (
      <>
        <PageHeader title="Deployment" />
        <Notice variant="error">{error}</Notice>
      </>
    );
  }

  const d = data?.deployment;
  const checks = data?.health_checks ?? [];
  const passes = checks.filter((c) => c.status === 'PASS').length;
  const drifted = d?.actual_revision && d.actual_revision !== d.desired_revision;

  return (
    <>
      <PageHeader
        title={d ? `Deployment #${d.number}` : 'Deployment'}
        count={
          d ? (
            <>
              <Link href={`/console/projects/${d.project_id}`}>{d.environment_name}</Link>
              {' · release '}
              <span className={ui.mono}>{d.release_version}</span>
              {d.is_rollback && ' · rollback'}
            </>
          ) : (
            'Loading'
          )
        }
        subtitle={d?.reason}
        actions={
          d ? (
            <Button variant="danger" onClick={rollback} disabled={busy}>
              {busy ? 'Rolling back' : 'Roll back environment'}
            </Button>
          ) : undefined
        }
      />

      {error && <Notice variant="error">{error}</Notice>}
      {notice && <Notice>{notice}</Notice>}

      <Readout>
        <Reading label="State" value={<StateBadge state={d?.state} />} />
        <Reading label="Health" value={<StateBadge state={d?.health_status} />} />
        <Reading
          label="Sync"
          value={<StateBadge state={drifted ? 'DRIFTED' : d?.sync_status} />}
        />
        <Reading label="Duration" value={duration(d?.duration_ms)} meta="request to verified" />
        <Reading label="Checks passed" value={passes} of={checks.length} />
        <Reading label="Requested" value={d ? ago(d.requested_at) : '—'} meta={d?.requested_by} />
      </Readout>

      <div
        style={{
          display: 'grid',
          gridTemplateColumns: 'minmax(0,1fr) minmax(0,1fr)',
          gap: 'var(--space-8)',
          marginBottom: 'var(--space-10)',
        }}
      >
        <Section label="Desired state">
          <KeyValue
            entries={[
              ['Revision', <span key="r" className={ui.mono}>{d?.desired_revision ?? '—'}</span>],
              ['Image', <span key="i" className={ui.mono}>{d?.image ?? '—'}</span>],
              [
                'Digest',
                <span key="dg" className={ui.mono} title={d?.image_digest}>
                  {d?.image_digest ? d.image_digest.slice(0, 26) : '—'}
                </span>,
              ],
              [
                'Image size',
                <span key="sz" className={ui.mono}>{bytes(d?.image_size_bytes)}</span>,
              ],
              ['Commit', <span key="c" className={ui.mono}>{shortSha(d?.commit_sha)}</span>],
              ['Release', <span key="v" className={ui.mono}>{d?.release_version ?? '—'}</span>],
            ]}
          />
        </Section>

        <Section label="Observed state">
          <KeyValue
            entries={[
              [
                'Revision',
                <span
                  key="r"
                  className={ui.mono}
                  style={drifted ? { color: 'var(--state-drift)' } : undefined}
                >
                  {d?.actual_revision ?? 'not observed'}
                </span>,
              ],
              ['Sync status', <StateBadge key="s" state={d?.sync_status} />],
              ['Verified at', <span key="v" className={ui.mono}>{timestamp(d?.verified_at)}</span>],
              ['Finished at', <span key="f" className={ui.mono}>{timestamp(d?.finished_at)}</span>],
            ]}
          />
          {drifted && (
            <div style={{ marginTop: 'var(--space-4)' }}>
              <Notice variant="warn">
                What is running does not match what NEXUS applied. The reconciler recorded
                this as drift.
              </Notice>
            </div>
          )}
        </Section>
      </div>

      <Section
        label="Health verification"
        meta={`${passes} of ${checks.length} probes passed`}
      >
        <Ledger
          columns={CHECK_COLUMNS}
          head={['Attempt', 'Result', 'Status', 'Latency', 'Message', 'When']}
        >
          {checks.length === 0 && (
            <Empty
              title="No probes recorded"
              hint="This deployment did not reach the verification stage."
            />
          )}
          {checks.map((c) => (
            <LedgerRow key={c.attempt} columns={CHECK_COLUMNS}>
              <span className={ui.mono}>{String(c.attempt).padStart(2, '0')}</span>
              <span>
                <StateBadge state={c.status} />
              </span>
              <span className={ui.mono}>{c.http_status ?? '—'}</span>
              <span className={ui.mono}>{c.latency_ms ? `${c.latency_ms}ms` : '—'}</span>
              <span className={`${ui.dim} ${ui.truncate}`}>{c.message || 'responded healthy'}</span>
              <span className={`${ui.mono} ${ui.right}`} title={timestamp(c.checked_at)}>
                {ago(c.checked_at)}
              </span>
            </LedgerRow>
          ))}
        </Ledger>
      </Section>
    </>
  );
}
