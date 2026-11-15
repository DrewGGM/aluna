-- A deposit account's status changes only with its row in deposits.status_changes (Q-141 c): the history is what
-- the freeze requests count and what operations reads, and a change by hand would skip it. Each row records the
-- transaction that wrote it; a status update needs a row of its own transaction with its from and to, written in it
-- (changed_at covers what xid8 does not after a logical restore, as ledger.stamp_entry does). Only the defaults fill
-- both columns; old rows get transaction 1, which no change matches. One row of a transaction stands for every
-- change with its from and to in it: the application writes one per change (Store.SetStatus).
--
-- A process from before this migration updates before it records, and the trigger refuses it: deploy the api and
-- the worker with it, and roll it back before rolling them back.

-- +goose Up
ALTER TABLE deposits.status_changes ADD COLUMN changed_xact xid8 NOT NULL DEFAULT '1';
ALTER TABLE deposits.status_changes ALTER COLUMN changed_xact SET DEFAULT pg_current_xact_id();

-- +goose StatementBegin
CREATE FUNCTION deposits.require_status_change() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM deposits.status_changes
                   WHERE account_id = NEW.id AND from_status = OLD.status AND to_status = NEW.status
                     AND changed_xact = pg_current_xact_id() AND changed_at >= transaction_timestamp()) THEN
        RAISE EXCEPTION 'deposit account % went from % to % without its status change', NEW.id, OLD.status, NEW.status
            USING ERRCODE = 'prohibited_sql_statement_attempted', TABLE = 'accounts';
    END IF;
    RETURN NULL;
END
$$;
-- +goose StatementEnd

CREATE TRIGGER accounts_status_recorded AFTER UPDATE OF status ON deposits.accounts
    FOR EACH ROW WHEN (OLD.status IS DISTINCT FROM NEW.status) EXECUTE FUNCTION deposits.require_status_change();

-- +goose Down
DROP TRIGGER accounts_status_recorded ON deposits.accounts;
DROP FUNCTION deposits.require_status_change();
ALTER TABLE deposits.status_changes DROP COLUMN changed_xact;
