-- CUIF mappings (FR-36, ADR-0011): which code of the regulator's catalog the internal accounts of an entity are, by
-- the start of their code, from valid_from (included) to valid_to (excluded). Two mappings of a pattern never hold
-- on the same day, and a mapping does not change: a correction is another period.

-- +goose Up
CREATE TABLE accounting.cuif_mappings (
    legal_entity_id uuid NOT NULL,
    pattern         text NOT NULL CHECK (pattern ~ '^[A-Za-z0-9][A-Za-z0-9._/-]{0,63}$'),
    cuif_code       text NOT NULL CHECK (cuif_code ~ '^[0-9]{6,10}$'),
    valid_from      date NOT NULL,
    valid_to        date NOT NULL,
    created_at      timestamptz NOT NULL DEFAULT clock_timestamp(),
    PRIMARY KEY (legal_entity_id, pattern, valid_from),
    CHECK (valid_from < valid_to),
    CONSTRAINT cuif_mappings_no_overlap
        EXCLUDE USING gist (legal_entity_id WITH =, pattern WITH =, daterange(valid_from, valid_to) WITH &&)
);

-- +goose StatementBegin
CREATE FUNCTION accounting.forbid_mapping_change() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    RAISE EXCEPTION 'a CUIF mapping is never changed or deleted: a correction is another period'
        USING ERRCODE = 'prohibited_sql_statement_attempted', TABLE = 'cuif_mappings';
END
$$;
-- +goose StatementEnd

CREATE TRIGGER cuif_mappings_unchanged BEFORE UPDATE OR DELETE ON accounting.cuif_mappings
    FOR EACH ROW EXECUTE FUNCTION accounting.forbid_mapping_change();
CREATE TRIGGER cuif_mappings_not_truncated BEFORE TRUNCATE ON accounting.cuif_mappings
    FOR EACH STATEMENT EXECUTE FUNCTION accounting.forbid_mapping_change();

GRANT SELECT, INSERT (legal_entity_id, pattern, cuif_code, valid_from, valid_to) ON accounting.cuif_mappings TO aluna_app;

-- +goose Down
DROP TABLE accounting.cuif_mappings;
DROP FUNCTION accounting.forbid_mapping_change();
