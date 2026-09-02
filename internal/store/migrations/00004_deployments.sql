-- +goose Up
-- +goose StatementBegin

-- Artifacts, releases, deployments, health verification and GitOps state.

CREATE TABLE artifacts (
    id          text PRIMARY KEY,
    project_id  text        NOT NULL REFERENCES projects (id) ON DELETE CASCADE,
    run_id      text        REFERENCES pipeline_runs (id) ON DELETE SET NULL,
    kind        text        NOT NULL,
    -- For a container image: the fully qualified reference including the digest.
    reference   text        NOT NULL,
    digest      text        NOT NULL,
    size_bytes  bigint,
    created_at  timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT artifacts_kind_valid CHECK (kind IN ('CONTAINER_IMAGE', 'ARCHIVE', 'SBOM'))
);
-- The digest is the identity of the content. Two runs that build byte-identical images
-- produce the same digest, and that is a fact worth being able to look up.
CREATE INDEX artifacts_digest_idx ON artifacts (digest);
CREATE INDEX artifacts_project_idx ON artifacts (project_id, id DESC);

-- A release is an immutable, named bundle of "this commit, built into this artifact".
-- Deployments reference releases, which is what makes rollback expressible: rolling
-- back is deploying an earlier release, not rebuilding an earlier commit.
CREATE TABLE releases (
    id          text PRIMARY KEY,
    project_id  text        NOT NULL REFERENCES projects (id) ON DELETE CASCADE,
    version     text        NOT NULL,
    commit_sha  text        NOT NULL,
    artifact_id text        NOT NULL REFERENCES artifacts (id),
    run_id      text        REFERENCES pipeline_runs (id) ON DELETE SET NULL,
    created_at  timestamptz NOT NULL DEFAULT now()
);
CREATE UNIQUE INDEX releases_project_version_key ON releases (project_id, version);
CREATE INDEX releases_project_idx ON releases (project_id, id DESC);

CREATE TABLE deployments (
    id             text PRIMARY KEY,
    project_id     text        NOT NULL REFERENCES projects (id) ON DELETE CASCADE,
    environment_id text        NOT NULL REFERENCES environments (id) ON DELETE CASCADE,
    release_id     text        NOT NULL REFERENCES releases (id),
    run_id         text        REFERENCES pipeline_runs (id) ON DELETE SET NULL,
    number         bigint      NOT NULL,
    state          text        NOT NULL DEFAULT 'REQUESTED',
    strategy       text        NOT NULL DEFAULT 'ROLLING',

    -- The revision NEXUS wants running, and the revision it has actually observed.
    -- Holding both is what makes drift a first-class, queryable fact rather than
    -- something a human notices.
    desired_revision text      NOT NULL,
    actual_revision  text,
    sync_status      text      NOT NULL DEFAULT 'OUT_OF_SYNC',
    health_status    text      NOT NULL DEFAULT 'UNKNOWN',

    -- Rollback provenance. Both are null for an ordinary deployment.
    rollback_of      text      REFERENCES deployments (id) ON DELETE SET NULL,
    rolled_back_to   text      REFERENCES releases (id) ON DELETE SET NULL,
    requested_by     text      REFERENCES users (id) ON DELETE SET NULL,
    reason           text      NOT NULL DEFAULT '',

    requested_at   timestamptz NOT NULL DEFAULT now(),
    started_at     timestamptz,
    verified_at    timestamptz,
    finished_at    timestamptz,
    version        integer     NOT NULL DEFAULT 1,
    CONSTRAINT deployments_state_valid CHECK (
        state IN ('REQUESTED', 'PROVISIONING', 'DEPLOYING', 'VERIFYING', 'HEALTHY',
                  'DEGRADED', 'FAILED', 'ROLLING_BACK', 'ROLLED_BACK', 'SUPERSEDED')
    ),
    CONSTRAINT deployments_sync_valid CHECK (sync_status IN ('SYNCED', 'OUT_OF_SYNC', 'DRIFTED', 'UNKNOWN')),
    CONSTRAINT deployments_health_valid CHECK (health_status IN ('HEALTHY', 'DEGRADED', 'UNHEALTHY', 'UNKNOWN')),
    CONSTRAINT deployments_strategy_valid CHECK (strategy IN ('ROLLING', 'RECREATE', 'BLUE_GREEN')),
    -- A deployment cannot be verified before it started.
    CONSTRAINT deployments_verify_implies_start
        CHECK (verified_at IS NULL OR started_at IS NOT NULL)
);
CREATE UNIQUE INDEX deployments_project_number_key ON deployments (project_id, number);
CREATE INDEX deployments_environment_idx ON deployments (environment_id, id DESC);
CREATE INDEX deployments_project_idx ON deployments (project_id, id DESC);
-- At most one deployment may be live in an environment at a time. Expressed as a partial
-- unique index so the database, not application ordering, prevents two concurrent
-- promotions from both believing they own production.
CREATE UNIQUE INDEX deployments_one_live_per_environment
    ON deployments (environment_id)
    WHERE state IN ('HEALTHY', 'DEGRADED');
