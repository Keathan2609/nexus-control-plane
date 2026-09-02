import type { CSSProperties, ReactNode } from 'react';
import { stateLabel, tone, type Tone } from '@/lib/format';
import styles from './ui.module.css';

/** Presentational primitives. No data fetching, no state. */

const TONE_CLASS: Record<Tone, string> = {
  healthy: styles.toneHealthy!,
  running: styles.toneRunning!,
  warn: styles.toneWarn!,
  failed: styles.toneFailed!,
  drift: styles.toneDrift!,
  idle: styles.toneIdle!,
};

const VALUE_TONE: Record<Tone, string> = {
  healthy: styles.valueHealthy!,
  running: '',
  warn: styles.valueWarn!,
  failed: styles.valueFailed!,
  drift: styles.valueDrift!,
  idle: '',
};

/** States that mean "something is happening right now" animate their mark. */
const ACTIVE_STATES = new Set([
  'RUNNING',
  'QUEUED',
  'DEPLOYING',
  'VERIFYING',
  'PROVISIONING',
  'REQUESTED',
  'ROLLING_BACK',
]);

export function StateBadge({
  state,
  label,
}: {
  state: string | undefined | null;
  label?: string;
}): ReactNode {
  const t = tone(state);
  const active = ACTIVE_STATES.has((state ?? '').toUpperCase());
  return (
    <span className={`${styles.badge} ${TONE_CLASS[t]}`}>
      <span className={`${styles.badgeMark} ${active ? styles.badgePulse : ''}`} />
      {label ?? stateLabel(state)}
    </span>
  );
}

export function PageHeader({
  title,
  count,
  subtitle,
  actions,
}: {
  title: string;
  count?: ReactNode;
  subtitle?: string;
  actions?: ReactNode;
}): ReactNode {
  return (
    <header className={styles.pageHeader}>
      <div>
        <h1 className={styles.pageTitle}>{title}</h1>
        {count !== undefined && <div className={styles.pageCount}>{count}</div>}
        {subtitle !== undefined && <p className={styles.pageSubtitle}>{subtitle}</p>}
      </div>
      {actions !== undefined && <div className={styles.pageActions}>{actions}</div>}
    </header>
  );
}

export function Section({
  label,
  meta,
  children,
}: {
  label: string;
  meta?: ReactNode;
  children: ReactNode;
}): ReactNode {
  return (
    <section className={styles.section}>
      <header className={styles.sectionHead}>
        <span className={styles.sectionLabel}>{label}</span>
        {meta !== undefined && <span className={styles.sectionMeta}>{meta}</span>}
      </header>
      {children}
    </section>
  );
}

export function Readout({ children }: { children: ReactNode }): ReactNode {
  return <div className={styles.readout}>{children}</div>;
}

export function Reading({
  label,
  value,
  of,
  meta,
  state,
}: {
  label: string;
  value: ReactNode;
  /** The denominator, set smaller so the numerator reads as the measurement. */
  of?: ReactNode;
  meta?: ReactNode;
  state?: string;
}): ReactNode {
  const valueClass = state ? VALUE_TONE[tone(state)] : '';
  return (
    <div className={styles.readoutItem}>
      <span className={styles.readoutLabel}>{label}</span>
      <span className={`${styles.readoutValue} ${valueClass}`}>
        {value}
        {of !== undefined && <span className={styles.readoutOf}>/{of}</span>}
      </span>
      {meta !== undefined && <span className={styles.readoutMeta}>{meta}</span>}
    </div>
  );
}

/**
 * A ledger. `columns` is a grid-template-columns value shared by the head and every
 * row, which is what keeps the columns genuinely aligned.
 */
export function Ledger({
  columns,
  head,
  children,
}: {
  columns: string;
  head: ReactNode[];
  children: ReactNode;
}): ReactNode {
  const style = { gridTemplateColumns: columns } as CSSProperties;
  return (
    <div className={styles.ledger}>
      <div className={styles.ledgerHead} style={style}>
        {head.map((h, i) => (
          <span key={i} className={i >= head.length - 1 ? styles.right : undefined}>
            {h}
          </span>
        ))}
      </div>
      {children}
    </div>
  );
}

export function LedgerRow({
  columns,
  active,
  children,
  onClick,
  as = 'div',
}: {
  columns: string;
  active?: boolean;
  children: ReactNode;
  onClick?: () => void;
  as?: 'div' | 'button';
}): ReactNode {
  const style = { gridTemplateColumns: columns } as CSSProperties;
  const className = `${styles.ledgerRow} ${active ? styles.ledgerRowActive : ''}`;

  if (as === 'button' || onClick) {
    return (
      <button type="button" className={className} style={style} onClick={onClick}>
        {children}
      </button>
    );
  }
  return (
    <div className={className} style={style}>
      {children}
    </div>
  );
}

export function KeyValue({ entries }: { entries: Array<[string, ReactNode]> }): ReactNode {
  return (
    <dl className={styles.kv}>
      {entries.map(([key, value]) => (
        <div key={key} style={{ display: 'contents' }}>
          <dt className={styles.kvKey}>{key}</dt>
          <dd className={styles.kvValue}>{value}</dd>
        </div>
      ))}
    </dl>
  );
}

export function Button({
  children,
  variant = 'default',
  onClick,
  disabled,
  type = 'button',
  title,
}: {
  children: ReactNode;
  variant?: 'default' | 'primary' | 'danger';
  onClick?: () => void;
  disabled?: boolean;
  type?: 'button' | 'submit';
  title?: string;
}): ReactNode {
  const variantClass =
    variant === 'primary'
      ? styles.buttonPrimary
      : variant === 'danger'
        ? styles.buttonDanger
        : '';
  return (
    <button
      type={type}
      className={`${styles.button} ${variantClass}`}
      onClick={onClick}
      disabled={disabled}
      title={title}
    >
      {children}
    </button>
  );
}

export function Notice({
  children,
  variant = 'default',
}: {
  children: ReactNode;
  variant?: 'default' | 'error' | 'warn';
}): ReactNode {
  const variantClass =
    variant === 'error' ? styles.noticeError : variant === 'warn' ? styles.noticeWarn : '';
  return <div className={`${styles.notice} ${variantClass}`}>{children}</div>;
}

export function Empty({ title, hint }: { title: string; hint?: string }): ReactNode {
  return (
    <div className={styles.empty}>
      <div className={styles.emptyTitle}>{title}</div>
      {hint !== undefined && <div>{hint}</div>}
    </div>
  );
}

export function CodeBlock({ children }: { children: string }): ReactNode {
  return <pre className={styles.code}>{children}</pre>;
}

export function Spinner(): ReactNode {
  return <span className={styles.spinner} aria-label="loading" />;
}

export function Live({ connected }: { connected: boolean }): ReactNode {
  return (
    <span className={styles.live}>
      <span className={`${styles.liveDot} ${connected ? '' : styles.liveDotOff}`} />
      {connected ? 'Live' : 'Reconnecting'}
    </span>
  );
}

/** Placeholder rows sized to the content they replace, so the page does not jump. */
export function LoadingRows({ count = 5, columns = 4 }: { count?: number; columns?: number }): ReactNode {
  return (
    <>
      {Array.from({ length: count }, (_, i) => (
        <div key={i} className={styles.skeletonRow}>
          {Array.from({ length: columns }, (_, c) => (
            <span
              key={c}
              className={styles.skeleton}
              style={{ maxWidth: c === 0 ? '18%' : c === columns - 1 ? '8%' : '24%' }}
            />
          ))}
        </div>
      ))}
    </>
  );
}

export { styles as ui };
