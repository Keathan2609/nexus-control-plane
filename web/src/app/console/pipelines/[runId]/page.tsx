'use client';

import Link from 'next/link';
import { useParams } from 'next/navigation';
import { useCallback, useEffect, useMemo, useState, type ReactNode } from 'react';

import { ExecutionLane, lane } from '@/components/lane';
import {
  Button,
  Empty,
  KeyValue,
  Notice,
  PageHeader,
  Reading,
  Readout,
  Section,
  StateBadge,
  ui,
} from '@/components/ui';
import { api, ApiError, openStream, type Job, type LogLine, type Run } from '@/lib/api';
import { ago, duration, shortSha, timestamp } from '@/lib/format';

interface RunResponse {
  run: Run;
  jobs: Job[];
}

const TERMINAL = new Set(['SUCCEEDED', 'FAILED', 'CANCELLED', 'TIMED_OUT']);

export default function RunPage(): ReactNode {
  const params = useParams<{ runId: string }>();
  const runId = params.runId;

  const [data, setData] = useState<RunResponse | null>(null);
  const [selectedId, setSelectedId] = useState<string | null>(null);
  const [logs, setLogs] = useState<LogLine[] | null>(null);
  const [error, setError] = useState<string | null>(null);
  const [cancelling, setCancelling] = useState(false);

  const load = useCallback(() => {
    api
      .get<RunResponse>(`/api/v1/runs/${runId}`)
      .then((res) => {
        setData(res);
        setError(null);
        // Select something useful on first load: the step that failed if one did,
        // otherwise whatever is running, otherwise the last step to finish. Landing on
        // the failure is the whole reason someone opens a failed run.
        setSelectedId((current) => current ?? defaultSelection(res.jobs));
      })
      .catch((err: Error) => setError(err.message));
  }, [runId]);

  useEffect(() => {
    load();
  }, [load]);

  const live = data ? !TERMINAL.has(data.run.state) : false;

  // A finished run cannot change, so the stream subscription is dropped once it is
  // terminal rather than left open for the life of the tab.
  useEffect(() => {
    if (!live) return;
    return openStream(load);
  }, [live, load]);

  useEffect(() => {
    if (!selectedId) return;
    setLogs(null);
    api
      .get<{ data: LogLine[] }>(`/api/v1/jobs/${selectedId}/logs?limit=500`)
      .then((res) => setLogs(res.data))
      .catch(() => setLogs([]));
  }, [selectedId, data?.run.state]);

  const selected = useMemo(
    () => data?.jobs.find((j) => j.id === selectedId) ?? null,
    [data, selectedId],
  );

  async function cancel() {
    setCancelling(true);
    try {
      await api.post(`/api/v1/runs/${runId}/cancel`);
      load();
    } catch (err) {
      setError(err instanceof ApiError ? err.message : 'Could not cancel this run.');
    } finally {
      setCancelling(false);
    }
  }

  if (error && !data) {
    return (
      <>
        <PageHeader title="Run" />
        <Notice variant="error">{error}</Notice>
      </>
    );
  }

  const run = data?.run;
  const jobs = data?.jobs ?? [];
  const done = jobs.filter((j) => TERMINAL.has(j.state) || j.state === 'SKIPPED').length;

  return (
    <>
      <PageHeader
        title={run ? `Run #${run.number}` : 'Run'}
        count={
          run ? (
            <>
              <Link href={`/console/projects/${run.project_id}`}>{run.project_name}</Link>
              {' · '}
              <span className={ui.mono}>
                {run.branch}@{shortSha(run.commit_sha)}
              </span>
              {run.pull_request ? ` · PR #${run.pull_request}` : ''}
            </>
          ) : (
            'Loading'
          )
        }
        subtitle={run?.commit_message}
        actions={
          run && !TERMINAL.has(run.state) ? (
            <Button variant="danger" onClick={cancel} disabled={cancelling}>
              {cancelling ? 'Cancelling' : 'Cancel run'}
            </Button>
          ) : undefined
        }
      />

      {error && <Notice variant="error">{error}</Notice>}

      <Readout>
        <Reading label="State" value={<StateBadge state={run?.state} />} />
        <Reading label="Jobs" value={done} of={jobs.length} />
        <Reading label="Duration" value={duration(run?.duration_ms)} meta={live ? 'elapsed' : 'total'} />
        <Reading label="Trigger" value={run?.trigger.toLowerCase() ?? '—'} meta={run?.commit_author} />
        <Reading label="Started" value={run?.started_at ? ago(run.started_at) : '—'} meta={timestamp(run?.started_at)} />
      </Readout>

      <Section
        label="Execution"
        meta={live ? 'updating live' : `${jobs.length} steps`}
      >
        {jobs.length > 0 ? (
          <ExecutionLane
            jobs={jobs}
            selected={selectedId ?? undefined}
            onSelect={(job) => setSelectedId(job.id)}
          />
        ) : (
          <Empty title="No jobs" hint="This run has not been planned yet." />
        )}
      </Section>

      <div className={lane.layout}>
        <Section label={selected ? `${selected.name} — output` : 'Output'}>
          {!selected && <Empty title="Select a step" hint="Choose a step above to read its output." />}
          {selected && logs === null && <Empty title="Loading output" />}
          {selected && logs?.length === 0 && (
            <Empty
              title="No output"
              hint={
                selected.state === 'BLOCKED'
                  ? 'This step is waiting on an upstream step.'
                  : selected.state === 'SKIPPED'
                    ? 'This step was skipped because an upstream step did not succeed.'
                    : 'This step produced no output.'
              }
            />
          )}
          {selected && logs && logs.length > 0 && (
            <div className={lane.logs}>
              {logs.map((line) => (
                <div key={`${line.attempt}-${line.seq}`} className={lane.logRow}>
                  <span className={lane.logSeq}>{line.seq}</span>
                  <span
                    className={`${lane.logText} ${
                      line.stream === 'stderr'
                        ? lane.logStderr
                        : line.stream === 'system'
                          ? lane.logSystem
                          : ''
                    }`}
                  >
                    {line.message}
                  </span>
                </div>
              ))}
            </div>
          )}
        </Section>

        <div className={lane.panel}>
          {selected ? (
            <>
              <div className={lane.panelSection}>
                <div className={lane.panelLabel}>Step</div>
                <KeyValue
                  entries={[
                    ['State', <StateBadge key="s" state={selected.state} />],
                    ['Type', <span key="t" className={ui.mono}>{selected.type.toLowerCase()}</span>],
                    ['Duration', <span key="d" className={ui.mono}>{duration(selected.duration_ms)}</span>],
                    [
                      'Attempts',
                      <span key="a" className={ui.mono}>
                        {selected.attempt} of {selected.max_attempts}
                      </span>,
                    ],
                    [
                      'Depends on',
                      selected.depends_on.length > 0 ? (
                        <span key="dep" className={ui.mono}>{selected.depends_on.join(', ')}</span>
                      ) : (
                        <span key="dep" className={ui.dim}>nothing</span>
                      )
                    ],
                    [
                      'Worker',
                      <span key="w" className={ui.mono}>{selected.worker_id ?? '—'}</span>,
                    ],
                  ]}
                />
              </div>

              {selected.error_message && (
                <div className={lane.panelSection}>
                  <div className={lane.panelLabel}>Failure</div>
                  <Notice variant="error">
                    <span className={ui.mono}>{selected.error_code}</span>
                    <br />
                    {selected.error_message}
                  </Notice>
                </div>
              )}

              <div className={lane.panelSection}>
                <div className={lane.panelLabel}>Attempt history</div>
                {(selected.attempts ?? []).length === 0 && (
                  <div className={lane.emptyPanel}>Not attempted yet.</div>
                )}
                {(selected.attempts ?? []).map((attempt) => (
                  <div key={attempt.attempt} className={lane.attempt}>
                    <span className={lane.attemptNumber}>
                      {String(attempt.attempt).padStart(2, '0')}
                    </span>
                    <span>
                      <StateBadge state={attempt.state} />
                      {attempt.retry_reason === 'RETRY' && (
                        <span className={lane.attemptReason}> · retry</span>
                      )}
                    </span>
                    <span className={lane.attemptMeta}>{ago(attempt.started_at)}</span>
                    {attempt.error_message && (
                      <span className={lane.attemptError}>{attempt.error_message}</span>
                    )}
                  </div>
                ))}
              </div>
            </>
          ) : (
            <div className={lane.emptyPanel}>Select a step to inspect it.</div>
          )}
        </div>
      </div>
    </>
  );
}

function defaultSelection(jobs: Job[]): string | null {
  const failed = jobs.find((j) => j.state === 'FAILED' || j.state === 'TIMED_OUT');
  if (failed) return failed.id;
  const running = jobs.find((j) => j.state === 'RUNNING');
  if (running) return running.id;
  const finished = [...jobs]
    .filter((j) => j.finished_at)
    .sort((a, b) => (a.finished_at! < b.finished_at! ? 1 : -1));
  return finished[0]?.id ?? jobs[0]?.id ?? null;
}