-- The reconciler scans in-flight deployments every cycle.
CREATE INDEX deployments_active_idx ON deployments (state)
    WHERE state IN ('REQUESTED', 'PROVISIONING', 'DEPLOYING', 'VERIFYING', 'ROLLING_BACK');

CREATE TABLE deployment_counters (
    project_id  text PRIMARY KEY REFERENCES projects (id) ON DELETE CASCADE,
    next_number bigint NOT NULL DEFAULT 1
);

-- Each probe of a deployment, retained. "It became healthy" is a claim; these rows are
-- the evidence for it, and they are what the console shows under health verification.
CREATE TABLE health_checks (
    id            text PRIMARY KEY,
    deployment_id text        NOT NULL REFERENCES deployments (id) ON DELETE CASCADE,
    target        text        NOT NULL,
    attempt       integer     NOT NULL,
    status        text        NOT NULL,
    http_status   integer,
    latency_ms    integer,
    message       text        NOT NULL DEFAULT '',
    checked_at    timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT health_checks_status_valid CHECK (status IN ('PASS', 'FAIL', 'TIMEOUT'))
);
CREATE INDEX health_checks_deployment_idx ON health_checks (deployment_id, checked_at);

-- The rendered desired state for an environment: what NEXUS wants Kubernetes to be
-- running. Argo CD (or the local applier) reconciles this; NEXUS owns producing it.
CREATE TABLE gitops_revisions (
    id             text PRIMARY KEY,
    project_id     text        NOT NULL REFERENCES projects (id) ON DELETE CASCADE,
    environment_id text        NOT NULL REFERENCES environments (id) ON DELETE CASCADE,
    deployment_id  text        REFERENCES deployments (id) ON DELETE SET NULL,
    revision       text        NOT NULL,
    -- Content hash of the rendered manifests. Two renders of the same desired state
    -- produce the same hash, which is how "no change" is distinguished from "changed".
    manifest_hash  text        NOT NULL,
    manifests      text        NOT NULL,
    created_at     timestamptz NOT NULL DEFAULT now()
);
CREATE UNIQUE INDEX gitops_revisions_env_revision_key ON gitops_revisions (environment_id, revision);
CREATE INDEX gitops_revisions_env_idx ON gitops_revisions (environment_id, id DESC);

-- Observed divergence between desired and actual state.
CREATE TABLE drift_records (
    id             text PRIMARY KEY,
    environment_id text        NOT NULL REFERENCES environments (id) ON DELETE CASCADE,
    deployment_id  text        REFERENCES deployments (id) ON DELETE SET NULL,
    desired_revision text      NOT NULL,
    actual_revision  text,
    detail         text        NOT NULL DEFAULT '',
    detected_at    timestamptz NOT NULL DEFAULT now(),
    resolved_at    timestamptz,
    resolution     text,
    CONSTRAINT drift_resolution_valid
        CHECK (resolution IS NULL OR resolution IN ('RECONCILED', 'ACCEPTED', 'SUPERSEDED'))
);
CREATE INDEX drift_records_open_idx ON drift_records (environment_id) WHERE resolved_at IS NULL;

CREATE TABLE incidents (
    id             text PRIMARY KEY,
    project_id     text        NOT NULL REFERENCES projects (id) ON DELETE CASCADE,
    environment_id text        REFERENCES environments (id) ON DELETE SET NULL,
    deployment_id  text        REFERENCES deployments (id) ON DELETE SET NULL,
    title          text        NOT NULL,
    severity       text        NOT NULL DEFAULT 'MINOR',
    state          text        NOT NULL DEFAULT 'OPEN',
    detail         text        NOT NULL DEFAULT '',
    opened_at      timestamptz NOT NULL DEFAULT now(),
    resolved_at    timestamptz,
    CONSTRAINT incidents_severity_valid CHECK (severity IN ('CRITICAL', 'MAJOR', 'MINOR')),
    CONSTRAINT incidents_state_valid CHECK (state IN ('OPEN', 'ACKNOWLEDGED', 'RESOLVED'))
);
CREATE INDEX incidents_open_idx ON incidents (project_id) WHERE state <> 'RESOLVED';

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP TABLE IF EXISTS incidents;
DROP TABLE IF EXISTS drift_records;
DROP TABLE IF EXISTS gitops_revisions;
DROP TABLE IF EXISTS health_checks;
DROP TABLE IF EXISTS deployment_counters;
DROP TABLE IF EXISTS deployments;
DROP TABLE IF EXISTS releases;
DROP TABLE IF EXISTS artifacts;
-- +goose StatementEnd
