'use client';

import { useRouter } from 'next/navigation';
import { useState, type FormEvent, type ReactNode } from 'react';

import { api, ApiError, setToken, type User } from '@/lib/api';
import styles from './page.module.css';

const DEMO_ACCOUNTS = [
  { email: 'admin@nexus.local', role: 'Admin' },
  { email: 'dev@nexus.local', role: 'Developer' },
  { email: 'viewer@nexus.local', role: 'Viewer' },
];

const DEMO_PASSWORD = 'nexus-demo-password';

export default function LoginPage(): ReactNode {
  const router = useRouter();
  const [email, setEmail] = useState('admin@nexus.local');
  const [password, setPassword] = useState(DEMO_PASSWORD);
  const [error, setError] = useState<string | null>(null);
  const [busy, setBusy] = useState(false);

  async function submit(event: FormEvent) {
    event.preventDefault();
    setBusy(true);
    setError(null);
    try {
      const res = await api.post<{ token: string; user: User }>('/api/v1/auth/login', {
        email,
        password,
      });
      setToken(res.token);
      router.replace('/console');
    } catch (err) {
      setError(
        err instanceof ApiError && err.code === 'unreachable'
          ? 'Cannot reach the control plane. Is nexus-api running on port 8080?'
          : 'Invalid email or password.',
      );
      setBusy(false);
    }
  }

  return (
    <div className={styles.page}>
      <div className={styles.panel}>
        <div className={styles.brand}>
          <span className={styles.mark} aria-hidden="true">
            {Array.from({ length: 6 }, (_, i) => (
              <span
                key={i}
                className={`${styles.markDot} ${[0, 1, 3, 5].includes(i) ? styles.markDotLit : ''}`}
              />
            ))}
          </span>
          <span className={styles.wordmark}>NEXUS</span>
        </div>
        <p className={styles.tagline}>
          Delivery control plane. Source to running deployment, with the state machine,
          the evidence and the audit trail behind it.
        </p>

        <form className={styles.form} onSubmit={submit}>
          {error && <div className={styles.error}>{error}</div>}

          <div className={styles.field}>
            <label className={styles.label} htmlFor="email">
              Email
            </label>
            <input
              id="email"
              className={styles.input}
              type="email"
              value={email}
              autoComplete="username"
              onChange={(e) => setEmail(e.target.value)}
              required
            />
          </div>

          <div className={styles.field}>
            <label className={styles.label} htmlFor="password">
              Password
            </label>
            <input
              id="password"
              className={styles.input}
              type="password"
              value={password}
              autoComplete="current-password"
              onChange={(e) => setPassword(e.target.value)}
              required
            />
          </div>

          <button className={styles.submit} type="submit" disabled={busy}>
            {busy ? 'Signing in' : 'Sign in'}
          </button>
        </form>

        <div className={styles.hint}>
          <div className={styles.hintLabel}>Demo accounts</div>
          {DEMO_ACCOUNTS.map((account) => (
            <button
              key={account.email}
              type="button"
              className={styles.account}
              onClick={() => {
                setEmail(account.email);
                setPassword(DEMO_PASSWORD);
              }}
            >
              <span>{account.email}</span>
              <span className={styles.accountRole}>{account.role}</span>
            </button>
          ))}
        </div>
      </div>

      <div className={styles.field2}>
        <div className={styles.fieldNote}>
          git push → webhook → pipeline → build → scan → image → deploy → verify → promote
          <br />
          health verification failed → rollback → previous release restored → recovery verified
        </div>
      </div>
    </div>
  );
}
