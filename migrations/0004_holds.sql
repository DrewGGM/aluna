-- Holds de dos fases (FR-6, ADR-0003): una reserva de fondos sobre una cuenta sync de la entidad, que
-- termina en exactamente un estado: captured (con la transacción real que la asentó), released o
-- expired. Mientras está pending, su importe cuenta en el pendiente de la cuenta origen (lo suma y lo
-- resta el caso de uso en la misma transacción). Un hold terminado no cambia.

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
    capture_tx_id      uuid UNIQUE, -- una transacción asienta a lo sumo un hold
    created_at         timestamptz NOT NULL DEFAULT clock_timestamp(),
    -- origen y contrapartida de la misma entidad y en la moneda del hold, como los entries
    FOREIGN KEY (account_id, legal_entity_id, currency) REFERENCES ledger.accounts (id, legal_entity_id, currency),
    FOREIGN KEY (counter_account_id, legal_entity_id, currency) REFERENCES ledger.accounts (id, legal_entity_id, currency),
    FOREIGN KEY (capture_tx_id, legal_entity_id) REFERENCES ledger.transactions (id, legal_entity_id),
    CONSTRAINT holds_distinct_accounts CHECK (account_id <> counter_account_id),
    -- solo un hold captured tiene transacción e importe capturado, y los tiene los dos
    CONSTRAINT holds_capture_by_status CHECK (
        (status = 'captured') = (capture_tx_id IS NOT NULL)
        AND (status = 'captured') = (captured_amount > 0))
);

CREATE INDEX holds_pending_expiry_idx ON ledger.holds (expires_at) WHERE status = 'pending'; -- job de expiración (T-022)

-- Un hold solo sale de pending, una vez, y lo único que cambia es su resultado.
-- +goose StatementBegin
CREATE FUNCTION ledger.forbid_hold_change() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF OLD.status <> 'pending' OR NEW.status = 'pending' THEN
        RAISE EXCEPTION 'un hold solo sale de pending una vez'
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

-- Vaciar holds pending dejaría su importe para siempre en el pendiente de las cuentas.
CREATE TRIGGER holds_no_truncate
    BEFORE TRUNCATE ON ledger.holds
    FOR EACH STATEMENT EXECUTE FUNCTION ledger.forbid_change();

GRANT SELECT ON ledger.holds TO aluna_app;
GRANT INSERT (id, legal_entity_id, account_id, counter_account_id, currency, amount, expires_at) ON ledger.holds TO aluna_app;
GRANT UPDATE (status, captured_amount, capture_tx_id) ON ledger.holds TO aluna_app;

-- +goose Down
-- Borra los holds con sus datos: solo sirve en desarrollo y pruebas.
DROP TABLE ledger.holds;
DROP FUNCTION ledger.forbid_hold_change();
