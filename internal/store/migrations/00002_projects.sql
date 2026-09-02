-- +goose Up
-- +goose StatementBegin

-- Projects, the repositories they track, and the environments they deploy into.

CREATE TABLE projects (
    id              text PRIMARY KEY,
    organisation_id text        NOT NULL REFERENCES organisations (id) ON DELETE CASCADE,
    name            text        NOT NULL,
    slug            text        NOT NULL,
    description     text        NOT NULL DEFAULT '',
    -- Detected from repository contents at import time (see internal/repos/detect.go).
    -- Deterministic file-based detection, not inference.
    runtime         text        NOT NULL DEFAULT 'UNKNOWN',
    created_by      text        REFERENCES users (id) ON DELETE SET NULL,
    created_at      timestamptz NOT NULL DEFAULT now(),
    updated_at      timestamptz NOT NULL DEFAULT now(),
    archived_at     timestamptz,
    CONSTRAINT projects_slug_format CHECK (slug ~ '^[a-z0-9][a-z0-9-]{0,62}$'),
    CONSTRAINT projects_runtime_valid
        CHECK (runtime IN ('NODE', 'GO', 'PYTHON', 'UNKNOWN'))
);
CREATE UNIQUE INDEX projects_org_slug_key ON projects (organisation_id, slug);
CREATE INDEX projects_org_idx ON projects (organisation_id) WHERE archived_at IS NULL;

CREATE TABLE repositories (
    id            text PRIMARY KEY,
    project_id    text        NOT NULL REFERENCES projects (id) ON DELETE CASCADE,
    provider      text        NOT NULL,
    owner         text        NOT NULL,
    name          text        NOT NULL,
    clone_url     text        NOT NULL,
    default_branch text       NOT NULL DEFAULT 'main',
    -- Reference into the secret store, never a credential. See internal/auth/secrets.go.
    webhook_secret_ref text,
    connected_at  timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT repositories_provider_valid CHECK (provider IN ('GITHUB', 'GITLAB', 'LOCAL'))
);
-- One project tracks one repository in this version, but the schema does not forbid a
-- second, so adding monorepo support later does not require a migration of existing rows.
CREATE INDEX repositories_project_idx ON repositories (project_id);
CREATE UNIQUE INDEX repositories_provider_path_key ON repositories (provider, owner, name);

CREATE TABLE environments (
    id           text PRIMARY KEY,
    project_id   text        NOT NULL REFERENCES projects (id) ON DELETE CASCADE,
    name         text        NOT NULL,
    kind         text        NOT NULL,
    namespace    text        NOT NULL,
    url          text,
    -- Preview environments are created from a pull request and destroyed when it
    -- closes. These columns are meaningless for long-lived environments and are
    -- constrained to be absent there.
    pull_request_number integer,
    source_branch       text,
    expires_at          timestamptz,
    state        text        NOT NULL DEFAULT 'ACTIVE',
    created_at   timestamptz NOT NULL DEFAULT now(),
    destroyed_at timestamptz,
    CONSTRAINT environments_kind_valid
        CHECK (kind IN ('DEVELOPMENT', 'PREVIEW', 'STAGING', 'PRODUCTION')),
    CONSTRAINT environments_state_valid
        CHECK (state IN ('PROVISIONING', 'ACTIVE', 'DESTROYING', 'DESTROYED', 'FAILED')),
    -- Only a preview environment may carry pull-request provenance or an expiry.
    CONSTRAINT environments_preview_fields
        CHECK (
            (kind = 'PREVIEW')
            OR (pull_request_number IS NULL AND expires_at IS NULL)
        )
);
CREATE UNIQUE INDEX environments_project_name_key ON environments (project_id, name);
CREATE INDEX environments_project_idx ON environments (project_id) WHERE destroyed_at IS NULL;
-- The reconciler sweeps this every cycle, so it gets its own partial index.
CREATE INDEX environments_expiry_idx ON environments (expires_at)
    WHERE kind = 'PREVIEW' AND state = 'ACTIVE' AND expires_at IS NOT NULL;

CREATE TABLE services (
    id             text PRIMARY KEY,
    project_id     text        NOT NULL REFERENCES projects (id) ON DELETE CASCADE,
    name           text        NOT NULL,
    port           integer     NOT NULL DEFAULT 8080,
    health_path    text        NOT NULL DEFAULT '/healthz',
    created_at     timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT services_port_range CHECK (port > 0 AND port < 65536)
);
CREATE UNIQUE INDEX services_project_name_key ON services (project_id, name);

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP TABLE IF EXISTS services;
DROP TABLE IF EXISTS environments;
DROP TABLE IF EXISTS repositories;
DROP TABLE IF EXISTS projects;
-- +goose StatementEnd
