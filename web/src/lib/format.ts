/** Formatting shared across the console, so a duration reads the same on every screen. */

export type Tone = 'healthy' | 'running' | 'warn' | 'failed' | 'drift' | 'idle';

/**
 * Maps a lifecycle state onto a visual tone.
 *
 * Every state in the system passes through here, so a state that is added to the backend
 * and forgotten here shows as idle rather than as an unstyled string.
 */
export function tone(state: string | undefined | null): Tone {
  switch ((state ?? '').toUpperCase()) {
    case 'SUCCEEDED':
    case 'HEALTHY':
    case 'PASS':
    case 'SYNCED':
    case 'ACTIVE':
      return 'healthy';
    case 'RUNNING':
    case 'DEPLOYING':
    case 'VERIFYING':
    case 'PROVISIONING':
    case 'QUEUED':
    case 'REQUESTED':
    case 'ROLLING_BACK':
      return 'running';
    case 'DEGRADED':
    case 'WARN':
    case 'OUT_OF_SYNC':
    case 'DRAINING':
      return 'warn';
    case 'FAILED':
    case 'TIMED_OUT':
    case 'UNHEALTHY':
    case 'FAIL':
    case 'TIMEOUT':
    case 'LOST':
    case 'REJECTED':
      return 'failed';
    case 'DRIFTED':
      return 'drift';
    default:
      return 'idle';
  }
}

/** Human label for a state. Underscores read as machine output in an interface. */
export function stateLabel(state: string | undefined | null): string {
  if (!state) return 'Unknown';
  return state.replace(/_/g, ' ');
}

/**
 * Durations, formatted the way an engineer reads them.
 *
 * Sub-second values keep milliseconds because that is the resolution that matters for a
 * health probe; anything over a minute drops to whole seconds because nobody cares that
 * a build took 3m 42.187s.
 */
export function duration(ms: number | undefined | null): string {
  if (ms === undefined || ms === null) return '—';
  if (ms < 1000) return `${Math.round(ms)}ms`;

  const totalSeconds = Math.floor(ms / 1000);
  if (totalSeconds < 60) {
    const withDecimal = ms / 1000;
    return withDecimal < 10 ? `${withDecimal.toFixed(1)}s` : `${totalSeconds}s`;
  }
  const minutes = Math.floor(totalSeconds / 60);
  const seconds = totalSeconds % 60;
  if (minutes < 60) return `${minutes}m ${String(seconds).padStart(2, '0')}s`;

  const hours = Math.floor(minutes / 60);
  return `${hours}h ${String(minutes % 60).padStart(2, '0')}m`;
}

export function durationSeconds(seconds: number | undefined | null): string {
  if (seconds === undefined || seconds === null) return '—';
  return duration(seconds * 1000);
}

/** Relative time, compact. "8m ago" rather than "8 minutes ago". */
export function ago(iso: string | undefined | null): string {
  if (!iso) return '—';
  const then = new Date(iso).getTime();
  if (Number.isNaN(then)) return '—';

  const seconds = Math.floor((Date.now() - then) / 1000);
  if (seconds < 0) return 'just now';
  if (seconds < 45) return `${seconds}s ago`;

  const minutes = Math.floor(seconds / 60);
  if (minutes < 60) return `${minutes}m ago`;

  const hours = Math.floor(minutes / 60);
  if (hours < 24) return `${hours}h ago`;

  const days = Math.floor(hours / 24);
  if (days < 30) return `${days}d ago`;

  return new Date(iso).toLocaleDateString(undefined, { day: 'numeric', month: 'short' });
}

/** Absolute timestamp for tooltips and detail rows. */
export function timestamp(iso: string | undefined | null): string {
  if (!iso) return '—';
  const d = new Date(iso);
  if (Number.isNaN(d.getTime())) return '—';
  return d.toLocaleString(undefined, {
    day: '2-digit',
    month: 'short',
    hour: '2-digit',
    minute: '2-digit',
    second: '2-digit',
    hour12: false,
  });
}

export function shortSha(sha: string | undefined | null): string {
  if (!sha) return '—';
  return sha.slice(0, 7);
}

/** `main@a84fc12` — the way a commit is referred to in conversation. */
export function ref(branch: string, sha: string): string {
  return `${branch}@${shortSha(sha)}`;
}

export function percent(value: number | undefined | null): string {
  if (value === undefined || value === null) return '—';
  return `${value.toFixed(value % 1 === 0 ? 0 : 1)}%`;
}

export function bytes(n: number | undefined | null): string {
  if (!n) return '—';
  const units = ['B', 'KB', 'MB', 'GB'];
  let value = n;
  let unit = 0;
  while (value >= 1024 && unit < units.length - 1) {
    value /= 1024;
    unit++;
  }
  return `${value.toFixed(value < 10 && unit > 0 ? 1 : 0)}${units[unit]}`;
}

/** Turns an audit action such as `deployment.requested` into `Deployment requested`. */
export function actionLabel(action: string): string {
  const [resource = '', verb = ''] = action.split('.');
  const noun = resource.replace(/_/g, ' ');
  const past = verb.replace(/_/g, ' ');
  return `${noun.charAt(0).toUpperCase()}${noun.slice(1)} ${past}`.trim();
}
