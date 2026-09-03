/**
 * The console's only route to data.
 *
 * Every screen reads through this client, and this client talks to the NEXUS API and
 * nothing else. There is no Next.js route handler proxying requests and no server-side
 * data layer holding its own copy of anything: the control plane is the single source of
 * truth, and authorisation is enforced in exactly one place.
 */

const API_URL = process.env.NEXT_PUBLIC_API_URL ?? 'http://localhost:8080';

const TOKEN_KEY = 'nexus.token';

export function getToken(): string | null {
  if (typeof window === 'undefined') return null;
  try {
    return window.localStorage.getItem(TOKEN_KEY);
  } catch {
    // Private browsing and hardened settings both throw on access rather than
    // returning null, and a console that crashes on load in those cases is worse
    // than one that simply asks the user to sign in again.
    return null;
  }
}

export function setToken(token: string): void {
  try {
    window.localStorage.setItem(TOKEN_KEY, token);
  } catch {
    /* the session still works for this tab; it just will not survive a reload */
  }
}

export function clearToken(): void {
  try {
    window.localStorage.removeItem(TOKEN_KEY);
  } catch {
    /* nothing to do */
  }
}

/** The single error shape the API returns. */
export interface ApiErrorBody {
  code: string;
  message: string;
  fields?: Array<{ field: string; message: string }>;
  request_id?: string;
}

export class ApiError extends Error {
  readonly status: number;
  readonly code: string;
  readonly fields: Array<{ field: string; message: string }>;
  readonly requestId?: string;

  constructor(status: number, body: ApiErrorBody) {
    super(body.message);
    this.name = 'ApiError';
    this.status = status;
    this.code = body.code;
    this.fields = body.fields ?? [];
    this.requestId = body.request_id;
  }
}

async function request<T>(path: string, init: RequestInit = {}): Promise<T> {
  const token = getToken();
  const headers = new Headers(init.headers);
  headers.set('Accept', 'application/json');
  if (init.body) headers.set('Content-Type', 'application/json');
  if (token) headers.set('Authorization', `Bearer ${token}`);

  let response: Response;
  try {
    response = await fetch(`${API_URL}${path}`, { ...init, headers });
  } catch {
    // A network failure is not an API error and must not be reported as one — the
    // difference matters when the control plane is down versus rejecting a request.
    throw new ApiError(0, {
      code: 'unreachable',
      message: 'Cannot reach the control plane.',
    });
  }

  if (response.status === 401) {
    clearToken();
    if (typeof window !== 'undefined' && !window.location.pathname.startsWith('/login')) {
      window.location.href = '/login';
    }
    throw new ApiError(401, { code: 'unauthorised', message: 'Session expired.' });
  }

  if (response.status === 204) return undefined as T;

  const text = await response.text();
  const parsed: unknown = text ? JSON.parse(text) : {};

  if (!response.ok) {
    const body = (parsed as { error?: ApiErrorBody }).error ?? {
      code: 'unknown',
      message: `Request failed with status ${response.status}`,
    };
    throw new ApiError(response.status, body);
  }
  return parsed as T;
}

export const api = {
  get: <T>(path: string) => request<T>(path),
  post: <T>(path: string, body?: unknown) =>
    request<T>(path, { method: 'POST', body: body ? JSON.stringify(body) : undefined }),
};

/** Opens the control plane event stream. */
export function openStream(onEvent: (event: StreamEvent) => void, projectId?: string): () => void {
  const token = getToken();
  if (!token) return () => {};

  // EventSource cannot set an Authorization header, so the token travels as a query
  // parameter on this one endpoint. It is a short-lived JWT over a same-origin request
  // in development and behind TLS in deployment; the alternative is a second auth
  // scheme for one endpoint.
  const params = new URLSearchParams({ token });
  if (projectId) params.set('project_id', projectId);

  const source = new EventSource(`${API_URL}/api/v1/stream?${params.toString()}`);
  source.addEventListener('nexus', (event) => {
    try {
      onEvent(JSON.parse((event as MessageEvent<string>).data) as StreamEvent);
    } catch {
      /* a malformed frame is dropped rather than breaking the stream */
    }
  });
  return () => source.close();
}

export interface StreamEvent {
  id: string;
  type: string;
  aggregate_type: string;
  aggregate_id: string;
  correlation_id: string;
  occurred_at: string;
  payload: Record<string, unknown>;
}

