-- Almacén de idempotencia (architecture §9, ADR-0009): una fila por (scope, clave) que se escribe en la
-- misma transacción del efecto. Nace en 'in_progress' y se completa con la respuesta antes del commit;
-- nunca queda guardada a medias, ni cambia una vez completada. Solo se guardan respuestas 2xx y 4xx: un
-- 5xx no se guarda para que el cliente pueda reintentar. aluna_app solo inserta la reserva (scope, key,
-- huella) y solo actualiza la respuesta: no puede fabricar una respuesta guardada ni cambiar la huella.

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

CREATE INDEX idempotency_keys_created_at_idx ON infra.idempotency_keys (created_at); -- para la purga

-- Una fila solo pasa de in_progress a completed una vez: la respuesta guardada no cambia.
-- +goose StatementBegin
CREATE FUNCTION infra.forbid_idempotency_response_change() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF OLD.status <> 'in_progress' OR NEW.status <> 'completed' THEN
        RAISE EXCEPTION 'una clave de idempotencia solo se completa una vez'
            USING ERRCODE = 'prohibited_sql_statement_attempted', TABLE = 'idempotency_keys';
    END IF;
    RETURN NEW;
END
$$;
-- +goose StatementEnd

CREATE TRIGGER idempotency_keys_complete_once
    BEFORE UPDATE ON infra.idempotency_keys
    FOR EACH ROW EXECUTE FUNCTION infra.forbid_idempotency_response_change();

-- Al commit, la clave reclamada tiene que estar completada: si no, cada reintento sería
-- request_in_progress hasta la purga.
-- +goose StatementBegin
CREATE FUNCTION infra.check_idempotency_key_completed() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF EXISTS (SELECT 1 FROM infra.idempotency_keys
               WHERE scope = NEW.scope AND key = NEW.key AND status = 'in_progress') THEN
        RAISE EXCEPTION 'la clave de idempotencia % se reclamó y no se completó', NEW.key
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
-- Solo sirve en desarrollo y pruebas: en producción las claves vigentes se perderían.
DROP TABLE infra.idempotency_keys;
DROP FUNCTION infra.check_idempotency_key_completed(), infra.forbid_idempotency_response_change();
