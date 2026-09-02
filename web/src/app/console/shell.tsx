'use client';

import Link from 'next/link';
import { usePathname, useRouter } from 'next/navigation';
import { useCallback, useEffect, useState, type ReactNode } from 'react';

import { api, clearToken, getToken, openStream, type OverviewMetrics, type User } from '@/lib/api';
import styles from './layout.module.css';

const NAV = [
  {
    group: 'Delivery',
    items: [
      { href: '/console', label: 'Overview', exact: true },
      { href: '/console/projects', label: 'Projects' },
      { href: '/console/pipelines', label: 'Pipelines' },
      { href: '/console/deployments', label: 'Deployments' },
    ],
  },
  {
    group: 'Platform',
    items: [
      { href: '/console/environments', label: 'Environments' },
      { href: '/console/infrastructure', label: 'Infrastructure' },
      { href: '/console/observability', label: 'Observability' },
    ],
  },
  {
    group: 'Governance',
    items: [{ href: '/console/audit', label: 'Audit' }],
  },
];

export function Shell({ children }: { children: ReactNode }): ReactNode {
  const pathname = usePathname();
  const router = useRouter();
  const [user, setUser] = useState<User | null>(null);
  const [metrics, setMetrics] = useState<OverviewMetrics | null>(null);
  const [connected, setConnected] = useState(false);

  useEffect(() => {
    if (!getToken()) {
      router.replace('/login');
      return;
    }
    api
      .get<{ user: User }>('/api/v1/auth/me')
      .then((res) => setUser(res.user))
      .catch(() => router.replace('/login'));
  }, [router]);

  const refreshRail = useCallback(() => {
    api
      .get<{ metrics: OverviewMetrics }>('/api/v1/overview')
      .then((res) => setMetrics(res.metrics))
      .catch(() => {
        /* the rail going stale must not take the page down */
      });
  }, []);

  useEffect(() => {
    refreshRail();
    // The rail is also refreshed by the event stream. This interval is the floor, so
    // the readings stay honest when nothing is emitting events — an idle queue depth of
    // zero and a stale queue depth of zero look identical otherwise.
    const timer = window.setInterval(refreshRail, 15_000);
    return () => window.clearInterval(timer);
  }, [refreshRail]);

  useEffect(() => {
    const close = openStream(() => {
      setConnected(true);
      refreshRail();
    });
    // EventSource does not report a successful open through our handler until the first
    // event arrives, so optimistically mark connected and let a failed fetch correct it.
    setConnected(true);
    return () => {
      close();
      setConnected(false);
    };
  }, [refreshRail]);

  const signOut = () => {
    clearToken();
    router.replace('/login');
  };

  const crumbs = breadcrumbFor(pathname);

  return (
    <div className={styles.shell}>
      <div className={styles.brand}>
        <Mark />
        <span className={styles.brandText}>
          <span className={styles.brandMark}>NEXUS</span>
          <span className={styles.brandVersion}>control plane</span>
        </span>
      </div>

      <header className={styles.header}>
        <div className={styles.breadcrumb}>
          <span className={styles.breadcrumbRoot}>NEXUS</span>
          {crumbs.map((c) => (
            <span key={c} style={{ display: 'contents' }}>
              <span className={styles.breadcrumbSep}>/</span>
              <span>{c}</span>
            </span>
          ))}
        </div>
        <div className={styles.headerRight}>
          {user && (
            <span className={styles.identity}>
              <span className={styles.identityName}>{user.name}</span>
              <span className={styles.identityRole}>{user.role}</span>
            </span>
          )}
          <button type="button" className={styles.signOut} onClick={signOut}>
            Sign out
          </button>
        </div>
      </header>

      <div className={styles.rail}>
        <Reading label="Queue" value={metrics?.queue_depth ?? '—'} strong={(metrics?.queue_depth ?? 0) > 0} />
        <Reading label="Running" value={metrics?.jobs_running ?? '—'} />
        <Reading
          label="Workers"
          value={metrics?.workers_live ?? '—'}
          variant={metrics && metrics.workers_live === 0 ? 'warn' : undefined}
        />
        <Reading label="Healthy" value={metrics?.healthy_environments ?? '—'} variant="healthy" />
        <Reading
          label="Degraded"
          value={metrics?.degraded_deployments ?? '—'}
          variant={metrics && metrics.degraded_deployments > 0 ? 'failed' : undefined}
        />
        <Reading
          label="Drift"
          value={metrics?.open_drift ?? '—'}
          variant={metrics && metrics.open_drift > 0 ? 'drift' : undefined}
        />
        <Reading
          label="Outbox"
          value={metrics?.outbox_pending ?? '—'}
          variant={metrics && metrics.outbox_pending > 200 ? 'warn' : undefined}
        />
        <span className={`${styles.railItem} ${styles.railSpacer}`} />
        <span className={styles.railItem}>
          <span className={styles.railLabel}>Stream</span>
          <span className={`${styles.railValue} ${connected ? styles.railValueHealthy : styles.railValueWarn}`}>
            {connected ? 'live' : 'offline'}
          </span>
        </span>
      </div>

      <nav className={styles.nav}>
        {NAV.map((group) => (
          <div key={group.group} className={styles.navGroup}>
            <div className={styles.navGroupLabel}>{group.group}</div>
            {group.items.map((item) => {
              const active = item.exact ? pathname === item.href : pathname.startsWith(item.href);
              return (
                <Link
                  key={item.href}
                  href={item.href}
                  className={`${styles.navLink} ${active ? styles.navLinkActive : ''}`}
                >
                  <span className={styles.navLabel}>{item.label}</span>
                  {badgeFor(item.label, metrics)}
                </Link>
              );
            })}
          </div>
        ))}
        <div className={styles.navFooter}>
          <span>Build dev</span>
        </div>
      </nav>

      <main className={styles.content}>{children}</main>
    </div>
  );
}

