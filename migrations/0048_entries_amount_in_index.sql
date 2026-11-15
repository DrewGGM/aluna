-- The bitemporal balance (Q-68) sums an account's entries up to a value date and a posted instant. Its index had
-- no amount, so the sum read every entry from the table: on an account with millions of them (the async ones) that
-- outran the statement timeout. With the amount in the index, PostgreSQL sums from the index alone (index-only
-- scan, as the entries are never updated). The index is replaced CONCURRENTLY, so a database with history keeps
-- taking postings meanwhile: outside a transaction, the new one first.

-- +goose NO TRANSACTION

-- +goose Up
CREATE INDEX CONCURRENTLY IF NOT EXISTS entries_account_value_amount_idx
    ON ledger.entries (account_id, value_date, posted_at) INCLUDE (amount);
DROP INDEX CONCURRENTLY IF EXISTS ledger.entries_account_value_idx;

-- +goose Down
CREATE INDEX CONCURRENTLY IF NOT EXISTS entries_account_value_idx ON ledger.entries (account_id, value_date, posted_at);
DROP INDEX CONCURRENTLY IF EXISTS ledger.entries_account_value_amount_idx;
