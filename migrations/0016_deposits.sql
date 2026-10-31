-- Deposit accounts (FR-22): a party's contract with a version of a product (FR-19), held in one ledger
-- account (liability). The account keeps its status; every change of status is recorded with who made it.
-- The foreign keys keep the party, the ledger account and its currency in the account's legal entity.

-- +goose Up
CREATE TABLE deposits.accounts (
    id                        uuid PRIMARY KEY,
    legal_entity_id           uuid NOT NULL,
    party_id                  uuid NOT NULL,
    product_version_id        uuid NOT NULL REFERENCES products.versions (id),
    ledger_account_id         uuid NOT NULL UNIQUE,
    currency                  char(3) NOT NULL,
    status                    text NOT NULL CHECK (status IN ('pending', 'active', 'frozen', 'dormant', 'closed')),
    opened_on                 date NOT NULL,
    last_customer_activity_on date NOT NULL,
    created_at                timestamptz NOT NULL DEFAULT clock_timestamp(),
    FOREIGN KEY (party_id, legal_entity_id) REFERENCES party.parties (id, legal_entity_id),
    FOREIGN KEY (ledger_account_id, legal_entity_id, currency) REFERENCES ledger.accounts (id, legal_entity_id, currency),
    CHECK (last_customer_activity_on >= opened_on)
);

CREATE TABLE deposits.status_changes (
    account_id  uuid NOT NULL REFERENCES deposits.accounts (id),
    from_status text NOT NULL CHECK (from_status IN ('pending', 'active', 'frozen', 'dormant', 'closed')),
    to_status   text NOT NULL CHECK (to_status IN ('pending', 'active', 'frozen', 'dormant', 'closed') AND to_status <> from_status),
    actor       text NOT NULL CHECK (actor <> ''),
    changed_at  timestamptz NOT NULL DEFAULT clock_timestamp()
);

-- +goose StatementBegin
CREATE FUNCTION deposits.guard_account() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF TG_OP <> 'UPDATE' THEN
        RAISE EXCEPTION 'deposit accounts are never deleted: they are closed'
            USING ERRCODE = 'prohibited_sql_statement_attempted', TABLE = 'accounts';
    END IF;
    IF (NEW.id, NEW.legal_entity_id, NEW.party_id, NEW.product_version_id, NEW.ledger_account_id, NEW.currency, NEW.opened_on, NEW.created_at)
        IS DISTINCT FROM (OLD.id, OLD.legal_entity_id, OLD.party_id, OLD.product_version_id, OLD.ledger_account_id, OLD.currency, OLD.opened_on, OLD.created_at) THEN
        RAISE EXCEPTION 'only the status and the last customer activity of deposit account % change', OLD.id
            USING ERRCODE = 'prohibited_sql_statement_attempted', TABLE = 'accounts';
    END IF;
    IF OLD.status = 'closed' THEN
        RAISE EXCEPTION 'deposit account % is closed', OLD.id
            USING ERRCODE = 'prohibited_sql_statement_attempted', TABLE = 'accounts';
    END IF;
    RETURN NEW;
END
$$;

CREATE FUNCTION deposits.forbid_change() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    RAISE EXCEPTION 'deposits.% is never updated, deleted or truncated', TG_TABLE_NAME
        USING ERRCODE = 'prohibited_sql_statement_attempted', TABLE = TG_TABLE_NAME;
END
$$;
-- +goose StatementEnd

CREATE TRIGGER accounts_guarded BEFORE UPDATE OR DELETE ON deposits.accounts
    FOR EACH ROW EXECUTE FUNCTION deposits.guard_account();
CREATE TRIGGER accounts_not_truncated BEFORE TRUNCATE ON deposits.accounts
    FOR EACH STATEMENT EXECUTE FUNCTION deposits.forbid_change();
CREATE TRIGGER status_changes_immutable BEFORE UPDATE OR DELETE ON deposits.status_changes
    FOR EACH ROW EXECUTE FUNCTION deposits.forbid_change();
CREATE TRIGGER status_changes_not_truncated BEFORE TRUNCATE ON deposits.status_changes
    FOR EACH STATEMENT EXECUTE FUNCTION deposits.forbid_change();

GRANT SELECT ON deposits.accounts, deposits.status_changes TO aluna_app;
GRANT INSERT (id, legal_entity_id, party_id, product_version_id, ledger_account_id, currency, status, opened_on,
    last_customer_activity_on) ON deposits.accounts TO aluna_app;
GRANT UPDATE (status, last_customer_activity_on) ON deposits.accounts TO aluna_app;
GRANT INSERT (account_id, from_status, to_status, actor) ON deposits.status_changes TO aluna_app;

-- +goose Down
DROP TABLE deposits.status_changes;
DROP TABLE deposits.accounts;
DROP FUNCTION deposits.forbid_change();
DROP FUNCTION deposits.guard_account();
