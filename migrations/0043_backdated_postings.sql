-- Back-dating only up to the last close (ADR-0006, Q-76 b): a transaction whose business date falls in a closed period
-- or before one is refused, a month without a row included, not only one in the closed month itself.
--
-- The ledger checks before posting, without locking. A close takes its own month's lock, which a posting into an
-- earlier month does not take, so a back-dated posting that checked before a later close could commit after it. A
-- back-dated transaction (a business date in a month before the entity's) also takes the entity's back-dated lock,
-- shared, and the close takes it exclusive: the close waits for those in flight and keeps out the rest until it ends.
-- A transaction in the entity's month or later takes no extra lock: no month that can close is after it.

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

-- +goose Down
-- +goose StatementBegin
CREATE OR REPLACE FUNCTION calendar.refuse_closed_period() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE
    month text := to_char(NEW.business_date, 'YYYY-MM');
BEGIN
    PERFORM pg_advisory_xact_lock_shared(calendar.period_lock_key(NEW.legal_entity_id, month));
    -- a statement after the lock: it sees a close that committed while this one waited
    IF EXISTS (SELECT 1 FROM calendar.periods
               WHERE legal_entity_id = NEW.legal_entity_id AND period = month AND status = 'closed') THEN
        RAISE EXCEPTION 'accounting period % is closed', month
            USING ERRCODE = 'check_violation', TABLE = 'transactions', CONSTRAINT = 'transactions_in_open_period';
    END IF;
    RETURN NEW;
END
$$;
-- +goose StatementEnd
