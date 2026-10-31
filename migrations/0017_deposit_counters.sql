-- What each deposit account moved per period (FR-24), against which the product's daily and monthly limits
-- are checked: one row per account and business day (period '2026-03-02') and another per month ('2026-03').
-- A movement adds to both in its own transaction, under the account's lock; the rows only grow.

-- +goose Up
CREATE TABLE deposits.counters (
    account_id uuid NOT NULL REFERENCES deposits.accounts (id),
    period     text NOT NULL CHECK (period ~ '^[0-9]{4}-(0[1-9]|1[0-2])(-(0[1-9]|[12][0-9]|3[01]))?$'),
    debits     bigint NOT NULL DEFAULT 0 CHECK (debits >= 0),
    credits    bigint NOT NULL DEFAULT 0 CHECK (credits >= 0),
    PRIMARY KEY (account_id, period)
);

-- +goose StatementBegin
CREATE FUNCTION deposits.guard_counter() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF TG_OP <> 'UPDATE' THEN
        RAISE EXCEPTION 'the counters of deposit accounts are never deleted or truncated'
            USING ERRCODE = 'prohibited_sql_statement_attempted', TABLE = 'counters';
    END IF;
    IF (NEW.account_id, NEW.period) IS DISTINCT FROM (OLD.account_id, OLD.period)
        OR NEW.debits < OLD.debits OR NEW.credits < OLD.credits THEN
        RAISE EXCEPTION 'the counters of deposit account % only grow', OLD.account_id
            USING ERRCODE = 'prohibited_sql_statement_attempted', TABLE = 'counters';
    END IF;
    RETURN NEW;
END
$$;
-- +goose StatementEnd

CREATE TRIGGER counters_guarded BEFORE UPDATE OR DELETE ON deposits.counters
    FOR EACH ROW EXECUTE FUNCTION deposits.guard_counter();
CREATE TRIGGER counters_not_truncated BEFORE TRUNCATE ON deposits.counters
    FOR EACH STATEMENT EXECUTE FUNCTION deposits.guard_counter();

GRANT SELECT, INSERT (account_id, period, debits, credits) ON deposits.counters TO aluna_app;
GRANT UPDATE (debits, credits) ON deposits.counters TO aluna_app;

-- +goose Down
DROP TABLE deposits.counters;
DROP FUNCTION deposits.guard_counter();
