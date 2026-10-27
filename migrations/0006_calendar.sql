-- Entidades legales y su fecha de negocio (FR-13, ADR-0006). Las registra quien opera el despliegue; la
-- aplicación solo lee la fecha de negocio y la avanza con el rollover. Los postings la leen al empezar
-- su transacción sin bloquear la fila, así que el rollover no espera a nadie (QS-2).

-- +goose Up
CREATE TABLE calendar.legal_entities (
    id                  uuid PRIMARY KEY,
    name                text NOT NULL CHECK (name <> ''),
    timezone            text NOT NULL DEFAULT 'America/Bogota',
    business_date       date NOT NULL CHECK (isfinite(business_date)), -- 'infinity' no es un día
    functional_currency char(3) NOT NULL CHECK (functional_currency ~ '^[A-Z]{3}$'),
    created_at          timestamptz NOT NULL DEFAULT clock_timestamp()
);

-- La fecha de negocio solo avanza: retrocederla reabriría días ya cerrados.
-- +goose StatementBegin
CREATE FUNCTION calendar.forbid_business_date_rewind() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF NEW.business_date <= OLD.business_date THEN
        RAISE EXCEPTION 'la fecha de negocio solo avanza'
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
