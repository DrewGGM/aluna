-- Two-phase holds (FR-6, ADR-0003): a reservation of funds on one of the entity's sync accounts that
-- ends in exactly one state: captured (with the actual transaction that posted it), released or
-- expired. While pending, its amount counts toward the source account's pending balance (the use case
-- adds and subtracts it in the same transaction). A finished hold never changes.

-- +goose Up
CREATE TABLE ledger.holds (
    id                 uuid PRIMARY KEY, -- UUIDv7
    legal_entity_id    uuid NOT NULL,
    account_id         uuid NOT NULL,
    counter_account_id uuid NOT NULL,
    currency           char(3) NOT NULL,
    amount             bigint NOT NULL CHECK (amount > 0),
    captured_amount    bigint NOT NULL DEFAULT 0 CHECK (captured_amount BETWEEN 0 AND amount),
    status             text NOT NULL DEFAULT 'pending'
                           CHECK (status IN ('pending', 'captured', 'released', 'expired')),
    expires_at         timestamptz NOT NULL,
    capture_tx_id      uuid UNIQUE, -- a transaction posts at most one hold
    created_at         timestamptz NOT NULL DEFAULT clock_timestamp(),
    -- source and counter account of the same entity and in the hold's currency, like entries
    FOREIGN KEY (account_id, legal_entity_id, currency) REFERENCES ledger.accounts (id, legal_entity_id, currency),
    FOREIGN KEY (counter_account_id, legal_entity_id, currency) REFERENCES ledger.accounts (id, legal_entity_id, currency),
    FOREIGN KEY (capture_tx_id, legal_entity_id) REFERENCES ledger.transactions (id, legal_entity_id),
    CONSTRAINT holds_distinct_accounts CHECK (account_id <> counter_account_id),
    -- only a captured hold has a transaction and a captured amount, and it has both
    CONSTRAINT holds_capture_by_status CHECK (
        (status = 'captured') = (capture_tx_id IS NOT NULL)
        AND (status = 'captured') = (captured_amount > 0))
);

CREATE INDEX holds_pending_expiry_idx ON ledger.holds (expires_at) WHERE status = 'pending'; -- expiry job (T-022)

-- A hold only leaves pending, once, and the only thing that changes is its outcome.
-- +goose StatementBegin
CREATE FUNCTION ledger.forbid_hold_change() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF OLD.status <> 'pending' OR NEW.status = 'pending' THEN
        RAISE EXCEPTION 'a hold leaves pending only once'
            USING ERRCODE = 'prohibited_sql_statement_attempted', TABLE = 'holds';
    END IF;
    RETURN NEW;
END
$$;
-- +goose StatementEnd

CREATE TRIGGER holds_terminal_once
    BEFORE UPDATE ON ledger.holds
    FOR EACH ROW EXECUTE FUNCTION ledger.forbid_hold_change();

CREATE TRIGGER holds_no_delete
    BEFORE DELETE ON ledger.holds
    FOR EACH ROW EXECUTE FUNCTION ledger.forbid_change();

-- Truncating pending holds would leave their amounts in the accounts' pending balances forever.
CREATE TRIGGER holds_no_truncate
    BEFORE TRUNCATE ON ledger.holds
    FOR EACH STATEMENT EXECUTE FUNCTION ledger.forbid_change();

GRANT SELECT ON ledger.holds TO aluna_app;
GRANT INSERT (id, legal_entity_id, account_id, counter_account_id, currency, amount, expires_at) ON ledger.holds TO aluna_app;
GRANT UPDATE (status, captured_amount, capture_tx_id) ON ledger.holds TO aluna_app;

-- +goose Down
-- Drops the holds with their data: only for development and tests.
DROP TABLE ledger.holds;
DROP FUNCTION ledger.forbid_hold_change();
