-- +goose Up
-- +goose StatementBegin

-- Pipeline definitions, runs, jobs and attempts: the execution core.

CREATE TABLE pipeline_definitions (
    id          text PRIMARY KEY,
    project_id  text        NOT NULL REFERENCES projects (id) ON DELETE CASCADE,
    key         text        NOT NULL,
    name        text        NOT NULL,
    -- The step graph. JSONB is the right shape here and nowhere else in this schema:
    -- a definition is read as a whole, versioned as a whole, and its internal structure
    -- (step keys, dependency edges, retry policies) is validated in Go before it is
    -- ever written. Normalising it into step/edge tables would buy referential
    -- integrity over data that is already immutable once stored.
    spec        jsonb       NOT NULL,
    version     integer     NOT NULL DEFAULT 1,
    created_at  timestamptz NOT NULL DEFAULT now(),
    updated_at  timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT pipeline_definitions_spec_object CHECK (jsonb_typeof(spec) = 'object')
);
CREATE UNIQUE INDEX pipeline_definitions_project_key_version
    ON pipeline_definitions (project_id, key, version);

CREATE TABLE pipeline_runs (
    id             text PRIMARY KEY,
    project_id     text        NOT NULL REFERENCES projects (id) ON DELETE CASCADE,
    definition_id  text        NOT NULL REFERENCES pipeline_definitions (id),
    environment_id text        REFERENCES environments (id) ON DELETE SET NULL,
    -- Monotonic per project. This is the "#184" a human refers to; the ULID id is what
    -- machines use. Assigned in the same transaction that inserts the run.
    number         bigint      NOT NULL,
    state          text        NOT NULL DEFAULT 'PENDING',
    trigger        text        NOT NULL,
    triggered_by   text        REFERENCES users (id) ON DELETE SET NULL,
    commit_sha     text        NOT NULL,
    commit_message text        NOT NULL DEFAULT '',
    commit_author  text        NOT NULL DEFAULT '',
    branch         text        NOT NULL,
    pull_request_number integer,
    -- Set when the run is created from a webhook, so a redelivery can be traced back.
    webhook_event_id text,
    queued_at      timestamptz,
    started_at     timestamptz,
    finished_at    timestamptz,
    created_at     timestamptz NOT NULL DEFAULT now(),
    -- Guards against two API replicas writing conflicting state for the same run.
    version        integer     NOT NULL DEFAULT 1,
    CONSTRAINT pipeline_runs_state_valid CHECK (
        state IN ('PENDING', 'QUEUED', 'RUNNING', 'SUCCEEDED', 'FAILED', 'CANCELLED', 'TIMED_OUT')
    ),
    CONSTRAINT pipeline_runs_trigger_valid CHECK (
        trigger IN ('PUSH', 'PULL_REQUEST', 'MANUAL', 'PROMOTION', 'ROLLBACK', 'SCHEDULE')
    ),
    -- A finished run must have started. Cheap to state, and it catches the class of bug
    -- where a cancellation path forgets to stamp started_at.
    CONSTRAINT pipeline_runs_finish_implies_start
        CHECK (finished_at IS NULL OR started_at IS NOT NULL)
);
CREATE UNIQUE INDEX pipeline_runs_project_number_key ON pipeline_runs (project_id, number);
-- The console's default view: most recent runs for a project.
CREATE INDEX pipeline_runs_project_created_idx ON pipeline_runs (project_id, id DESC);
CREATE INDEX pipeline_runs_state_idx ON pipeline_runs (state)
    WHERE state IN ('PENDING', 'QUEUED', 'RUNNING');
CREATE INDEX pipeline_runs_commit_idx ON pipeline_runs (project_id, commit_sha);

-- Per-project run numbering. A sequence per project would mean DDL at project creation;
-- a counter row means one UPDATE ... RETURNING inside the run-creation transaction.
CREATE TABLE run_counters (
    project_id text PRIMARY KEY REFERENCES projects (id) ON DELETE CASCADE,
    next_number bigint NOT NULL DEFAULT 1
);

