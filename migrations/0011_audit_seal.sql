-- Block sealing of the audit log (ADR-0012, T-034): each block chains the hash of its records with the
-- previous block's hash, and the hash is anchored outside the database. A record changes only once:
-- when a new block seals it, in the same transaction that creates the block. Blocks are never modified
-- or deleted either.

-- +goose Up
CREATE TABLE audit.blocks (
    id        bigserial PRIMARY KEY,
    prev_hash bytea NOT NULL UNIQUE CHECK (octet_length(prev_hash) = 32), -- zeros in the first; unique: the chain never forks
    hash      bytea NOT NULL CHECK (octet_length(hash) = 32),
    records   integer NOT NULL CHECK (records > 0),
    sealed_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    -- the transaction that created it (64 bits: never wraps around); records can be assigned only in it
    sealing_xact xid8 NOT NULL DEFAULT pg_current_xact_id()
);

ALTER TABLE audit.records ADD CONSTRAINT records_block_fk FOREIGN KEY (block_id) REFERENCES audit.blocks (id);
-- before and after are objects: an SQL NULL and a jsonb null would seal the same
ALTER TABLE audit.records ADD CONSTRAINT records_summaries_are_objects
    CHECK ((before IS NULL OR jsonb_typeof(before) = 'object') AND (after IS NULL OR jsonb_typeof(after) = 'object'));
-- pending records (block_id NULL) and those of a block, in seq order
CREATE INDEX records_block_seq_idx ON audit.records (block_id, seq);

-- +goose StatementBegin
CREATE OR REPLACE FUNCTION audit.forbid_record_change() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    -- seal: block_id goes from NULL to a block created by this same transaction; nothing else changes
    IF TG_OP = 'UPDATE' THEN
        IF OLD.block_id IS NULL AND NEW.block_id IS NOT NULL
           AND (to_jsonb(OLD) - 'block_id') = (to_jsonb(NEW) - 'block_id')
           AND EXISTS (SELECT 1 FROM audit.blocks b WHERE b.id = NEW.block_id AND b.sealing_xact = pg_current_xact_id()) THEN
            RETURN NEW;
        END IF;
    END IF;
    RAISE EXCEPTION 'audit records are never modified or deleted'
        USING ERRCODE = 'prohibited_sql_statement_attempted', TABLE = 'records';
END
$$;

CREATE FUNCTION audit.forbid_block_change() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    RAISE EXCEPTION 'audit blocks are never modified or deleted'
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

-- The sealer runs as aluna_app (the worker): it reads the records, marks them and writes the blocks. A
-- role of its own for sealing, which the api would not have, is tracked in Q-89.
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
    RAISE EXCEPTION 'audit records are never modified or deleted'
        USING ERRCODE = 'prohibited_sql_statement_attempted', TABLE = 'records';
END
$$;
-- +goose StatementEnd
