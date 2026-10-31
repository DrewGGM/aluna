-- Market parameters (FR-21): series with a validity period of the usury rate by modality, the UVT, the
-- SMMLV, the UVB and the IBR by tenor. They are national, so they are the tenant's and not a legal
-- entity's. Each value holds from valid_from (included) to valid_to (excluded), and two values of the same
-- kind and modality never hold on the same day. A value is never changed or deleted: a correction is
-- another period.

-- +goose Up
CREATE EXTENSION IF NOT EXISTS btree_gist; -- text equality in the exclusion constraint

CREATE TABLE products.market_parameters (
    kind       text NOT NULL CHECK (kind IN ('usury_rate', 'uvt', 'smmlv', 'uvb', 'ibr')),
    modality   text NOT NULL CHECK (modality = '' OR modality ~ '^[a-z][a-z0-9_]{0,63}$'),
    valid_from date NOT NULL,
    valid_to   date NOT NULL,
    value      numeric NOT NULL CHECK (value > 0 AND value < 'Infinity' AND scale(value) <= 8), -- no NaN or Infinity
    created_at timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT market_parameters_pkey PRIMARY KEY (kind, modality, valid_from),
    CHECK (valid_from < valid_to),
    CONSTRAINT market_parameters_no_overlap
        EXCLUDE USING gist (kind WITH =, modality WITH =, daterange(valid_from, valid_to) WITH &&)
);

-- +goose StatementBegin
CREATE FUNCTION products.forbid_parameter_change() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    RAISE EXCEPTION 'market parameters are never updated or deleted: a correction is another period'
        USING ERRCODE = 'prohibited_sql_statement_attempted', TABLE = TG_TABLE_NAME;
END
$$;
-- +goose StatementEnd

CREATE TRIGGER market_parameters_immutable BEFORE UPDATE OR DELETE ON products.market_parameters
    FOR EACH ROW EXECUTE FUNCTION products.forbid_parameter_change();
CREATE TRIGGER market_parameters_not_truncated BEFORE TRUNCATE ON products.market_parameters
    FOR EACH STATEMENT EXECUTE FUNCTION products.forbid_parameter_change();

GRANT SELECT ON products.market_parameters TO aluna_app;
GRANT INSERT (kind, modality, valid_from, valid_to, value) ON products.market_parameters TO aluna_app;

-- +goose Down
DROP TABLE products.market_parameters;
DROP FUNCTION products.forbid_parameter_change();
-- btree_gist stays: it may have been there before, or another schema may use it.
