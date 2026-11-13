-- Daily GL (FR-36, ADR-0011): what each CUIF code moved on each closed business date, by currency, recalculated from
-- the journal and never edited by hand. gl_days says which days are projected, how it went and up to where they saw
-- the journal: seen_below is the oldest transaction in progress when the projection read the day, so one with an id
-- from it on may be missing and the day is projected again. A day with an account without a mapping in force is
-- unmapped and has no lines. Reconciliation gets a control for those accounts. The journal gets an index by entity
-- and business date, which the projection and the control read by.

-- +goose Up
CREATE TABLE accounting.gl_days (
    legal_entity_id uuid NOT NULL,
    business_date   date NOT NULL,
    status          text NOT NULL CHECK (status IN ('projected', 'unmapped')),
    seen_below      xid8 NOT NULL,
    projected_at    timestamptz NOT NULL DEFAULT clock_timestamp(),
    PRIMARY KEY (legal_entity_id, business_date)
);

CREATE TABLE accounting.gl_lines (
    legal_entity_id uuid NOT NULL,
    business_date   date NOT NULL,
    cuif_code       text NOT NULL CHECK (cuif_code ~ '^[0-9]{6,10}$'),
    currency        char(3) NOT NULL,
    debit_units     bigint NOT NULL CHECK (debit_units >= 0),  -- in minor units, positive
    credit_units    bigint NOT NULL CHECK (credit_units >= 0), -- in minor units, positive
    PRIMARY KEY (legal_entity_id, business_date, cuif_code, currency),
    FOREIGN KEY (legal_entity_id, business_date) REFERENCES accounting.gl_days (legal_entity_id, business_date)
);

CREATE INDEX transactions_by_business_date ON ledger.transactions (legal_entity_id, business_date);

GRANT SELECT, INSERT (legal_entity_id, business_date, status, seen_below), UPDATE (status, seen_below, projected_at)
    ON accounting.gl_days TO aluna_app;
GRANT SELECT, INSERT, DELETE ON accounting.gl_lines TO aluna_app;

ALTER TABLE recon.control_runs
    DROP CONSTRAINT control_runs_control_check,
    ADD CONSTRAINT control_runs_control_check CHECK (control IN
        ('trial_balance', 'balances_match_entries', 'rail_accounts_at_zero', 'customers_match_control', 'accounts_mapped_to_cuif'));
ALTER TABLE recon.breaks
    DROP CONSTRAINT breaks_control_check,
    ADD CONSTRAINT breaks_control_check CHECK (control IN
        ('trial_balance', 'balances_match_entries', 'rail_accounts_at_zero', 'customers_match_control', 'statement_match',
         'accounts_mapped_to_cuif'));

-- +goose Down
-- The controls' CHECKs stay as they are: runs and breaks of accounts_mapped_to_cuif are never deleted, and a CHECK
-- that also allows it does no harm.
DROP INDEX ledger.transactions_by_business_date;
DROP TABLE accounting.gl_lines, accounting.gl_days;
