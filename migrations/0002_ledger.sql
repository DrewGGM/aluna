-- Double-entry ledger (ADR-0003). Amounts are signed BIGINT in minor units: debit positive, credit
-- negative. The database enforces the accounting invariants too, as defense in depth against a bug or
-- direct access with the application role: money is neither created nor destroyed, posted data never
-- changes and nobody moves another legal entity's accounts. The table owner can disable triggers; that
-- is reserved for audited operations.
--
-- Conventions for module migrations: triggers named <table>_<property>, functions named after what
-- they do, fixed SQLSTATEs the adapters recognize (23514 invariant, 2F003 forbidden change) and
-- explicit per-table GRANTs to aluna_app.

-- +goose Up
CREATE TABLE ledger.accounts (
    id              uuid PRIMARY KEY,
    legal_entity_id uuid NOT NULL,
    code            text NOT NULL,
    kind            text NOT NULL CHECK (kind IN ('asset', 'liability', 'equity', 'income', 'expense')),
    currency        char(3) NOT NULL CHECK (currency ~ '^[A-Z]{3}$'),
    balance_mode    text NOT NULL DEFAULT 'sync' CHECK (balance_mode IN ('sync', 'async')),
    lower_limit     bigint NOT NULL DEFAULT 0, -- in natural terms (see kind)
    owner_ref       text,
    created_at      timestamptz NOT NULL DEFAULT clock_timestamp(),
    UNIQUE (legal_entity_id, code),
    UNIQUE (id, legal_entity_id, currency), -- target of the entries FK
    CHECK (balance_mode = 'sync' OR owner_ref IS NULL) -- async accounts are internal, with no owner
);

CREATE TABLE ledger.transactions (
    id              uuid PRIMARY KEY, -- UUIDv7
    legal_entity_id uuid NOT NULL,
    code            text NOT NULL,
    value_date      date NOT NULL,
    business_date   date NOT NULL,
    posted_at       timestamptz NOT NULL, -- set by transactions_stamp with clock_timestamp()
    posted_xact     xid8 NOT NULL,        -- database transaction that created it (transactions_stamp)
    source          text NOT NULL,
    external_ref    text NOT NULL,
    actor           text NOT NULL,
    channel         text NOT NULL,
    rule_version    text NOT NULL,
    reverses_id     uuid,
    metadata        jsonb NOT NULL DEFAULT '{}',
    CONSTRAINT transactions_single_reversal UNIQUE (reverses_id), -- a transaction is reversed once
    CONSTRAINT transactions_idempotent_ref UNIQUE (legal_entity_id, source, external_ref), -- internal producers
    UNIQUE (id, legal_entity_id),
    -- only a transaction of the same entity can be reversed, and never by itself
    FOREIGN KEY (reverses_id, legal_entity_id) REFERENCES ledger.transactions (id, legal_entity_id),
    CONSTRAINT transactions_not_self_reversal CHECK (reverses_id <> id)
);

-- entries_stamp copies legal_entity_id, value_date and posted_at from the transaction: an entry cannot
-- end up at a different instant, value date or entity than its transaction.
CREATE TABLE ledger.entries (
    id              bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    transaction_id  uuid NOT NULL REFERENCES ledger.transactions (id),
    legal_entity_id uuid NOT NULL,
    account_id      uuid NOT NULL,
    currency        char(3) NOT NULL,
    amount          bigint NOT NULL CHECK (amount <> 0),
    value_date      date NOT NULL,
    posted_at       timestamptz NOT NULL,
    -- an entry is in its account's currency, and the account belongs to the transaction's entity
    FOREIGN KEY (account_id, legal_entity_id, currency) REFERENCES ledger.accounts (id, legal_entity_id, currency)
);
CREATE INDEX entries_transaction_idx ON ledger.entries (transaction_id);
CREATE INDEX entries_account_value_idx ON ledger.entries (account_id, value_date, posted_at); -- bitemporal balance (T-024)
CREATE INDEX entries_account_history_idx ON ledger.entries (account_id, posted_at, id);       -- paginated history (T-056)

-- version is incremented on every balance update (T-011).
CREATE TABLE ledger.balances (
    account_id      uuid PRIMARY KEY REFERENCES ledger.accounts (id),
    posted          bigint NOT NULL DEFAULT 0, -- Σ signed entries
    pending_debits  bigint NOT NULL DEFAULT 0 CHECK (pending_debits >= 0),
    pending_credits bigint NOT NULL DEFAULT 0 CHECK (pending_credits >= 0),
    version         bigint NOT NULL DEFAULT 0
);

-- +goose StatementBegin
CREATE FUNCTION ledger.stamp_transaction() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    NEW.posted_at := clock_timestamp();
    NEW.posted_xact := pg_current_xact_id();
    RETURN NEW;
END
$$;
-- +goose StatementEnd

-- An entry is only accepted in the same database transaction that created its transaction: a posted
-- transaction takes no new legs, not even balanced ones (a botched retry would duplicate the movement).
-- +goose StatementBegin
CREATE FUNCTION ledger.stamp_entry() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE
    parent ledger.transactions%ROWTYPE;
