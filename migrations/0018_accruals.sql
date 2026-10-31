-- Daily interest accrual of deposit accounts (FR-25): one row per account and value date, written once by
-- the accrual job for a closed business date. Each row keeps the account's accrual state at the end of its
-- day (interest.AccrualState, in minor units): the next day starts from it.

-- +goose Up
CREATE TABLE deposits.accruals (
    account_id      uuid NOT NULL REFERENCES deposits.accounts (id),
    value_date      date NOT NULL,
    -- posted balance at the close of the value date; a reversal on an earlier value date can make it negative,
    -- and then the day earns nothing
    balance_units   bigint NOT NULL,
    interest        numeric NOT NULL CHECK (interest >= 0),     -- accrued that day
    accrued_precise numeric NOT NULL CHECK (accrued_precise >= 0), -- accrued since the account opened
    posted_units    bigint NOT NULL CHECK (posted_units >= 0),  -- posted (capitalized) since the account opened
    created_at      timestamptz NOT NULL DEFAULT clock_timestamp(),
    PRIMARY KEY (account_id, value_date)
);

CREATE TRIGGER accruals_immutable BEFORE UPDATE OR DELETE ON deposits.accruals
    FOR EACH ROW EXECUTE FUNCTION deposits.forbid_change();
CREATE TRIGGER accruals_not_truncated BEFORE TRUNCATE ON deposits.accruals
    FOR EACH STATEMENT EXECUTE FUNCTION deposits.forbid_change();

GRANT SELECT, INSERT (account_id, value_date, balance_units, interest, accrued_precise, posted_units)
    ON deposits.accruals TO aluna_app;

-- +goose Down
DROP TABLE deposits.accruals;
