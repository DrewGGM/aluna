-- Accounting periods per entity (FR-15, ADR-0006): open → closing → closed. The ledger does not post with
-- a business date inside a closed period. A month with no row is open; the close (T-069) creates and
-- advances the rows, and until then the application only reads them.

-- +goose Up
CREATE TABLE calendar.periods (
    legal_entity_id uuid NOT NULL REFERENCES calendar.legal_entities (id),
    period          char(7) NOT NULL CHECK (period ~ '^[0-9]{4}-(0[1-9]|1[0-2])$'), -- yyyy-mm
    status          text NOT NULL DEFAULT 'open' CHECK (status IN ('open', 'closing', 'closed')),
    closed_at       timestamptz,
    closed_by       text,
    PRIMARY KEY (legal_entity_id, period),
    -- a closed period records when and by whom it was closed, and only a closed one records it
    CHECK ((status = 'closed') = (closed_at IS NOT NULL AND closed_by IS NOT NULL)),
    CHECK ((closed_at IS NULL) = (closed_by IS NULL))
);

GRANT SELECT ON calendar.periods TO aluna_app;

-- +goose Down
DROP TABLE calendar.periods;
