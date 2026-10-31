-- Legal entities and their business date (FR-13, ADR-0006). Whoever operates the deployment registers
-- them; the application only reads the business date and advances it with the rollover. Postings read
-- it at the start of their transaction without locking the row, so the rollover waits for no one (QS-2).

-- +goose Up
CREATE TABLE calendar.legal_entities (
    id                  uuid PRIMARY KEY,
    name                text NOT NULL CHECK (name <> ''),
    timezone            text NOT NULL DEFAULT 'America/Bogota',
    business_date       date NOT NULL CHECK (isfinite(business_date)), -- 'infinity' is not a day
    functional_currency char(3) NOT NULL CHECK (functional_currency ~ '^[A-Z]{3}$'),
    created_at          timestamptz NOT NULL DEFAULT clock_timestamp()
);

-- The business date only moves forward: rewinding it would reopen days already closed.
-- +goose StatementBegin
CREATE FUNCTION calendar.forbid_business_date_rewind() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF NEW.business_date <= OLD.business_date THEN
        RAISE EXCEPTION 'the business date only moves forward'
            USING ERRCODE = 'prohibited_sql_statement_attempted', TABLE = 'legal_entities';
    END IF;
    RETURN NEW;
END
$$;
-- +goose StatementEnd

CREATE TRIGGER legal_entities_business_date_forward
    BEFORE UPDATE OF business_date ON calendar.legal_entities
    FOR EACH ROW EXECUTE FUNCTION calendar.forbid_business_date_rewind();

GRANT SELECT ON calendar.legal_entities TO aluna_app;
GRANT UPDATE (business_date) ON calendar.legal_entities TO aluna_app;

-- +goose Down
DROP TABLE calendar.legal_entities;
DROP FUNCTION calendar.forbid_business_date_rewind();
