-- Outbox e inbox de eventos (FR-12, ADR-0007). El módulo que produce un evento escribe su fila en la
-- misma transacción que el efecto, junto con el job River que lo publica; cada consumidor interno anota
-- en el inbox los eventos que ya procesó, en la misma transacción que su efecto, y así un evento
-- entregado dos veces se procesa una vez.

-- +goose Up
CREATE TABLE infra.outbox (
    id              uuid PRIMARY KEY, -- UUIDv7: el id del CloudEvent
    -- aluna.<modulo>.<agregado>.<evento>.v<N>
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
    event_id     uuid NOT NULL REFERENCES infra.outbox (id) ON DELETE CASCADE, -- la purga del outbox se lleva su inbox
    processed_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    PRIMARY KEY (consumer, event_id)
);

GRANT SELECT ON infra.outbox TO aluna_app;
GRANT INSERT (id, type, subject, legal_entity_id, correlation_id, schema_version, payload) ON infra.outbox TO aluna_app;
GRANT UPDATE (published_at) ON infra.outbox TO aluna_app;
GRANT DELETE ON infra.outbox TO aluna_app; -- purga de publicados viejos (job periódico del worker)
GRANT INSERT (consumer, event_id) ON infra.inbox TO aluna_app;

-- +goose Down
DROP TABLE infra.inbox, infra.outbox;
