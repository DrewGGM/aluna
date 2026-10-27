-- Registro de auditoría de comandos (FR-40, ADR-0012): cada comando deja aquí quién, desde dónde, qué
-- pidió y qué resultó, en la misma transacción que su efecto. Es evidencia: nadie lo modifica ni lo
-- borra, tampoco el dueño de las tablas. El sellado por bloques (T-034) llenará block_id.

-- +goose Up
CREATE TABLE audit.records (
    seq            bigserial PRIMARY KEY,
    block_id       bigint,
    recorded_at    timestamptz NOT NULL DEFAULT clock_timestamp(),
    actor          text NOT NULL CHECK (actor <> ''),
    role           text NOT NULL CHECK (role <> ''),
    channel        text NOT NULL CHECK (channel <> ''),
    ip             text NOT NULL,
    correlation_id text NOT NULL CHECK (correlation_id <> ''),
    command        text NOT NULL CHECK (command <> ''),
    targets        text[] NOT NULL,
    result         text NOT NULL CHECK (result <> ''),
    before         jsonb,
    after          jsonb
);

-- +goose StatementBegin
CREATE FUNCTION audit.forbid_record_change() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    RAISE EXCEPTION 'los registros de auditoría no se modifican ni se borran'
        USING ERRCODE = 'prohibited_sql_statement_attempted', TABLE = 'records';
END
$$;
-- +goose StatementEnd

CREATE TRIGGER records_immutable
    BEFORE UPDATE OR DELETE ON audit.records
    FOR EACH ROW EXECUTE FUNCTION audit.forbid_record_change();

CREATE TRIGGER records_not_truncated
    BEFORE TRUNCATE ON audit.records
    FOR EACH STATEMENT EXECUTE FUNCTION audit.forbid_record_change();

REVOKE UPDATE, DELETE, TRUNCATE ON audit.records FROM aluna_app;
GRANT INSERT (actor, role, channel, ip, correlation_id, command, targets, result, before, after) ON audit.records TO aluna_app;
GRANT USAGE ON SEQUENCE audit.records_seq_seq TO aluna_app;

-- +goose Down
DROP TABLE audit.records;
DROP FUNCTION audit.forbid_record_change();
