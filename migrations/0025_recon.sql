-- Continuous controls (FR-33): every run of a control over an entity, with how many findings it had, and the
-- breaks they open. A break stays open until it is resolved, so a run that finds it again opens no other: one
-- open break per entity, control and subject (a currency or a ledger account, depending on the control). Runs
-- are evidence and never change; neither runs nor breaks are deleted.

-- +goose Up
CREATE TABLE recon.control_runs (
    id              uuid PRIMARY KEY,
    legal_entity_id uuid NOT NULL,
    control         text NOT NULL CHECK (control IN
        ('trial_balance', 'balances_match_entries', 'rail_accounts_at_zero', 'customers_match_control')),
    findings        integer NOT NULL CHECK (findings >= 0),
    subjects        text[] NOT NULL DEFAULT '{}', -- of the findings, in a control that waits for a second run
    ran_at          timestamptz NOT NULL DEFAULT clock_timestamp()
);

CREATE INDEX control_runs_by_entity ON recon.control_runs (legal_entity_id, control, ran_at);

CREATE TABLE recon.breaks (
    id               uuid PRIMARY KEY,
    legal_entity_id  uuid NOT NULL,
    control          text NOT NULL CHECK (control IN
        ('trial_balance', 'balances_match_entries', 'rail_accounts_at_zero', 'customers_match_control')),
    subject          text NOT NULL CHECK (char_length(subject) BETWEEN 1 AND 64),
    currency         char(3) NOT NULL,
    difference_units bigint NOT NULL CHECK (difference_units <> 0), -- in minor units, as found when it opened
    status           text NOT NULL DEFAULT 'open' CHECK (status IN ('open', 'resolved')),
    opened_at        timestamptz NOT NULL DEFAULT clock_timestamp()
);

CREATE UNIQUE INDEX breaks_open_once ON recon.breaks (legal_entity_id, control, subject) WHERE status = 'open';

-- +goose StatementBegin
CREATE FUNCTION recon.forbid_change() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    RAISE EXCEPTION 'recon.% keeps its rows', TG_TABLE_NAME
        USING ERRCODE = 'prohibited_sql_statement_attempted', TABLE = TG_TABLE_NAME;
END
$$;
-- +goose StatementEnd

CREATE TRIGGER control_runs_unchanged BEFORE UPDATE OR DELETE ON recon.control_runs
    FOR EACH ROW EXECUTE FUNCTION recon.forbid_change();
CREATE TRIGGER control_runs_not_truncated BEFORE TRUNCATE ON recon.control_runs
    FOR EACH STATEMENT EXECUTE FUNCTION recon.forbid_change();
CREATE TRIGGER breaks_not_deleted BEFORE DELETE ON recon.breaks
    FOR EACH ROW EXECUTE FUNCTION recon.forbid_change();
CREATE TRIGGER breaks_not_truncated BEFORE TRUNCATE ON recon.breaks
    FOR EACH STATEMENT EXECUTE FUNCTION recon.forbid_change();

GRANT SELECT, INSERT (id, legal_entity_id, control, findings, subjects) ON recon.control_runs TO aluna_app;
GRANT SELECT, INSERT (id, legal_entity_id, control, subject, currency, difference_units) ON recon.breaks TO aluna_app;

-- +goose Down
DROP TABLE recon.breaks, recon.control_runs;
DROP FUNCTION recon.forbid_change();
