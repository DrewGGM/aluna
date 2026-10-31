-- Quotes of what an operation costs (FR-28): the fees shown to the customer before the operation, kept so
-- that the operation that uses the quote charges exactly that. A quote is used once and expires; nothing
-- else in it changes. The account is a deposit account, with no foreign key: pricing does not depend on
-- deposits (deposits depends on pricing).

-- +goose Up
CREATE TABLE pricing.quotes (
    id              uuid PRIMARY KEY,
    legal_entity_id uuid NOT NULL,
    account_id      uuid NOT NULL,
    operation       text NOT NULL CHECK (operation IN ('withdrawal', 'transfer')),
    currency        char(3) NOT NULL CHECK (currency ~ '^[A-Z]{3}$'),
    amount_units    bigint NOT NULL CHECK (amount_units > 0),
    fees            jsonb NOT NULL, -- [{"code": ..., "units": ...}] in the quote's currency
    -- statement_timestamp() is read once per statement: the INSERT computes expires_at from the same instant
    created_at      timestamptz NOT NULL DEFAULT statement_timestamp(),
    expires_at      timestamptz NOT NULL,
    used_at         timestamptz,
    CHECK (expires_at > created_at),
    CHECK (used_at IS NULL OR used_at >= created_at)
);

CREATE INDEX quotes_used_by_account ON pricing.quotes (account_id, used_at) WHERE used_at IS NOT NULL;

-- +goose StatementBegin
CREATE FUNCTION pricing.guard_quote() RETURNS trigger LANGUAGE plpgsql AS $$
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

CREATE TRIGGER quotes_guarded BEFORE UPDATE OR DELETE ON pricing.quotes
    FOR EACH ROW EXECUTE FUNCTION pricing.guard_quote();
CREATE TRIGGER quotes_not_truncated BEFORE TRUNCATE ON pricing.quotes
    FOR EACH STATEMENT EXECUTE FUNCTION pricing.guard_quote();

GRANT SELECT, INSERT (id, legal_entity_id, account_id, operation, currency, amount_units, fees, expires_at)
    ON pricing.quotes TO aluna_app;
GRANT UPDATE (used_at) ON pricing.quotes TO aluna_app;

-- +goose Down
DROP TABLE pricing.quotes;
DROP FUNCTION pricing.guard_quote();
