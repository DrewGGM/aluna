-- The Events API (FR-46) pages an entity's events in the order of the transactions that emitted them, and shows only
-- those of transactions older than every one still running (below the snapshot's xmin): any event that commits later
-- comes from a transaction at or above it, so a consumer that keeps the cursor never skips one. created_xact records
-- that transaction; the events before this migration, all committed, take 1. The indexes serve a page with and
-- without a type, so a poll for a rare type does not walk the whole window. They are built inside the migration's
-- transaction, so on a large outbox they block the emitters while they build: apply it in a window, or build them
-- beforehand with CONCURRENTLY.

-- +goose Up
ALTER TABLE infra.outbox ADD COLUMN created_xact xid8 NOT NULL DEFAULT '1';
ALTER TABLE infra.outbox ALTER COLUMN created_xact SET DEFAULT pg_current_xact_id();
CREATE INDEX outbox_by_entity ON infra.outbox (legal_entity_id, created_xact, id);
CREATE INDEX outbox_by_entity_and_type ON infra.outbox (legal_entity_id, type, created_xact, id);

-- +goose Down
DROP INDEX infra.outbox_by_entity_and_type;
DROP INDEX infra.outbox_by_entity;
ALTER TABLE infra.outbox DROP COLUMN created_xact;
