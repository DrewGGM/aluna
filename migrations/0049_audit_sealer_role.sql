-- The audit sealer gets a role of its own (Q-89, ADR-0021). Until now it ran as aluna_app, the api's role too: the
-- api could read the whole audit log and insert a fake block at the head, which the next sealing would anchor. Now
-- only aluna_sealer reads the records and the blocks, marks the records and inserts the blocks; the worker and the
-- audit CLI take it within their transactions (SET LOCAL ROLE), so their user must be a member of it and the api's
-- must not (GRANT aluna_sealer TO <user>). aluna_app keeps inserting records and reading when a record was stored
-- and whether it is sealed, for the operations metric, never what it says.

-- +goose Up
-- +goose StatementBegin
DO $$
BEGIN
    CREATE ROLE aluna_sealer NOLOGIN;
EXCEPTION WHEN duplicate_object OR unique_violation THEN
    NULL;
END
$$;
-- +goose StatementEnd

GRANT USAGE ON SCHEMA audit TO aluna_sealer;
GRANT SELECT ON audit.records, audit.blocks TO aluna_sealer;
GRANT UPDATE (block_id) ON audit.records TO aluna_sealer;
GRANT INSERT (prev_hash, hash, records) ON audit.blocks TO aluna_sealer;
GRANT USAGE ON SEQUENCE audit.blocks_id_seq TO aluna_sealer;

REVOKE SELECT ON audit.records, audit.blocks FROM aluna_app;
REVOKE UPDATE (block_id) ON audit.records FROM aluna_app;
REVOKE INSERT (prev_hash, hash, records) ON audit.blocks FROM aluna_app;
REVOKE USAGE ON SEQUENCE audit.blocks_id_seq FROM aluna_app;
GRANT SELECT (seq, recorded_at, block_id) ON audit.records TO aluna_app;

-- +goose Down
REVOKE SELECT (seq, recorded_at, block_id) ON audit.records FROM aluna_app;
GRANT SELECT ON audit.records, audit.blocks TO aluna_app;
GRANT UPDATE (block_id) ON audit.records TO aluna_app;
GRANT INSERT (prev_hash, hash, records) ON audit.blocks TO aluna_app;
GRANT USAGE ON SEQUENCE audit.blocks_id_seq TO aluna_app;

-- the role belongs to the cluster, as aluna_app: only its privileges here go
REVOKE ALL ON audit.records, audit.blocks FROM aluna_sealer;
REVOKE USAGE ON SEQUENCE audit.blocks_id_seq FROM aluna_sealer;
REVOKE USAGE ON SCHEMA audit FROM aluna_sealer;
