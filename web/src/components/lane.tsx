'use client';

import type { ReactNode } from 'react';

import { duration, tone, type Tone } from '@/lib/format';
import type { Job } from '@/lib/api';
import styles from './lane.module.css';

const TONE_CLASS: Record<Tone, string> = {
  healthy: styles.healthy!,
  running: styles.running!,
  warn: styles.warn!,
  failed: styles.failed!,
  drift: styles.drift!,
  idle: styles.idle!,
};

/**
 * Groups jobs into their dependency waves.
 *
 * The wave number is computed by the planner and stored on the job, so the console does
 * not recompute the graph layout: two clients looking at the same run see the same
 * arrangement, and the arrangement does not shift as jobs complete.
 */
function waves(jobs: Job[]): Job[][] {
  const byWave = new Map<number, Job[]>();
  for (const job of jobs) {
    const list = byWave.get(job.wave) ?? [];
    list.push(job);
    byWave.set(job.wave, list);
  }
  return [...byWave.entries()]
    .sort((a, b) => a[0] - b[0])
    .map(([, list]) => list.sort((a, b) => a.step_key.localeCompare(b.step_key)));
}

export function ExecutionLane({
  jobs,
  selected,
  onSelect,
}: {
  jobs: Job[];
  selected?: string;
  onSelect: (job: Job) => void;
}): ReactNode {
  const columns = waves(jobs);

  return (
    <div className={styles.lane}>
      {columns.map((column, index) => (
        <div key={index} className={styles.wave}>
          <div className={styles.waveLabel}>Wave {index + 1}</div>
          {column.map((job) => (
            <button
              key={job.id}
              type="button"
              className={`${styles.step} ${TONE_CLASS[tone(job.state)]} ${
                selected === job.id ? styles.stepSelected : ''
              }`}
              onClick={() => onSelect(job)}
              title={`${job.name} — ${job.state}`}
            >
              <span className={styles.stepMark} />
              <span className={styles.stepName}>{job.name}</span>
              <span className={styles.stepDuration}>
                {job.attempt > 1 && <span className={styles.stepRetry}>×{job.attempt} </span>}
                {job.state === 'BLOCKED' || job.state === 'SKIPPED'
                  ? '—'
                  : duration(job.duration_ms)}
              </span>
            </button>
          ))}
        </div>
      ))}
    </div>
  );
}

export { styles as lane };
