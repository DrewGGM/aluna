-- Sellado del registro de auditoría por bloques (ADR-0012, T-034): cada bloque encadena el hash de sus
-- registros con el del bloque anterior, y el hash se ancla fuera de la base. Un registro solo cambia una
-- vez: cuando lo sella un bloque nuevo, en la misma transacción que lo crea. Los bloques tampoco se
-- modifican ni se borran.

-- +goose Up
CREATE TABLE audit.blocks (
    id        bigserial PRIMARY KEY,
    prev_hash bytea NOT NULL UNIQUE CHECK (octet_length(prev_hash) = 32), -- ceros en el primero; único: la cadena no se bifurca
    hash      bytea NOT NULL CHECK (octet_length(hash) = 32),
    records   integer NOT NULL CHECK (records > 0),
    sealed_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    -- la transacción que lo creó (64 bits: no da la vuelta); solo en ella se le pueden asignar registros
    sealing_xact xid8 NOT NULL DEFAULT pg_current_xact_id()
);

ALTER TABLE audit.records ADD CONSTRAINT records_block_fk FOREIGN KEY (block_id) REFERENCES audit.blocks (id);
-- before y after son objetos: un NULL de SQL y un null de jsonb se sellarían igual
ALTER TABLE audit.records ADD CONSTRAINT records_summaries_are_objects
    CHECK ((before IS NULL OR jsonb_typeof(before) = 'object') AND (after IS NULL OR jsonb_typeof(after) = 'object'));
-- los pendientes (block_id NULL) y los de un bloque, en orden de seq
CREATE INDEX records_block_seq_idx ON audit.records (block_id, seq);

-- +goose StatementBegin
CREATE OR REPLACE FUNCTION audit.forbid_record_change() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    -- sellar: block_id pasa de NULL a un bloque que esta misma transacción acaba de crear y nada más cambia
    IF TG_OP = 'UPDATE' THEN
        IF OLD.block_id IS NULL AND NEW.block_id IS NOT NULL
           AND (to_jsonb(OLD) - 'block_id') = (to_jsonb(NEW) - 'block_id')
           AND EXISTS (SELECT 1 FROM audit.blocks b WHERE b.id = NEW.block_id AND b.sealing_xact = pg_current_xact_id()) THEN
            RETURN NEW;
        END IF;
    END IF;
    RAISE EXCEPTION 'los registros de auditoría no se modifican ni se borran'
        USING ERRCODE = 'prohibited_sql_statement_attempted', TABLE = 'records';
END
$$;

CREATE FUNCTION audit.forbid_block_change() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    RAISE EXCEPTION 'los bloques de auditoría no se modifican ni se borran'
        USING ERRCODE = 'prohibited_sql_statement_attempted', TABLE = 'blocks';
END
$$;
-- +goose StatementEnd

CREATE TRIGGER blocks_immutable
    BEFORE UPDATE OR DELETE ON audit.blocks
    FOR EACH ROW EXECUTE FUNCTION audit.forbid_block_change();

CREATE TRIGGER blocks_not_truncated
    BEFORE TRUNCATE ON audit.blocks
    FOR EACH STATEMENT EXECUTE FUNCTION audit.forbid_block_change();

-- El sellador corre como aluna_app (el worker): lee los registros, los marca y escribe los bloques. Un rol
-- propio del sellado, que la api no tenga, queda en Q-89.
GRANT SELECT ON audit.records, audit.blocks TO aluna_app;
GRANT UPDATE (block_id) ON audit.records TO aluna_app;
GRANT INSERT (prev_hash, hash, records) ON audit.blocks TO aluna_app;
GRANT USAGE ON SEQUENCE audit.blocks_id_seq TO aluna_app;

-- +goose Down
REVOKE UPDATE (block_id) ON audit.records FROM aluna_app;
REVOKE SELECT ON audit.records FROM aluna_app;
DROP INDEX audit.records_block_seq_idx;
ALTER TABLE audit.records DROP CONSTRAINT records_summaries_are_objects;
ALTER TABLE audit.records DROP CONSTRAINT records_block_fk;
DROP TABLE audit.blocks;
DROP FUNCTION audit.forbid_block_change();

-- +goose StatementBegin
CREATE OR REPLACE FUNCTION audit.forbid_record_change() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    RAISE EXCEPTION 'los registros de auditoría no se modifican ni se borran'
        USING ERRCODE = 'prohibited_sql_statement_attempted', TABLE = 'records';
END
$$;
-- +goose StatementEnd
