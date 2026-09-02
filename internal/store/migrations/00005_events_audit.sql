-- +goose Up
-- +goose StatementBegin

-- Event transport, idempotency guards and the audit trail.

-- The transactional outbox.
--
-- A state change that must announce itself writes its event here in the same
-- transaction as the state change. A relay drains this table into NATS afterwards.
-- The alternative — publishing to NATS inside the handler — has a failure window in
-- which the row is committed and the event never published, or the event is published
-- and the transaction rolls back. Both leave the system permanently inconsistent with
-- no way to detect it. See docs/events.md.
CREATE TABLE outbox (
    id             bigserial PRIMARY KEY,
    event_id       text        NOT NULL,
    event_type     text        NOT NULL,
    event_version  integer     NOT NULL DEFAULT 1,
    aggregate_type text        NOT NULL,
    aggregate_id   text        NOT NULL,
    correlation_id text        NOT NULL,
    causation_id   text,
    payload        jsonb       NOT NULL,
    occurred_at    timestamptz NOT NULL DEFAULT now(),

    -- Relay bookkeeping.
    published_at   timestamptz,
    attempts       integer     NOT NULL DEFAULT 0,
    last_error     text,
    next_attempt_at timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT outbox_payload_object CHECK (jsonb_typeof(payload) = 'object')
);
CREATE UNIQUE INDEX outbox_event_id_key ON outbox (event_id);
-- The relay's only query: unpublished rows that are due, oldest first. Partial so the
-- index holds the backlog rather than the entire event history.
CREATE INDEX outbox_pending_idx ON outbox (next_attempt_at, id)
    WHERE published_at IS NULL;
CREATE INDEX outbox_aggregate_idx ON outbox (aggregate_type, aggregate_id, id);

-- Consumer-side idempotency.
--
-- JetStream gives at-least-once delivery, so a handler must tolerate seeing the same
-- event twice. A handler records the event here inside the same transaction as its own
-- writes; the unique constraint means the second delivery loses the race and the
-- handler skips its side effects.
CREATE TABLE processed_events (
    consumer     text        NOT NULL,
    event_id     text        NOT NULL,
    processed_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (consumer, event_id)
);
-- Old rows are pruned by the reconciler; the guard only needs to cover the redelivery
-- window, not all of history.
CREATE INDEX processed_events_age_idx ON processed_events (processed_at);

-- Raw inbound webhook deliveries.
--
-- Persisted before any processing, so a delivery that crashes the handler is still
-- visible and replayable rather than lost.
CREATE TABLE webhook_events (
    id            text PRIMARY KEY,
    provider      text        NOT NULL,
    -- The provider's own delivery identifier. The unique index on it is the entire
    -- duplicate-delivery defence: GitHub retries deliveries, and a retry must not
    -- start a second pipeline run for the same push.
    delivery_id   text        NOT NULL,
    event_type    text        NOT NULL,
    repository_id text        REFERENCES repositories (id) ON DELETE SET NULL,
    signature_valid boolean   NOT NULL,
    payload       jsonb       NOT NULL,
    headers       jsonb       NOT NULL DEFAULT '{}'::jsonb,
    state         text        NOT NULL DEFAULT 'RECEIVED',
    error         text,
    received_at   timestamptz NOT NULL DEFAULT now(),
    processed_at  timestamptz,
    CONSTRAINT webhook_events_state_valid
        CHECK (state IN ('RECEIVED', 'PROCESSED', 'IGNORED', 'REJECTED', 'FAILED'))
);
CREATE UNIQUE INDEX webhook_events_provider_delivery_key ON webhook_events (provider, delivery_id);
CREATE INDEX webhook_events_pending_idx ON webhook_events (received_at) WHERE state = 'RECEIVED';
CREATE INDEX webhook_events_repo_idx ON webhook_events (repository_id, id DESC);

-- The audit trail.
--
-- Append-only by convention and by permission: the application role is granted INSERT
-- and SELECT but not UPDATE or DELETE (see 00006_roles.sql), so "immutable" is enforced
-- by the database rather than promised by the code.
CREATE TABLE audit_events (
    id           text PRIMARY KEY,
    organisation_id text     NOT NULL REFERENCES organisations (id) ON DELETE CASCADE,
    actor_id     text        REFERENCES users (id) ON DELETE SET NULL,
    actor_type   text        NOT NULL DEFAULT 'USER',
    actor_label  text        NOT NULL DEFAULT '',
    action       text        NOT NULL,
    resource_type text       NOT NULL,
    resource_id  text        NOT NULL,
    project_id   text        REFERENCES projects (id) ON DELETE SET NULL,
    correlation_id text      NOT NULL DEFAULT '',
    ip_address   inet,
    metadata     jsonb       NOT NULL DEFAULT '{}'::jsonb,
    occurred_at  timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT audit_events_actor_type_valid
        CHECK (actor_type IN ('USER', 'SYSTEM', 'WEBHOOK', 'TOKEN'))
);
CREATE INDEX audit_events_org_idx ON audit_events (organisation_id, id DESC);
CREATE INDEX audit_events_project_idx ON audit_events (project_id, id DESC);
CREATE INDEX audit_events_resource_idx ON audit_events (resource_type, resource_id, id DESC);
CREATE INDEX audit_events_actor_idx ON audit_events (actor_id, id DESC);

-- Secret references. Values live in the configured secret backend; this table records
-- only where to find them and who may. No column here ever holds a secret value.
CREATE TABLE secret_references (
    id          text PRIMARY KEY,
    project_id  text        NOT NULL REFERENCES projects (id) ON DELETE CASCADE,
    key         text        NOT NULL,
    backend     text        NOT NULL DEFAULT 'LOCAL',
    -- Opaque locator interpreted by the backend, e.g. a Vault path or an env var name.
    locator     text        NOT NULL,
    description text        NOT NULL DEFAULT '',
    created_by  text        REFERENCES users (id) ON DELETE SET NULL,
    created_at  timestamptz NOT NULL DEFAULT now(),
    rotated_at  timestamptz,
    CONSTRAINT secret_references_backend_valid CHECK (backend IN ('LOCAL', 'VAULT', 'AWS_SM', 'GCP_SM'))
);
CREATE UNIQUE INDEX secret_references_project_key ON secret_references (project_id, key);

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP TABLE IF EXISTS secret_references;
DROP TABLE IF EXISTS audit_events;
DROP TABLE IF EXISTS webhook_events;
DROP TABLE IF EXISTS processed_events;
DROP TABLE IF EXISTS outbox;
-- +goose StatementEnd
