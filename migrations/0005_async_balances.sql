-- Balance aggregation for async accounts (FR-9, ADR-0004): a posting neither locks nor updates an async
-- account's balance; the aggregator sums its entries later and records how far it got.
--
-- The cursor is (posted_xact, entries.id) and not just entries.id: ids are assigned on insert but
-- transactions commit in a different order, so a slow posting with a lower id would commit behind an
-- id-based cursor and stay out of the balance forever. The aggregator only takes database transactions
-- older than the oldest one in progress (pg_snapshot_xmin), which can no longer add entries; those that
-- commit later have a posted_xact greater than the cursor.

-- +goose Up
CREATE TABLE ledger.async_cursor (
    singleton   boolean PRIMARY KEY DEFAULT true CHECK (singleton), -- a single row
    posted_xact xid8 NOT NULL DEFAULT '0',
    entry_id    bigint NOT NULL DEFAULT 0
);
INSERT INTO ledger.async_cursor DEFAULT VALUES;

CREATE INDEX transactions_posted_xact_idx ON ledger.transactions (posted_xact); -- aggregator batches

-- The cursor only moves forward: rewinding it would add already aggregated entries again.
-- +goose StatementBegin
CREATE FUNCTION ledger.forbid_cursor_rewind() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF (NEW.posted_xact, NEW.entry_id) <= (OLD.posted_xact, OLD.entry_id) THEN
        RAISE EXCEPTION 'the aggregation cursor only moves forward'
            USING ERRCODE = 'prohibited_sql_statement_attempted', TABLE = 'async_cursor';
    END IF;
    RETURN NEW;
END
$$;
-- +goose StatementEnd

CREATE TRIGGER async_cursor_forward
    BEFORE UPDATE ON ledger.async_cursor
    FOR EACH ROW EXECUTE FUNCTION ledger.forbid_cursor_rewind();

GRANT SELECT ON ledger.async_cursor TO aluna_app;
GRANT UPDATE (posted_xact, entry_id) ON ledger.async_cursor TO aluna_app;

-- +goose Down
DROP TABLE ledger.async_cursor;
DROP FUNCTION ledger.forbid_cursor_rewind();
DROP INDEX ledger.transactions_posted_xact_idx;
