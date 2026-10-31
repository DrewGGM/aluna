-- Event outbox and inbox (FR-12, ADR-0007). The module that produces an event writes its row in the
-- same transaction as the effect, together with the River job that publishes it; each internal consumer
-- records in the inbox the events it has already processed, in the same transaction as its effect, so
-- an event delivered twice is processed once.

-- +goose Up
CREATE TABLE infra.outbox (
    id              uuid PRIMARY KEY, -- UUIDv7: the CloudEvent id
    -- aluna.<module>.<aggregate>.<event>.v<N>
    type            text NOT NULL CHECK (type ~ '^aluna\.[a-z]+\.[a-z-]+\.[a-z-]+\.v[1-9][0-9]*$'),
    subject         text NOT NULL CHECK (subject <> ''),
    legal_entity_id uuid NOT NULL CHECK (legal_entity_id <> '00000000-0000-0000-0000-000000000000'),
    correlation_id  uuid,
    schema_version  integer NOT NULL CHECK (schema_version >= 1),
    payload         jsonb NOT NULL,
    created_at      timestamptz NOT NULL DEFAULT clock_timestamp(),
    published_at    timestamptz
);

CREATE TABLE infra.inbox (
    consumer     text NOT NULL CHECK (consumer <> ''),
    event_id     uuid NOT NULL REFERENCES infra.outbox (id) ON DELETE CASCADE, -- an outbox purge cascades to its inbox
    processed_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    PRIMARY KEY (consumer, event_id)
);

GRANT SELECT ON infra.outbox TO aluna_app;
GRANT INSERT (id, type, subject, legal_entity_id, correlation_id, schema_version, payload) ON infra.outbox TO aluna_app;
GRANT UPDATE (published_at) ON infra.outbox TO aluna_app;
GRANT DELETE ON infra.outbox TO aluna_app; -- purge of old published rows (periodic worker job)
GRANT INSERT (consumer, event_id) ON infra.inbox TO aluna_app;

-- +goose Down
DROP TABLE infra.inbox, infra.outbox;
