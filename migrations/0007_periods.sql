-- Periodos contables por entidad (FR-15, ADR-0006): open → closing → closed. El ledger no asienta con
-- una fecha de negocio dentro de un periodo cerrado. Un mes sin fila está abierto; el cierre (T-069)
-- crea y avanza las filas, y hasta entonces la aplicación solo las lee.

-- +goose Up
CREATE TABLE calendar.periods (
    legal_entity_id uuid NOT NULL REFERENCES calendar.legal_entities (id),
    period          char(7) NOT NULL CHECK (period ~ '^[0-9]{4}-(0[1-9]|1[0-2])$'), -- yyyy-mm
    status          text NOT NULL DEFAULT 'open' CHECK (status IN ('open', 'closing', 'closed')),
    closed_at       timestamptz,
    closed_by       text,
    PRIMARY KEY (legal_entity_id, period),
    -- un periodo cerrado dice cuándo y quién lo cerró, y solo uno cerrado lo dice
    CHECK ((status = 'closed') = (closed_at IS NOT NULL AND closed_by IS NOT NULL)),
    CHECK ((closed_at IS NULL) = (closed_by IS NULL))
);

GRANT SELECT ON calendar.periods TO aluna_app;

-- +goose Down
DROP TABLE calendar.periods;
