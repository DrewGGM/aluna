-- A clock that steps back does not reject a valid posting (Q-149): ledger.stamp_entry and
-- deposits.require_status_change tell a row of this transaction from one restored from another cluster with the same
-- xid by its stamp (clock_timestamp()) against this transaction's start. A clock stepped back after the start (an
-- NTP step, a resumed virtual machine) stamps before it, and the check refused this transaction's own rows. Both now
-- allow five minutes: a row restored with pg_dump would have to be dumped, restored and reached by the new cluster's
-- xid counter within five minutes of its stamp to pass. After a cutover by logical replication the new cluster's
-- counter is its own and may reach numbers the old one used soon after, so open traffic on it five minutes after
-- the switchover (runbook).

-- +goose Up
-- +goose StatementBegin
CREATE OR REPLACE FUNCTION ledger.stamp_entry() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE
    parent ledger.transactions%ROWTYPE;
BEGIN
    SELECT * INTO parent FROM ledger.transactions WHERE id = NEW.transaction_id;
    IF NOT FOUND THEN
        RAISE EXCEPTION 'transaction % does not exist', NEW.transaction_id USING ERRCODE = 'foreign_key_violation';
    END IF;
    -- posted_at covers what xid8 does not: after a logical restore into another cluster the xid counter
    -- starts over and could repeat, but restored data lies more than five minutes in the past; a clock stepped
    -- back by less does not refuse this transaction's own legs.
    IF parent.posted_xact <> pg_current_xact_id() OR parent.posted_at < transaction_timestamp() - interval '5 minutes' THEN
        RAISE EXCEPTION 'transaction % is already posted: it accepts no new entries', NEW.transaction_id
            USING ERRCODE = 'prohibited_sql_statement_attempted';
    END IF;
    NEW.legal_entity_id := parent.legal_entity_id;
    NEW.value_date := parent.value_date;
    NEW.posted_at := parent.posted_at;
    RETURN NEW;
END
$$;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE OR REPLACE FUNCTION deposits.require_status_change() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM deposits.status_changes
                   WHERE account_id = NEW.id AND from_status = OLD.status AND to_status = NEW.status
                     AND changed_xact = pg_current_xact_id() AND changed_at >= transaction_timestamp() - interval '5 minutes') THEN
        RAISE EXCEPTION 'deposit account % went from % to % without its status change', NEW.id, OLD.status, NEW.status
            USING ERRCODE = 'prohibited_sql_statement_attempted', TABLE = 'accounts';
    END IF;
    RETURN NULL;
END
$$;
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
CREATE OR REPLACE FUNCTION ledger.stamp_entry() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE
    parent ledger.transactions%ROWTYPE;
BEGIN
    SELECT * INTO parent FROM ledger.transactions WHERE id = NEW.transaction_id;
    IF NOT FOUND THEN
        RAISE EXCEPTION 'transaction % does not exist', NEW.transaction_id USING ERRCODE = 'foreign_key_violation';
    END IF;
    -- posted_at covers what xid8 does not: after a logical restore into another cluster the xid counter
    -- starts over and could repeat, but restored data always lies in the past.
    IF parent.posted_xact <> pg_current_xact_id() OR parent.posted_at < transaction_timestamp() THEN
        RAISE EXCEPTION 'transaction % is already posted: it accepts no new entries', NEW.transaction_id
            USING ERRCODE = 'prohibited_sql_statement_attempted';
    END IF;
    NEW.legal_entity_id := parent.legal_entity_id;
    NEW.value_date := parent.value_date;
    NEW.posted_at := parent.posted_at;
    RETURN NEW;
END
$$;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE OR REPLACE FUNCTION deposits.require_status_change() RETURNS trigger LANGUAGE plpgsql AS $$
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
