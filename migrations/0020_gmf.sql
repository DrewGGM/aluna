-- The GMF (FR-29): the deposit accounts exempt from it and how much of its monthly exemption (350 UVT) a
-- party's exempt debits used, which only grows. A quote also keeps the GMF it showed and the part of the
-- exemption it uses, so that the operation that uses it is charged exactly that. As in pricing.quotes, the
-- accounts and parties have no foreign key: pricing does not depend on deposits or party.

-- +goose Up
CREATE TABLE pricing.gmf_exempt_accounts (
    account_id      uuid PRIMARY KEY,
    legal_entity_id uuid NOT NULL,
    party_id        uuid NOT NULL,
    marked_at       timestamptz NOT NULL DEFAULT clock_timestamp()
);

CREATE INDEX gmf_exempt_accounts_by_party ON pricing.gmf_exempt_accounts (legal_entity_id, party_id);

CREATE TABLE pricing.gmf_monthly (
    legal_entity_id   uuid NOT NULL,
    party_id          uuid NOT NULL,
    month             date NOT NULL CHECK (extract(day FROM month) = 1), -- the month's first day
    exempt_used_units bigint NOT NULL CHECK (exempt_used_units >= 0),   -- in COP minor units
    PRIMARY KEY (legal_entity_id, party_id, month)
);

ALTER TABLE pricing.quotes
    ADD COLUMN gmf_units bigint NOT NULL DEFAULT 0 CHECK (gmf_units >= 0),
    ADD COLUMN exempt_units bigint NOT NULL DEFAULT 0 CHECK (exempt_units BETWEEN 0 AND amount_units);

-- +goose StatementBegin
CREATE FUNCTION pricing.forbid_change() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    RAISE EXCEPTION 'pricing.% is never updated, deleted or truncated', TG_TABLE_NAME
        USING ERRCODE = 'prohibited_sql_statement_attempted', TABLE = TG_TABLE_NAME;
END
$$;

CREATE FUNCTION pricing.guard_gmf_monthly() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF TG_OP <> 'UPDATE' THEN
        RAISE EXCEPTION 'the GMF exemption used is never deleted or truncated'
            USING ERRCODE = 'prohibited_sql_statement_attempted', TABLE = 'gmf_monthly';
    END IF;
    IF (NEW.legal_entity_id, NEW.party_id, NEW.month) IS DISTINCT FROM (OLD.legal_entity_id, OLD.party_id, OLD.month)
        OR NEW.exempt_used_units < OLD.exempt_used_units THEN
        RAISE EXCEPTION 'the GMF exemption used of party % in % only grows', OLD.party_id, OLD.month
            USING ERRCODE = 'prohibited_sql_statement_attempted', TABLE = 'gmf_monthly';
    END IF;
    RETURN NEW;
END
$$;

CREATE OR REPLACE FUNCTION pricing.guard_quote() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF TG_OP <> 'UPDATE' THEN
        RAISE EXCEPTION 'quotes are never deleted or truncated'
            USING ERRCODE = 'prohibited_sql_statement_attempted', TABLE = 'quotes';
    END IF;
    IF OLD.used_at IS NOT NULL OR NEW.used_at IS NULL
        OR (NEW.id, NEW.legal_entity_id, NEW.account_id, NEW.operation, NEW.currency, NEW.amount_units, NEW.fees, NEW.gmf_units,
            NEW.exempt_units, NEW.created_at, NEW.expires_at)
            IS DISTINCT FROM (OLD.id, OLD.legal_entity_id, OLD.account_id, OLD.operation, OLD.currency, OLD.amount_units, OLD.fees,
            OLD.gmf_units, OLD.exempt_units, OLD.created_at, OLD.expires_at) THEN
        RAISE EXCEPTION 'a quote is only used, once: quote %', OLD.id
            USING ERRCODE = 'prohibited_sql_statement_attempted', TABLE = 'quotes';
    END IF;
    RETURN NEW;
END
$$;
-- +goose StatementEnd

CREATE TRIGGER gmf_exempt_accounts_unchanged BEFORE UPDATE OR DELETE ON pricing.gmf_exempt_accounts
    FOR EACH ROW EXECUTE FUNCTION pricing.forbid_change();
CREATE TRIGGER gmf_exempt_accounts_not_truncated BEFORE TRUNCATE ON pricing.gmf_exempt_accounts
    FOR EACH STATEMENT EXECUTE FUNCTION pricing.forbid_change();
CREATE TRIGGER gmf_monthly_guarded BEFORE UPDATE OR DELETE ON pricing.gmf_monthly
    FOR EACH ROW EXECUTE FUNCTION pricing.guard_gmf_monthly();
CREATE TRIGGER gmf_monthly_not_truncated BEFORE TRUNCATE ON pricing.gmf_monthly
    FOR EACH STATEMENT EXECUTE FUNCTION pricing.guard_gmf_monthly();

GRANT SELECT, INSERT (account_id, legal_entity_id, party_id) ON pricing.gmf_exempt_accounts TO aluna_app;
GRANT SELECT, INSERT (legal_entity_id, party_id, month, exempt_used_units), UPDATE (exempt_used_units)
    ON pricing.gmf_monthly TO aluna_app;
GRANT INSERT (gmf_units, exempt_units) ON pricing.quotes TO aluna_app;

-- +goose Down
-- +goose StatementBegin
CREATE OR REPLACE FUNCTION pricing.guard_quote() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF TG_OP <> 'UPDATE' THEN
        RAISE EXCEPTION 'quotes are never deleted or truncated'
            USING ERRCODE = 'prohibited_sql_statement_attempted', TABLE = 'quotes';
    END IF;
    IF OLD.used_at IS NOT NULL OR NEW.used_at IS NULL
        OR (NEW.id, NEW.legal_entity_id, NEW.account_id, NEW.operation, NEW.currency, NEW.amount_units, NEW.fees, NEW.created_at, NEW.expires_at)
            IS DISTINCT FROM (OLD.id, OLD.legal_entity_id, OLD.account_id, OLD.operation, OLD.currency, OLD.amount_units, OLD.fees, OLD.created_at, OLD.expires_at) THEN
        RAISE EXCEPTION 'a quote is only used, once: quote %', OLD.id
            USING ERRCODE = 'prohibited_sql_statement_attempted', TABLE = 'quotes';
    END IF;
    RETURN NEW;
END
$$;
-- +goose StatementEnd
ALTER TABLE pricing.quotes DROP COLUMN gmf_units, DROP COLUMN exempt_units;
DROP TABLE pricing.gmf_monthly;
DROP TABLE pricing.gmf_exempt_accounts;
DROP FUNCTION pricing.guard_gmf_monthly();
DROP FUNCTION pricing.forbid_change();
