-- Cash totals (FR-43): what each party deposited and withdrew in cash in a month, in each currency, for the
-- UIAF report of wave 1. A total only grows and nothing is deleted. Its party has no foreign key: compliance
-- does not own parties.

-- +goose Up
CREATE TABLE compliance.cash_totals (
    legal_entity_id uuid NOT NULL,
    party_id        uuid NOT NULL,
    month           date NOT NULL CHECK (extract(day FROM month) = 1), -- the month's first day
    direction       text NOT NULL CHECK (direction IN ('deposit', 'withdrawal')),
    currency        char(3) NOT NULL,
    total_units     bigint NOT NULL CHECK (total_units > 0), -- in minor units of currency
    PRIMARY KEY (legal_entity_id, party_id, month, direction, currency)
);

-- +goose StatementBegin
CREATE FUNCTION compliance.guard_cash_total() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF TG_OP <> 'UPDATE' THEN
        RAISE EXCEPTION 'cash totals are never deleted or truncated'
            USING ERRCODE = 'prohibited_sql_statement_attempted', TABLE = 'cash_totals';
    END IF;
    IF (NEW.legal_entity_id, NEW.party_id, NEW.month, NEW.direction, NEW.currency)
            IS DISTINCT FROM (OLD.legal_entity_id, OLD.party_id, OLD.month, OLD.direction, OLD.currency)
        OR NEW.total_units < OLD.total_units THEN
        RAISE EXCEPTION 'the cash total of party % in % only grows', OLD.party_id, OLD.month
            USING ERRCODE = 'prohibited_sql_statement_attempted', TABLE = 'cash_totals';
    END IF;
    RETURN NEW;
END
$$;
-- +goose StatementEnd

CREATE TRIGGER cash_totals_guarded BEFORE UPDATE OR DELETE ON compliance.cash_totals
    FOR EACH ROW EXECUTE FUNCTION compliance.guard_cash_total();
CREATE TRIGGER cash_totals_not_truncated BEFORE TRUNCATE ON compliance.cash_totals
    FOR EACH STATEMENT EXECUTE FUNCTION compliance.guard_cash_total();

GRANT SELECT, INSERT (legal_entity_id, party_id, month, direction, currency, total_units), UPDATE (total_units)
    ON compliance.cash_totals TO aluna_app;

-- +goose Down
DROP TABLE compliance.cash_totals;
DROP FUNCTION compliance.guard_cash_total();