/** The mark: six nodes, four lit. Drawn in CSS so it scales with the type around it. */
function Mark(): ReactNode {
  const lit = [0, 1, 3, 5];
  return (
    <span className={styles.mark} aria-hidden="true">
      {Array.from({ length: 6 }, (_, i) => (
        <span key={i} className={`${styles.markDot} ${lit.includes(i) ? styles.markDotLit : ''}`} />
      ))}
    </span>
  );
}

function Reading({
  label,
  value,
  strong,
  variant,
}: {
  label: string;
  value: ReactNode;
  strong?: boolean;
  variant?: 'healthy' | 'warn' | 'failed' | 'drift';
}): ReactNode {
  const variantClass =
    variant === 'healthy'
      ? styles.railValueHealthy
      : variant === 'warn'
        ? styles.railValueWarn
        : variant === 'failed'
          ? styles.railValueFailed
          : variant === 'drift'
            ? styles.railValueDrift
            : '';
  return (
    <span className={styles.railItem}>
      <span className={styles.railLabel}>{label}</span>
      <span
        className={`${styles.railValue} ${strong ? styles.railValueStrong : ''} ${variantClass}`}
      >
        {value}
      </span>
    </span>
  );
}

/** Only conditions that need a person get a badge. A count of zero shows nothing. */
function badgeFor(label: string, metrics: OverviewMetrics | null): ReactNode {
  if (!metrics) return null;
  if (label === 'Pipelines' && metrics.runs_failed_last_24h > 0) {
    return <span className={`${styles.navBadge} ${styles.navBadgeAlert}`}>{metrics.runs_failed_last_24h}</span>;
  }
  if (label === 'Environments' && metrics.open_drift > 0) {
    return <span className={`${styles.navBadge} ${styles.navBadgeDrift}`}>{metrics.open_drift}</span>;
  }
  if (label === 'Deployments' && metrics.degraded_deployments > 0) {
    return <span className={`${styles.navBadge} ${styles.navBadgeAlert}`}>{metrics.degraded_deployments}</span>;
  }
  return null;
}

function breadcrumbFor(pathname: string): string[] {
  const parts = pathname.split('/').filter(Boolean);
  if (parts.length <= 1) return ['Overview'];
  // Identifiers are not breadcrumb material — a prefixed ULID tells a reader nothing.
  return parts
    .slice(1)
    .filter((p) => !/^[a-z]+_[0-9a-z]{26}$/.test(p))
    .map((p) => p.charAt(0).toUpperCase() + p.slice(1));
}
