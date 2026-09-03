import { describe, expect, it, vi, afterEach } from 'vitest';

import {
  actionLabel,
  ago,
  bytes,
  duration,
  durationSeconds,
  percent,
  ref,
  shortSha,
  stateLabel,
  tone,
} from './format';

// These functions decide what an operator reads on every screen, so the cases that
// matter are the awkward ones: a missing value, a boundary, a state nobody added here.

describe('tone', () => {
  it('maps terminal success states to healthy', () => {
    for (const state of ['SUCCEEDED', 'HEALTHY', 'PASS', 'SYNCED', 'ACTIVE']) {
      expect(tone(state)).toBe('healthy');
    }
  });

  it('maps in-flight states to running', () => {
    for (const state of ['RUNNING', 'DEPLOYING', 'VERIFYING', 'QUEUED', 'ROLLING_BACK']) {
      expect(tone(state)).toBe('running');
    }
  });

  it('maps failure states to failed', () => {
    for (const state of ['FAILED', 'TIMED_OUT', 'UNHEALTHY', 'LOST', 'REJECTED']) {
      expect(tone(state)).toBe('failed');
    }
  });

  it('gives drift its own tone, because it is neither healthy nor failed', () => {
    expect(tone('DRIFTED')).toBe('drift');
  });

  // The point of the default branch: a state added to the backend and forgotten here
  // must render as something rather than as an unstyled string.
  it('falls back to idle for an unknown state', () => {
    expect(tone('SOMETHING_NEW')).toBe('idle');
    expect(tone(undefined)).toBe('idle');
    expect(tone(null)).toBe('idle');
    expect(tone('')).toBe('idle');
  });

  it('is case insensitive', () => {
    expect(tone('succeeded')).toBe('healthy');
    expect(tone('Running')).toBe('running');
  });
});

describe('stateLabel', () => {
  it('replaces underscores so states do not read as machine output', () => {
    expect(stateLabel('ROLLED_BACK')).toBe('ROLLED BACK');
    expect(stateLabel('TIMED_OUT')).toBe('TIMED OUT');
  });

  it('never renders an empty label', () => {
    expect(stateLabel(undefined)).toBe('Unknown');
    expect(stateLabel(null)).toBe('Unknown');
    expect(stateLabel('')).toBe('Unknown');
  });
});

describe('duration', () => {
  it('keeps milliseconds below a second, the resolution a health probe needs', () => {
    expect(duration(0)).toBe('0ms');
    expect(duration(14)).toBe('14ms');
    expect(duration(999)).toBe('999ms');
  });

  it('shows one decimal under ten seconds', () => {
    expect(duration(1000)).toBe('1.0s');
    expect(duration(2145)).toBe('2.1s');
  });

  it('drops the decimal from ten seconds up', () => {
    expect(duration(17_400)).toBe('17s');
    expect(duration(59_000)).toBe('59s');
  });

  it('switches to minutes and pads the seconds', () => {
    expect(duration(60_000)).toBe('1m 00s');
    expect(duration(222_000)).toBe('3m 42s');
  });

  it('switches to hours past sixty minutes', () => {
    expect(duration(3_600_000)).toBe('1h 00m');
    expect(duration(5_400_000)).toBe('1h 30m');
  });

  it('renders a dash rather than NaN when there is no value', () => {
    expect(duration(undefined)).toBe('—');
    expect(duration(null)).toBe('—');
  });
});

describe('durationSeconds', () => {
  it('converts to milliseconds and formats identically', () => {
    expect(durationSeconds(25.2)).toBe('25s');
    expect(durationSeconds(0.61)).toBe('610ms');
  });

  it('renders a dash when there is no value', () => {
    expect(durationSeconds(undefined)).toBe('—');
    expect(durationSeconds(null)).toBe('—');
  });
});

describe('ago', () => {
  afterEach(() => {
    vi.useRealTimers();
  });

  const at = (iso: string) => {
    vi.useFakeTimers();
    vi.setSystemTime(new Date(iso));
  };

  it('counts seconds, then minutes, then hours, then days', () => {
    at('2026-09-03T12:00:00Z');
    expect(ago('2026-09-03T11:59:50Z')).toBe('10s ago');
    expect(ago('2026-09-03T11:30:00Z')).toBe('30m ago');
    expect(ago('2026-09-03T06:00:00Z')).toBe('6h ago');
    expect(ago('2026-08-31T12:00:00Z')).toBe('3d ago');
  });

  // Clock skew between the API and the browser can produce a timestamp in the future.
  // "-3s ago" would look like a bug in the control plane rather than in the clock.
  it('does not render a negative age', () => {
    at('2026-09-03T12:00:00Z');
    expect(ago('2026-09-03T12:00:30Z')).toBe('just now');
  });

  it('renders a dash for a missing or unparseable timestamp', () => {
    expect(ago(undefined)).toBe('—');
    expect(ago(null)).toBe('—');
    expect(ago('not a date')).toBe('—');
  });
});

describe('shortSha and ref', () => {
  it('truncates a commit to seven characters', () => {
    expect(shortSha('a84fc1234567890abcdef')).toBe('a84fc12');
  });

  it('leaves an already-short value alone', () => {
    expect(shortSha('abc')).toBe('abc');
  });

  it('renders a dash rather than an empty cell', () => {
    expect(shortSha(undefined)).toBe('—');
    expect(shortSha(null)).toBe('—');
  });

  it('formats a ref the way a commit is referred to in conversation', () => {
    expect(ref('main', 'a84fc1234567890')).toBe('main@a84fc12');
  });
});

describe('percent', () => {
  it('drops the decimal on a whole number', () => {
    expect(percent(100)).toBe('100%');
    expect(percent(0)).toBe('0%');
  });

  it('keeps one decimal otherwise', () => {
    expect(percent(84.9)).toBe('84.9%');
  });

  it('renders a dash when there is no value', () => {
    expect(percent(undefined)).toBe('—');
    expect(percent(null)).toBe('—');
  });
});

describe('bytes', () => {
  it('scales through the units', () => {
    expect(bytes(512)).toBe('512B');
    expect(bytes(1024)).toBe('1.0KB');
    expect(bytes(3_555_324)).toBe('3.4MB');
  });

  it('drops the decimal once the number is large enough not to need it', () => {
    expect(bytes(15_728_640)).toBe('15MB');
  });

  it('renders a dash for zero or missing, since a zero-byte image is not a real one', () => {
    expect(bytes(0)).toBe('—');
    expect(bytes(undefined)).toBe('—');
    expect(bytes(null)).toBe('—');
  });
});

describe('actionLabel', () => {
  it('turns an audit action into a sentence', () => {
    expect(actionLabel('deployment.requested')).toBe('Deployment requested');
    expect(actionLabel('pipeline.triggered')).toBe('Pipeline triggered');
  });

  it('expands underscores on both halves', () => {
    expect(actionLabel('api_token.issued')).toBe('Api token issued');
    expect(actionLabel('deployment.rolled_back')).toBe('Deployment rolled back');
  });

  it('does not leave trailing whitespace when there is no verb', () => {
    expect(actionLabel('deployment')).toBe('Deployment');
  });
});
