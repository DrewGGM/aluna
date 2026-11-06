-- Rail statements (FR-34): each one goes in once, by its reference, and what does not match opens a break with
-- the statement and the reason. Every break has an owner, and it is resolved once, with a cause and by whom. A
-- statement never changes and, like a break, is never deleted. What a break resolves is its status, its cause
-- and who: the rest of it, and when, are not the caller's. The statement's lines are not kept: what matters of
-- them is in its breaks.

-- +goose Up
CREATE TABLE recon.statements (
    id              uuid PRIMARY KEY,
    legal_entity_id uuid NOT NULL,
    rail            text NOT NULL CHECK (char_length(rail) BETWEEN 1 AND 32),
    statement_ref   text NOT NULL CHECK (char_length(statement_ref) BETWEEN 1 AND 35),
    statement_date  date NOT NULL,
    line_count      integer NOT NULL CHECK (line_count >= 0),
    imported_by     text NOT NULL CHECK (char_length(imported_by) BETWEEN 1 AND 255),
    imported_at     timestamptz NOT NULL DEFAULT clock_timestamp(),
    CONSTRAINT statements_once UNIQUE (legal_entity_id, rail, statement_ref)
);

CREATE TRIGGER statements_unchanged BEFORE UPDATE OR DELETE ON recon.statements
    FOR EACH ROW EXECUTE FUNCTION recon.forbid_change();
CREATE TRIGGER statements_not_truncated BEFORE TRUNCATE ON recon.statements
    FOR EACH STATEMENT EXECUTE FUNCTION recon.forbid_change();

ALTER TABLE recon.breaks
    DROP CONSTRAINT breaks_control_check,
    ADD CONSTRAINT breaks_control_check CHECK (control IN
        ('trial_balance', 'balances_match_entries', 'rail_accounts_at_zero', 'customers_match_control', 'statement_match')),
    ADD COLUMN statement_id uuid REFERENCES recon.statements (id),
    ADD COLUMN reason       text CHECK (reason IN ('amount_mismatch', 'line_without_order', 'order_not_settled', 'order_without_line')),
    ADD COLUMN owner        text NOT NULL DEFAULT 'finance' CHECK (char_length(owner) BETWEEN 1 AND 255),
    ADD COLUMN cause        text CHECK (char_length(cause) BETWEEN 1 AND 1000),
    ADD COLUMN resolved_by  text CHECK (char_length(resolved_by) BETWEEN 1 AND 255),
    ADD COLUMN resolved_at  timestamptz,
    ADD CONSTRAINT breaks_of_a_statement CHECK ((control = 'statement_match') = (statement_id IS NOT NULL)
        AND (statement_id IS NULL) = (reason IS NULL)),
    ADD CONSTRAINT breaks_resolution CHECK ((status = 'resolved') = (cause IS NOT NULL) AND (cause IS NULL) = (resolved_by IS NULL));

CREATE INDEX breaks_by_entity ON recon.breaks (legal_entity_id, id DESC);

-- +goose StatementBegin
-- guard_break also stamps resolved_at: when a break was resolved is not what the caller says.
CREATE FUNCTION recon.guard_break() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF TG_OP <> 'UPDATE' THEN
        RAISE EXCEPTION 'recon.breaks keeps its rows'
            USING ERRCODE = 'prohibited_sql_statement_attempted', TABLE = 'breaks';
    END IF;
    IF OLD.status <> 'open' OR NEW.status <> 'resolved'
        OR (NEW.id, NEW.legal_entity_id, NEW.control, NEW.subject, NEW.currency, NEW.difference_units, NEW.opened_at, NEW.statement_id,
            NEW.reason, NEW.owner)
            IS DISTINCT FROM (OLD.id, OLD.legal_entity_id, OLD.control, OLD.subject, OLD.currency, OLD.difference_units, OLD.opened_at,
            OLD.statement_id, OLD.reason, OLD.owner) THEN
        RAISE EXCEPTION 'a break is only resolved, once: break %', OLD.id
            USING ERRCODE = 'prohibited_sql_statement_attempted', TABLE = 'breaks';
    END IF;
    NEW.resolved_at := clock_timestamp();
    RETURN NEW;
END
$$;
-- +goose StatementEnd

DROP TRIGGER breaks_not_deleted ON recon.breaks;
CREATE TRIGGER breaks_guarded BEFORE UPDATE OR DELETE ON recon.breaks
    FOR EACH ROW EXECUTE FUNCTION recon.guard_break();

GRANT SELECT, INSERT (id, legal_entity_id, rail, statement_ref, statement_date, line_count, imported_by) ON recon.statements TO aluna_app;
GRANT INSERT (statement_id, reason, owner), UPDATE (status, cause, resolved_by) ON recon.breaks TO aluna_app;

-- +goose Down
-- +goose StatementBegin
-- A resolved break or a statement fits nowhere in the old tables, and neither is ever deleted.
DO $$
BEGIN
    IF EXISTS (SELECT FROM recon.breaks WHERE status = 'resolved') OR EXISTS (SELECT FROM recon.statements) THEN
        RAISE EXCEPTION 'recon has statements or resolved breaks';
    END IF;
END
$$;
-- +goose StatementEnd

REVOKE INSERT (statement_id, reason, owner), UPDATE (status, cause, resolved_by) ON recon.breaks FROM aluna_app;
DROP TRIGGER breaks_guarded ON recon.breaks;
CREATE TRIGGER breaks_not_deleted BEFORE DELETE ON recon.breaks
    FOR EACH ROW EXECUTE FUNCTION recon.forbid_change();
DROP FUNCTION recon.guard_break();
DROP INDEX recon.breaks_by_entity;
ALTER TABLE recon.breaks
    DROP CONSTRAINT breaks_resolution,
    DROP CONSTRAINT breaks_of_a_statement,
    DROP COLUMN resolved_at,
    DROP COLUMN resolved_by,
    DROP COLUMN cause,
    DROP COLUMN owner,
    DROP COLUMN reason,
    DROP COLUMN statement_id,
    DROP CONSTRAINT breaks_control_check,
    ADD CONSTRAINT breaks_control_check CHECK (control IN
        ('trial_balance', 'balances_match_entries', 'rail_accounts_at_zero', 'customers_match_control'));
DROP TABLE recon.statements;
