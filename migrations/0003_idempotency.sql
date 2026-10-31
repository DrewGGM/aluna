-- Idempotency store (architecture §9, ADR-0009): one row per (scope, key), written in the same
-- transaction as the effect. It starts as 'in_progress' and is completed with the response before the
-- commit; it is never stored half-done and never changes once completed. Only 2xx and 4xx responses are
-- stored: a 5xx is not, so the client can retry. aluna_app only inserts the claim (scope, key,
-- fingerprint) and only updates the response: it cannot forge a stored response or change the
-- fingerprint.

-- +goose Up
CREATE TABLE infra.idempotency_keys (
    scope         text NOT NULL CHECK (length(scope) BETWEEN 1 AND 200),
    key           text NOT NULL CHECK (length(key) BETWEEN 1 AND 255),
    request_hash  bytea NOT NULL CHECK (octet_length(request_hash) = 32),
    status        text NOT NULL DEFAULT 'in_progress',
    response_code integer CHECK (response_code BETWEEN 200 AND 499),
    response_body bytea,
    created_at    timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (scope, key),
    CONSTRAINT idempotency_keys_response_by_status CHECK (
        (status = 'in_progress' AND response_code IS NULL AND response_body IS NULL)
        OR (status = 'completed' AND response_code IS NOT NULL AND response_body IS NOT NULL))
);

CREATE INDEX idempotency_keys_created_at_idx ON infra.idempotency_keys (created_at); -- for the purge

-- A row moves from in_progress to completed only once: the stored response never changes.
-- +goose StatementBegin
CREATE FUNCTION infra.forbid_idempotency_response_change() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF OLD.status <> 'in_progress' OR NEW.status <> 'completed' THEN
        RAISE EXCEPTION 'an idempotency key is completed only once'
            USING ERRCODE = 'prohibited_sql_statement_attempted', TABLE = 'idempotency_keys';
    END IF;
    RETURN NEW;
END
$$;
-- +goose StatementEnd

CREATE TRIGGER idempotency_keys_complete_once
    BEFORE UPDATE ON infra.idempotency_keys
    FOR EACH ROW EXECUTE FUNCTION infra.forbid_idempotency_response_change();

-- At commit, the claimed key must be completed: otherwise every retry would get request_in_progress
-- until the purge.
-- +goose StatementBegin
CREATE FUNCTION infra.check_idempotency_key_completed() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF EXISTS (SELECT 1 FROM infra.idempotency_keys
               WHERE scope = NEW.scope AND key = NEW.key AND status = 'in_progress') THEN
        RAISE EXCEPTION 'idempotency key % was claimed but not completed', NEW.key
            USING ERRCODE = 'check_violation', TABLE = 'idempotency_keys';
    END IF;
    RETURN NULL;
END
$$;
-- +goose StatementEnd

CREATE CONSTRAINT TRIGGER idempotency_keys_completed
    AFTER INSERT ON infra.idempotency_keys
    DEFERRABLE INITIALLY DEFERRED
    FOR EACH ROW EXECUTE FUNCTION infra.check_idempotency_key_completed();

GRANT SELECT, DELETE ON infra.idempotency_keys TO aluna_app;
GRANT INSERT (scope, key, request_hash) ON infra.idempotency_keys TO aluna_app;
GRANT UPDATE (status, response_code, response_body) ON infra.idempotency_keys TO aluna_app;

-- +goose Down
-- Only for development and tests: in production the live keys would be lost.
DROP TABLE infra.idempotency_keys;
DROP FUNCTION infra.check_idempotency_key_completed(), infra.forbid_idempotency_response_change();