BEGIN
    SELECT * INTO parent FROM ledger.transactions WHERE id = NEW.transaction_id;
    IF NOT FOUND THEN
        RAISE EXCEPTION 'transaction % does not exist', NEW.transaction_id USING ERRCODE = 'foreign_key_violation';
    END IF;
    -- posted_at covers what xid8 does not: after a logical restore into another cluster the xid counter
    -- starts over and could repeat, but restored data always lies in the past.
    IF parent.posted_xact <> pg_current_xact_id() OR parent.posted_at < transaction_timestamp() THEN
        RAISE EXCEPTION 'transaction % is already posted: it accepts no new entries', NEW.transaction_id
            USING ERRCODE = 'prohibited_sql_statement_attempted';
    END IF;
    NEW.legal_entity_id := parent.legal_entity_id;
    NEW.value_date := parent.value_date;
    NEW.posted_at := parent.posted_at;
    RETURN NEW;
END
$$;
-- +goose StatementEnd

-- At COMMIT, the transaction has at least 2 entries and sums to zero in every currency. It is checked
-- from both tables: from transactions to catch one with no entries, and from entries because a session
-- can run the check early with SET CONSTRAINTS ALL IMMEDIATE and keep inserting legs; with the entries
-- trigger every later leg is checked again.
-- +goose StatementBegin
CREATE FUNCTION ledger.assert_balanced(tx uuid) RETURNS void LANGUAGE plpgsql AS $$
DECLARE
    legs       bigint;
    unbalanced boolean;
BEGIN
    SELECT coalesce(sum(n), 0), coalesce(bool_or(total <> 0), false) INTO legs, unbalanced
    FROM (SELECT count(*) AS n, sum(amount) AS total FROM ledger.entries
          WHERE transaction_id = tx GROUP BY currency) per_currency;
    IF legs < 2 THEN
        RAISE EXCEPTION 'transaction % has % entries; it needs at least 2', tx, legs
            USING ERRCODE = 'check_violation';
    END IF;
    IF unbalanced THEN
        RAISE EXCEPTION 'transaction % does not sum to zero in every currency', tx
            USING ERRCODE = 'check_violation';
    END IF;
END
$$;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE FUNCTION ledger.check_transaction_balanced() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    PERFORM ledger.assert_balanced(NEW.id);
    RETURN NULL;
END
$$;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE FUNCTION ledger.check_entry_balanced() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    PERFORM ledger.assert_balanced(NEW.transaction_id);
    RETURN NULL;
END
$$;
-- +goose StatementEnd

-- Posted data never changes: it is corrected with a reversal. TRUNCATE is handled separately because
-- it fires no row-level triggers; guarding entries suffices, since the FK forces truncating
-- transactions together with entries.
-- +goose StatementBegin
CREATE FUNCTION ledger.forbid_change() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    RAISE EXCEPTION 'ledger.% is insert-only: correct it with a reversal', TG_TABLE_NAME
        USING ERRCODE = 'prohibited_sql_statement_attempted';
END
$$;
-- +goose StatementEnd

CREATE TRIGGER transactions_stamp BEFORE INSERT ON ledger.transactions
    FOR EACH ROW EXECUTE FUNCTION ledger.stamp_transaction();
CREATE CONSTRAINT TRIGGER transactions_balanced AFTER INSERT ON ledger.transactions
    DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION ledger.check_transaction_balanced();
CREATE TRIGGER transactions_immutable BEFORE UPDATE OR DELETE ON ledger.transactions
    FOR EACH ROW EXECUTE FUNCTION ledger.forbid_change();
CREATE TRIGGER entries_stamp BEFORE INSERT ON ledger.entries
    FOR EACH ROW EXECUTE FUNCTION ledger.stamp_entry();
CREATE CONSTRAINT TRIGGER entries_balanced AFTER INSERT ON ledger.entries
    DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION ledger.check_entry_balanced();
CREATE TRIGGER entries_immutable BEFORE UPDATE OR DELETE ON ledger.entries
    FOR EACH ROW EXECUTE FUNCTION ledger.forbid_change();
CREATE TRIGGER entries_no_truncate BEFORE TRUNCATE ON ledger.entries
    FOR EACH STATEMENT EXECUTE FUNCTION ledger.forbid_change();

GRANT SELECT, INSERT ON ledger.accounts, ledger.transactions, ledger.entries TO aluna_app;
GRANT SELECT, INSERT, UPDATE ON ledger.balances TO aluna_app;
-- aluna_app was never granted UPDATE, DELETE or TRUNCATE on the journal; the REVOKE makes that explicit
-- and covers PUBLIC.
REVOKE UPDATE, DELETE, TRUNCATE ON ledger.transactions, ledger.entries FROM aluna_app, PUBLIC;

-- +goose Down
-- Drops the ledger with all its data: only for development and tests.
DROP TABLE ledger.balances, ledger.entries, ledger.transactions, ledger.accounts;
DROP FUNCTION ledger.stamp_transaction(), ledger.stamp_entry(), ledger.check_transaction_balanced(),
    ledger.check_entry_balanced(), ledger.assert_balanced(uuid), ledger.forbid_change();
