-- A retry that crosses a close finds its original (Q-139): two postings with the same (source, external_ref) run at
-- once while their period closes; the one that loses met the period closed in refuse_closed_period before the
-- insert's ON CONFLICT could see the winner, and got period_closed although the money moved. A BEFORE trigger runs
-- before the conflict check, so in a closed period the trigger lets through a transaction whose reference is already
-- posted: the insert then conflicts and inserts nothing, and the ledger answers with the original. An open period
-- reads nothing more.

-- +goose Up
-- +goose StatementBegin
CREATE OR REPLACE FUNCTION calendar.refuse_closed_period() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE
    month text := to_char(NEW.business_date, 'YYYY-MM');
BEGIN
    PERFORM pg_advisory_xact_lock_shared(calendar.period_lock_key(NEW.legal_entity_id, month));
    IF month < (SELECT to_char(business_date, 'YYYY-MM') FROM calendar.legal_entities WHERE id = NEW.legal_entity_id) THEN
        PERFORM pg_advisory_xact_lock_shared(calendar.period_lock_key(NEW.legal_entity_id, 'backdated'));
    END IF;
    -- statements after the locks: they see a close, or the original of a retry, that committed while this one waited
    IF EXISTS (SELECT 1 FROM calendar.periods
               WHERE legal_entity_id = NEW.legal_entity_id AND period >= month AND status = 'closed') THEN
        -- the original of a retry: the insert conflicts on transactions_idempotent_ref and inserts nothing
        IF NOT EXISTS (SELECT 1 FROM ledger.transactions
                       WHERE legal_entity_id = NEW.legal_entity_id AND source = NEW.source AND external_ref = NEW.external_ref) THEN
            RAISE EXCEPTION 'accounting period % is closed or before a closed period', month
                USING ERRCODE = 'check_violation', TABLE = 'transactions', CONSTRAINT = 'transactions_in_open_period';
        END IF;
    END IF;
    RETURN NEW;
END
$$;
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
CREATE OR REPLACE FUNCTION calendar.refuse_closed_period() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE
    month text := to_char(NEW.business_date, 'YYYY-MM');
BEGIN
    PERFORM pg_advisory_xact_lock_shared(calendar.period_lock_key(NEW.legal_entity_id, month));
    IF month < (SELECT to_char(business_date, 'YYYY-MM') FROM calendar.legal_entities WHERE id = NEW.legal_entity_id) THEN
        PERFORM pg_advisory_xact_lock_shared(calendar.period_lock_key(NEW.legal_entity_id, 'backdated'));
    END IF;
    -- a statement after the locks: it sees a close that committed while this one waited
    IF EXISTS (SELECT 1 FROM calendar.periods
               WHERE legal_entity_id = NEW.legal_entity_id AND period >= month AND status = 'closed') THEN
        RAISE EXCEPTION 'accounting period % is closed or before a closed period', month
            USING ERRCODE = 'check_violation', TABLE = 'transactions', CONSTRAINT = 'transactions_in_open_period';
    END IF;
    RETURN NEW;
END
$$;
-- +goose StatementEnd
