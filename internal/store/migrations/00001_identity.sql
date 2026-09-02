-- +goose Up
-- +goose StatementBegin

-- Identity and access.
--
-- Identifiers are prefixed ULIDs stored as text (see internal/core/domain/ids.go). Text
-- rather than uuid because the prefix is part of the value and carries type information
-- into logs and URLs; the sortability that uuid-ossp would have cost us is provided by
-- the ULID itself, so "ORDER BY id" is still chronological and the primary key index
-- still appends rather than scattering.

CREATE TABLE organisations (
    id          text PRIMARY KEY,
    name        text        NOT NULL,
    slug        text        NOT NULL,
    created_at  timestamptz NOT NULL DEFAULT now(),
    updated_at  timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT organisations_slug_format CHECK (slug ~ '^[a-z0-9][a-z0-9-]{0,62}$')
);
CREATE UNIQUE INDEX organisations_slug_key ON organisations (slug);

CREATE TABLE users (
    id              text PRIMARY KEY,
    organisation_id text        NOT NULL REFERENCES organisations (id) ON DELETE CASCADE,
    email           text        NOT NULL,
    name            text        NOT NULL,
    -- Argon2id encoded string. Never selected into any API response type.
    password_hash   text        NOT NULL,
    is_active       boolean     NOT NULL DEFAULT true,
    created_at      timestamptz NOT NULL DEFAULT now(),
    updated_at      timestamptz NOT NULL DEFAULT now(),
    last_login_at   timestamptz
);
-- Email is unique per organisation, not globally: the same person may exist in two
-- organisations, and a global unique index would leak the existence of an account in
-- another tenant during signup.
CREATE UNIQUE INDEX users_org_email_key ON users (organisation_id, lower(email));

CREATE TABLE teams (
    id              text PRIMARY KEY,
    organisation_id text        NOT NULL REFERENCES organisations (id) ON DELETE CASCADE,
    name            text        NOT NULL,
    slug            text        NOT NULL,
    created_at      timestamptz NOT NULL DEFAULT now()
);
CREATE UNIQUE INDEX teams_org_slug_key ON teams (organisation_id, slug);

-- Roles are a fixed, small set rather than a user-editable permission matrix. The
-- permission strings each role grants live in internal/rbac, in code, because they are
-- checked on every request and a database round trip per authorisation decision buys
-- nothing when the mapping only changes with a deploy.
CREATE TABLE memberships (
    id         text PRIMARY KEY,
    user_id    text        NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    team_id    text        REFERENCES teams (id) ON DELETE CASCADE,
    role       text        NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT memberships_role_valid CHECK (role IN ('ADMIN', 'DEVELOPER', 'VIEWER'))
);
-- A user has at most one role per team, and at most one organisation-wide role
-- (team_id IS NULL). Two partial unique indexes express that; a single index would
-- treat every NULL team_id as distinct and let a user hold several org-wide roles.
CREATE UNIQUE INDEX memberships_user_team_key
    ON memberships (user_id, team_id) WHERE team_id IS NOT NULL;
CREATE UNIQUE INDEX memberships_user_org_key
    ON memberships (user_id) WHERE team_id IS NULL;

CREATE TABLE api_tokens (
    id          text PRIMARY KEY,
    user_id     text        NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    name        text        NOT NULL,
    -- SHA-256 of the token. The plaintext is shown once at creation and never stored,
    -- so a database disclosure does not yield usable credentials.
    token_hash  text        NOT NULL,
    prefix      text        NOT NULL,
    created_at  timestamptz NOT NULL DEFAULT now(),
    expires_at  timestamptz,
    last_used_at timestamptz,
    revoked_at  timestamptz
);
CREATE UNIQUE INDEX api_tokens_hash_key ON api_tokens (token_hash);
CREATE INDEX api_tokens_user_idx ON api_tokens (user_id) WHERE revoked_at IS NULL;

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP TABLE IF EXISTS api_tokens;
DROP TABLE IF EXISTS memberships;
DROP TABLE IF EXISTS teams;
DROP TABLE IF EXISTS users;
DROP TABLE IF EXISTS organisations;
-- +goose StatementEnd
