-- Products and their versions (FR-18, ADR-0008). A version is a definition that follows
-- deposit-product.v1.json; it goes from draft to pending when its publication is requested, to published
-- when someone else approves it, and to retired when a later version is published. Its definition is fixed
-- once its publication is requested: what is approved is what is published, and an account anchored to a
-- version keeps its conditions. Only the author of a version asks for its publication, so whoever approves
-- it is someone other than whoever wrote its conditions. Nothing is deleted.

-- +goose Up
CREATE TABLE products.products (
    id              uuid PRIMARY KEY,
    legal_entity_id uuid NOT NULL,
    code            text NOT NULL CHECK (code ~ '^[a-z][a-z0-9_]{0,63}$'),
    name            text NOT NULL CHECK (char_length(name) BETWEEN 1 AND 200),
    created_at      timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT products_code_unique UNIQUE (legal_entity_id, code)
);

CREATE TABLE products.versions (
    id           uuid PRIMARY KEY,
    product_id   uuid NOT NULL REFERENCES products.products (id),
    version      integer NOT NULL CHECK (version >= 1),
    definition   jsonb NOT NULL CHECK (jsonb_typeof(definition) = 'object'),
    created_by   text NOT NULL CHECK (created_by <> ''),
    status       text NOT NULL DEFAULT 'draft' CHECK (status IN ('draft', 'pending', 'published', 'retired')),
    created_at   timestamptz NOT NULL DEFAULT now(),
    published_at timestamptz,
    UNIQUE (product_id, version),
    CHECK ((status IN ('published', 'retired')) = (published_at IS NOT NULL))
);

-- A product has at most one version in force: the one new accounts are opened with.
CREATE UNIQUE INDEX versions_one_published ON products.versions (product_id) WHERE status = 'published';

-- +goose StatementBegin
CREATE FUNCTION products.forbid_change() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    RAISE EXCEPTION 'products are never updated or deleted'
        USING ERRCODE = 'prohibited_sql_statement_attempted', TABLE = TG_TABLE_NAME;
END
$$;

CREATE FUNCTION products.guard_version() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF TG_OP <> 'UPDATE' THEN
        RAISE EXCEPTION 'product versions are never deleted'
            USING ERRCODE = 'prohibited_sql_statement_attempted', TABLE = 'versions';
    END IF;
    IF (NEW.id, NEW.product_id, NEW.version, NEW.created_by, NEW.created_at)
        IS DISTINCT FROM (OLD.id, OLD.product_id, OLD.version, OLD.created_by, OLD.created_at) THEN
        RAISE EXCEPTION 'only the status of product version % changes', OLD.id
            USING ERRCODE = 'prohibited_sql_statement_attempted', TABLE = 'versions';
    END IF;
    IF NEW.definition IS DISTINCT FROM OLD.definition AND OLD.status <> 'draft' THEN
        RAISE EXCEPTION 'the definition of product version % is fixed once its publication is requested', OLD.id
            USING ERRCODE = 'prohibited_sql_statement_attempted', TABLE = 'versions';
    END IF;
    IF NEW.status IS DISTINCT FROM OLD.status
        AND (OLD.status, NEW.status) NOT IN (('draft', 'pending'), ('pending', 'published'), ('published', 'retired')) THEN
        RAISE EXCEPTION 'product version % cannot go from % to %', OLD.id, OLD.status, NEW.status
            USING ERRCODE = 'prohibited_sql_statement_attempted', TABLE = 'versions';
    END IF;
    IF NEW.published_at IS DISTINCT FROM OLD.published_at AND NOT (OLD.status = 'pending' AND NEW.status = 'published') THEN
        RAISE EXCEPTION 'the publication date of product version % is set once, when it is published', OLD.id
            USING ERRCODE = 'prohibited_sql_statement_attempted', TABLE = 'versions';
    END IF;
    RETURN NEW;
END
$$;
-- +goose StatementEnd

CREATE TRIGGER products_immutable BEFORE UPDATE OR DELETE ON products.products
    FOR EACH ROW EXECUTE FUNCTION products.forbid_change();
CREATE TRIGGER products_not_truncated BEFORE TRUNCATE ON products.products
    FOR EACH STATEMENT EXECUTE FUNCTION products.forbid_change();
CREATE TRIGGER versions_guarded BEFORE UPDATE OR DELETE ON products.versions
    FOR EACH ROW EXECUTE FUNCTION products.guard_version();
CREATE TRIGGER versions_not_truncated BEFORE TRUNCATE ON products.versions
    FOR EACH STATEMENT EXECUTE FUNCTION products.forbid_change();

GRANT SELECT ON products.products, products.versions TO aluna_app;
GRANT INSERT (id, legal_entity_id, code, name) ON products.products TO aluna_app;
GRANT INSERT (id, product_id, version, definition, created_by) ON products.versions TO aluna_app;
GRANT UPDATE (status, published_at) ON products.versions TO aluna_app;

-- +goose Down
DROP TABLE products.versions;
DROP TABLE products.products;
DROP FUNCTION products.guard_version();
DROP FUNCTION products.forbid_change();