CREATE TABLE jobs (
    id            text PRIMARY KEY,
    run_id        text        NOT NULL REFERENCES pipeline_runs (id) ON DELETE CASCADE,
    project_id    text        NOT NULL REFERENCES projects (id) ON DELETE CASCADE,
    step_key      text        NOT NULL,
    name          text        NOT NULL,
    type          text        NOT NULL,
    state         text        NOT NULL DEFAULT 'BLOCKED',
    -- Dependency edges, denormalised onto the job so the scheduler can evaluate
    -- readiness without re-reading and re-parsing the pipeline spec on every tick.
    depends_on    text[]      NOT NULL DEFAULT '{}',
    wave          integer     NOT NULL DEFAULT 0,

    attempt         integer   NOT NULL DEFAULT 0,
    max_attempts    integer   NOT NULL DEFAULT 1,
    -- When the job becomes eligible to be claimed. Backoff is expressed by pushing this
    -- into the future rather than by sleeping anywhere.
    run_at          timestamptz NOT NULL DEFAULT now(),
    timeout_seconds integer   NOT NULL DEFAULT 600,
    continue_on_error boolean NOT NULL DEFAULT false,
    priority        integer   NOT NULL DEFAULT 0,

    -- Lease. A worker owns a job only until lease_expires_at; see the reaping query in
    -- internal/store/postgres/jobs.go.
    worker_id        text,
    lease_expires_at timestamptz,
    heartbeat_at     timestamptz,

    spec        jsonb       NOT NULL DEFAULT '{}'::jsonb,
    error_code  text,
    error_message text,
    started_at  timestamptz,
    finished_at timestamptz,
    created_at  timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT jobs_state_valid CHECK (
        state IN ('BLOCKED', 'QUEUED', 'RUNNING', 'SUCCEEDED', 'FAILED', 'CANCELLED', 'TIMED_OUT', 'SKIPPED')
    ),
    CONSTRAINT jobs_attempts_sane CHECK (max_attempts >= 1 AND attempt >= 0 AND attempt <= max_attempts),
    -- A RUNNING job must be owned by a worker and must hold a lease. This is the
    -- invariant the whole execution plane depends on, so the database enforces it
    -- rather than trusting every code path that writes this row.
    CONSTRAINT jobs_running_has_lease CHECK (
        state <> 'RUNNING' OR (worker_id IS NOT NULL AND lease_expires_at IS NOT NULL)
    )
);
CREATE UNIQUE INDEX jobs_run_step_key ON jobs (run_id, step_key);
CREATE INDEX jobs_run_idx ON jobs (run_id);

-- The claim index. This is the single most performance-critical index in NEXUS: the
-- dispatch query orders by (priority DESC, run_at) over QUEUED rows only, and every
-- worker runs it in a loop. Making it partial keeps it small — it holds only the
-- claimable backlog, not the millions of finished jobs beside them.
CREATE INDEX jobs_claimable_idx ON jobs (priority DESC, run_at, id)
    WHERE state = 'QUEUED';

-- The reaping index: expired leases on running jobs.
CREATE INDEX jobs_lease_expiry_idx ON jobs (lease_expires_at)
    WHERE state = 'RUNNING';

-- Unblocking: when a job finishes, find the jobs in the same run that named it.
CREATE INDEX jobs_blocked_idx ON jobs (run_id) WHERE state = 'BLOCKED';

CREATE TABLE job_attempts (
    id           text PRIMARY KEY,
    job_id       text        NOT NULL REFERENCES jobs (id) ON DELETE CASCADE,
    run_id       text        NOT NULL REFERENCES pipeline_runs (id) ON DELETE CASCADE,
    attempt      integer     NOT NULL,
    state        text        NOT NULL,
    worker_id    text,
    exit_code    integer,
    error_code   text,
    error_message text,
    -- Why this attempt happened at all: the first is 'INITIAL', later ones name the
    -- failure that caused the retry. This is what the console shows in attempt history.
    retry_reason text,
    started_at   timestamptz NOT NULL DEFAULT now(),
    finished_at  timestamptz,
    CONSTRAINT job_attempts_state_valid CHECK (
        state IN ('RUNNING', 'SUCCEEDED', 'FAILED', 'CANCELLED', 'TIMED_OUT')
    )
);
CREATE UNIQUE INDEX job_attempts_job_attempt_key ON job_attempts (job_id, attempt);
CREATE INDEX job_attempts_run_idx ON job_attempts (run_id);

CREATE TABLE job_logs (
    id         bigserial PRIMARY KEY,
    job_id     text        NOT NULL REFERENCES jobs (id) ON DELETE CASCADE,
    attempt    integer     NOT NULL,
    stream     text        NOT NULL DEFAULT 'stdout',
    -- Sequence within the attempt. Wall-clock timestamps are not sufficient to order
    -- lines that a process emitted inside the same millisecond.
    seq        integer     NOT NULL,
    message    text        NOT NULL,
    logged_at  timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT job_logs_stream_valid CHECK (stream IN ('stdout', 'stderr', 'system'))
);
CREATE UNIQUE INDEX job_logs_job_attempt_seq_key ON job_logs (job_id, attempt, seq);

CREATE TABLE workers (
    id            text PRIMARY KEY,
    hostname      text        NOT NULL,
    version       text        NOT NULL DEFAULT '',
    concurrency   integer     NOT NULL DEFAULT 1,
    active_jobs   integer     NOT NULL DEFAULT 0,
    state         text        NOT NULL DEFAULT 'ACTIVE',
    started_at    timestamptz NOT NULL DEFAULT now(),
    last_heartbeat_at timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT workers_state_valid CHECK (state IN ('ACTIVE', 'DRAINING', 'STOPPED', 'LOST'))
);
-- Worker liveness is derived from last_heartbeat_at rather than stored, so a worker
-- that dies without deregistering is still correctly reported as lost.
CREATE INDEX workers_heartbeat_idx ON workers (last_heartbeat_at) WHERE state = 'ACTIVE';

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP TABLE IF EXISTS workers;
DROP TABLE IF EXISTS job_logs;
DROP TABLE IF EXISTS job_attempts;
DROP TABLE IF EXISTS jobs;
DROP TABLE IF EXISTS run_counters;
DROP TABLE IF EXISTS pipeline_runs;
DROP TABLE IF EXISTS pipeline_definitions;
-- +goose StatementEnd
