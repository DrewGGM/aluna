-- Indexes that the purges and reads of tables that only grow need with volume: the outbox purge, whose cascade
-- reads the inbox by event and whose batches look for published events; the list of screening cases of an entity,
-- by status or not; the count of an account's status changes that guards a freeze; and the dormancy job, which
-- looks for an entity's active accounts by product version. They are built CONCURRENTLY, so that a database with
-- history keeps taking writes meanwhile: outside a transaction, one by one.

-- +goose NO TRANSACTION

-- +goose Up
CREATE INDEX CONCURRENTLY IF NOT EXISTS inbox_by_event ON infra.inbox (event_id);
CREATE INDEX CONCURRENTLY IF NOT EXISTS outbox_by_publication ON infra.outbox (published_at);
CREATE INDEX CONCURRENTLY IF NOT EXISTS cases_by_entity ON compliance.cases (legal_entity_id, id);
CREATE INDEX CONCURRENTLY IF NOT EXISTS cases_by_entity_status ON compliance.cases (legal_entity_id, status, id);
CREATE INDEX CONCURRENTLY IF NOT EXISTS status_changes_by_account ON deposits.status_changes (account_id);
CREATE INDEX CONCURRENTLY IF NOT EXISTS accounts_active_by_version ON deposits.accounts (legal_entity_id, product_version_id, id)
    WHERE status = 'active';

-- +goose Down
DROP INDEX CONCURRENTLY IF EXISTS deposits.accounts_active_by_version;
DROP INDEX CONCURRENTLY IF EXISTS deposits.status_changes_by_account;
DROP INDEX CONCURRENTLY IF EXISTS compliance.cases_by_entity_status;
DROP INDEX CONCURRENTLY IF EXISTS compliance.cases_by_entity;
DROP INDEX CONCURRENTLY IF EXISTS infra.outbox_by_publication;
DROP INDEX CONCURRENTLY IF EXISTS infra.inbox_by_event;
