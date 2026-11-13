-- Closing a period (FR-37, US-14): the application moves a month from open to closing and from closing to closed, and
-- a closed one never changes (Constitution II: no reopening). The database stamps when it closed. A close that does
-- not finish is undone with its transaction, so the period stays open.
--
-- The ledger checks the period before posting, without locking, and writes later: a close in between would not see
-- that posting and would close under it. So every transaction takes its period's advisory lock, shared, and checks
-- again that the period is not closed; the close takes it exclusive before going closing, waits for the postings in
-- flight in that period and keeps out the rest until it ends. Postings in other periods do not wait.

-- +goose Up
-- +goose StatementBegin
CREATE FUNCTION calendar.guard_period() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF TG_OP <> 'UPDATE' THEN
        RAISE EXCEPTION 'periods are never deleted or truncated'
            USING ERRCODE = 'prohibited_sql_statement_attempted', TABLE = 'periods';
    END IF;
    IF (NEW.legal_entity_id, NEW.period) IS DISTINCT FROM (OLD.legal_entity_id, OLD.period)
        OR NOT ((OLD.status = 'open' AND NEW.status = 'closing' AND NEW.closed_by IS NULL)
            OR (OLD.status = 'closing' AND NEW.status = 'closed')) THEN
        RAISE EXCEPTION 'a period goes from open to closing and from closing to closed, once: period %', OLD.period
            USING ERRCODE = 'prohibited_sql_statement_attempted', TABLE = 'periods';
    END IF;
    IF NEW.status = 'closed' THEN
        NEW.closed_at := clock_timestamp();
    END IF;
    RETURN NEW;
END
$$;
-- +goose StatementEnd

CREATE TRIGGER periods_guarded BEFORE UPDATE OR DELETE ON calendar.periods
    FOR EACH ROW EXECUTE FUNCTION calendar.guard_period();
CREATE TRIGGER periods_not_truncated BEFORE TRUNCATE ON calendar.periods
    FOR EACH STATEMENT EXECUTE FUNCTION calendar.guard_period();

-- The key of a period's advisory lock, the same for the close and for the postings.
CREATE FUNCTION calendar.period_lock_key(entity uuid, period text) RETURNS bigint LANGUAGE sql IMMUTABLE
    RETURN hashtextextended('calendar.periods:' || entity::text || ':' || period, 0);

-- +goose StatementBegin
CREATE FUNCTION calendar.refuse_closed_period() RETURNS trigger LANGUAGE plpgsql AS $$
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

CREATE TRIGGER transactions_in_open_period BEFORE INSERT ON ledger.transactions
    FOR EACH ROW EXECUTE FUNCTION calendar.refuse_closed_period();

GRANT INSERT (legal_entity_id, period, status), UPDATE (status, closed_by) ON calendar.periods TO aluna_app;

-- +goose Down
REVOKE INSERT (legal_entity_id, period, status), UPDATE (status, closed_by) ON calendar.periods FROM aluna_app;
DROP TRIGGER transactions_in_open_period ON ledger.transactions;
DROP FUNCTION calendar.refuse_closed_period();
DROP FUNCTION calendar.period_lock_key(uuid, text);
DROP TRIGGER periods_not_truncated ON calendar.periods;
DROP TRIGGER periods_guarded ON calendar.periods;
DROP FUNCTION calendar.guard_period();