/* --- resource types, mirroring the API's JSON ------------------------------- */

export type Role = 'ADMIN' | 'DEVELOPER' | 'VIEWER';

export interface User {
  id: string;
  email: string;
  name: string;
  role: Role;
  organisation_id: string;
}

export interface Page<T> {
  data: T[];
  next_cursor?: string;
  has_more: boolean;
}

export interface Project {
  id: string;
  name: string;
  slug: string;
  description: string;
  runtime: string;
  repository?: string;
  default_branch?: string;
  created_at: string;
  environment_count: number;
  last_run_number?: number;
  last_run_state?: string;
  last_run_at?: string;
  production_revision?: string;
  production_health?: string;
  success_rate: number;
}

export interface Run {
  id: string;
  project_id: string;
  project_name?: string;
  number: number;
  state: string;
  trigger: string;
  commit_sha: string;
  commit_message: string;
  commit_author: string;
  branch: string;
  pull_request?: number;
  created_at: string;
  started_at?: string;
  finished_at?: string;
  duration_ms?: number;
  jobs_total: number;
  jobs_done: number;
}

export interface Attempt {
  attempt: number;
  state: string;
  worker_id?: string;
  retry_reason?: string;
  error_code?: string;
  error_message?: string;
  started_at: string;
  finished_at?: string;
}

export interface Job {
  id: string;
  run_id: string;
  step_key: string;
  name: string;
  type: string;
  state: string;
  depends_on: string[];
  wave: number;
  attempt: number;
  max_attempts: number;
  worker_id?: string;
  error_code?: string;
  error_message?: string;
  started_at?: string;
  finished_at?: string;
  duration_ms?: number;
  attempts?: Attempt[];
}

export interface LogLine {
  seq: number;
  attempt: number;
  stream: string;
  message: string;
  logged_at: string;
}

export interface Environment {
  id: string;
  project_id: string;
  project_name?: string;
  name: string;
  kind: string;
  namespace: string;
  url?: string;
  state: string;
  pull_request?: number;
  source_branch?: string;
  expires_at?: string;
  created_at: string;
  revision?: string;
  actual_revision?: string;
  health_status?: string;
  sync_status?: string;
  deployed_at?: string;
  open_drift: number;
}

export interface Deployment {
  id: string;
  project_id: string;
  environment_id: string;
  environment_name: string;
  number: number;
  state: string;
  release_version: string;
  commit_sha: string;
  image: string;
  image_digest?: string;
  image_size_bytes?: number;
  desired_revision: string;
  actual_revision?: string;
  sync_status: string;
  health_status: string;
  requested_by?: string;
  reason: string;
  is_rollback: boolean;
  requested_at: string;
  started_at?: string;
  verified_at?: string;
  finished_at?: string;
  duration_ms?: number;
}

export interface HealthCheck {
  attempt: number;
  status: string;
  http_status?: number;
  latency_ms?: number;
  message: string;
  checked_at: string;
}

export interface Worker {
  id: string;
  hostname: string;
  concurrency: number;
  active_jobs: number;
  state: string;
  started_at: string;
  last_heartbeat_at: string;
  live: boolean;
}

export interface AuditEntry {
  id: string;
  actor_id?: string;
  actor_type: string;
  actor_label: string;
  action: string;
  resource_type: string;
  resource_id: string;
  project_id?: string;
  correlation_id?: string;
  metadata: Record<string, unknown>;
  occurred_at: string;
}

export interface OverviewMetrics {
  projects: number;
  environments: number;
  runs_last_24h: number;
  runs_running: number;
  runs_failed_last_24h: number;
  success_rate: number;
  median_run_seconds?: number;
  deployments_last_24h: number;
  median_deploy_seconds?: number;
  healthy_environments: number;
  degraded_deployments: number;
  queue_depth: number;
  jobs_running: number;
  workers_live: number;
  open_drift: number;
  open_incidents: number;
  outbox_pending: number;
}

export interface Overview {
  metrics: OverviewMetrics;
  /** Which applier is actually in use, reported by the control plane. */
  deploy_provider?: string;
  recent_runs: Run[];
  recent_deployments: Deployment[];
  workers: Worker[];
  environments: Environment[];
}
