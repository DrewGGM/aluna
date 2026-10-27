-- Ledger de doble partida (ADR-0003). Los importes son BIGINT con signo en unidades menores: débito
-- positivo, crédito negativo. Las invariantes contables también las hace cumplir la base, como defensa
-- en profundidad ante un bug o un acceso directo con el rol de la aplicación: el dinero no se crea ni
-- se destruye, lo asentado no cambia y nadie mueve cuentas de otra entidad legal. El dueño de las
-- tablas puede desactivar triggers; eso queda para operaciones auditadas.
--
-- Convenciones para las migraciones de los módulos: triggers <tabla>_<propiedad>, funciones con el
-- nombre de lo que hacen, SQLSTATE fijos que los adapters reconocen (23514 invariante, 2F003 cambio
-- prohibido) y GRANT explícitos a aluna_app por tabla.

-- +goose Up
CREATE TABLE ledger.accounts (
    id              uuid PRIMARY KEY,
    legal_entity_id uuid NOT NULL,
    code            text NOT NULL,
    kind            text NOT NULL CHECK (kind IN ('asset', 'liability', 'equity', 'income', 'expense')),
    currency        char(3) NOT NULL CHECK (currency ~ '^[A-Z]{3}$'),
    balance_mode    text NOT NULL DEFAULT 'sync' CHECK (balance_mode IN ('sync', 'async')),
    lower_limit     bigint NOT NULL DEFAULT 0, -- en términos naturales (ver kind)
    owner_ref       text,
    created_at      timestamptz NOT NULL DEFAULT clock_timestamp(),
    UNIQUE (legal_entity_id, code),
    UNIQUE (id, legal_entity_id, currency), -- destino de la FK de entries
    CHECK (balance_mode = 'sync' OR owner_ref IS NULL) -- las cuentas async son internas, sin dueño
);

CREATE TABLE ledger.transactions (
    id              uuid PRIMARY KEY, -- UUIDv7
    legal_entity_id uuid NOT NULL,
    code            text NOT NULL,
    value_date      date NOT NULL,
    business_date   date NOT NULL,
    posted_at       timestamptz NOT NULL, -- lo fija transactions_stamp con clock_timestamp()
    posted_xact     xid8 NOT NULL,        -- transacción de base que la creó (transactions_stamp)
    source          text NOT NULL,
    external_ref    text NOT NULL,
    actor           text NOT NULL,
    channel         text NOT NULL,
    rule_version    text NOT NULL,
    reverses_id     uuid,
    metadata        jsonb NOT NULL DEFAULT '{}',
    CONSTRAINT transactions_single_reversal UNIQUE (reverses_id), -- una transacción se revierte una vez
    CONSTRAINT transactions_idempotent_ref UNIQUE (legal_entity_id, source, external_ref), -- productores internos
    UNIQUE (id, legal_entity_id),
    -- solo se revierte una transacción de la misma entidad, y nunca a sí misma
    FOREIGN KEY (reverses_id, legal_entity_id) REFERENCES ledger.transactions (id, legal_entity_id),
    CONSTRAINT transactions_not_self_reversal CHECK (reverses_id <> id)
);

-- legal_entity_id, value_date y posted_at los copia entries_stamp de la transacción: un entry no puede
-- quedar en otro instante, otra fecha valor u otra entidad que su transacción.
CREATE TABLE ledger.entries (
    id              bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    transaction_id  uuid NOT NULL REFERENCES ledger.transactions (id),
    legal_entity_id uuid NOT NULL,
    account_id      uuid NOT NULL,
    currency        char(3) NOT NULL,
    amount          bigint NOT NULL CHECK (amount <> 0),
    value_date      date NOT NULL,
    posted_at       timestamptz NOT NULL,
    -- un entry va en la moneda de su cuenta, y la cuenta es de la entidad de la transacción
    FOREIGN KEY (account_id, legal_entity_id, currency) REFERENCES ledger.accounts (id, legal_entity_id, currency)
);
CREATE INDEX entries_transaction_idx ON ledger.entries (transaction_id);
CREATE INDEX entries_account_value_idx ON ledger.entries (account_id, value_date, posted_at); -- saldo bitemporal (T-024)
CREATE INDEX entries_account_history_idx ON ledger.entries (account_id, posted_at, id);       -- movimientos paginados (T-056)

-- version se incrementa en cada actualización del saldo (T-011).
CREATE TABLE ledger.balances (
    account_id      uuid PRIMARY KEY REFERENCES ledger.accounts (id),
    posted          bigint NOT NULL DEFAULT 0, -- Σ entries con signo
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

-- Un entry solo entra en la misma transacción de base que creó su transacción: lo asentado no recibe
-- legs nuevos, ni siquiera balanceados (un reintento mal hecho duplicaría el movimiento).
-- +goose StatementBegin
CREATE FUNCTION ledger.stamp_entry() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE
    parent ledger.transactions%ROWTYPE;
BEGIN
    SELECT * INTO parent FROM ledger.transactions WHERE id = NEW.transaction_id;
    IF NOT FOUND THEN
        RAISE EXCEPTION 'la transacción % no existe', NEW.transaction_id USING ERRCODE = 'foreign_key_violation';
    END IF;
    -- posted_at cubre lo que xid8 no: tras una restauración lógica en otro clúster el contador de xid
    -- vuelve a empezar y podría repetirse, pero lo restaurado siempre quedó en el pasado.
    IF parent.posted_xact <> pg_current_xact_id() OR parent.posted_at < transaction_timestamp() THEN
        RAISE EXCEPTION 'la transacción % ya está asentada: no admite entries nuevos', NEW.transaction_id
            USING ERRCODE = 'prohibited_sql_statement_attempted';
    END IF;
    NEW.legal_entity_id := parent.legal_entity_id;
    NEW.value_date := parent.value_date;
    NEW.posted_at := parent.posted_at;
    RETURN NEW;
END
$$;
-- +goose StatementEnd

-- Al COMMIT, la transacción tiene al menos 2 entries y suma cero en cada moneda. Se revisa desde las
-- dos tablas: desde transactions para atrapar la que no tiene entries, y desde entries porque una
-- sesión puede adelantar el chequeo con SET CONSTRAINTS ALL IMMEDIATE y seguir insertando legs; con el
-- trigger de entries cada leg posterior se vuelve a revisar.
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
        RAISE EXCEPTION 'la transacción % tiene % entries; necesita al menos 2', tx, legs
            USING ERRCODE = 'check_violation';
    END IF;
    IF unbalanced THEN
        RAISE EXCEPTION 'la transacción % no suma cero en todas sus monedas', tx USING ERRCODE = 'check_violation';
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

-- Lo asentado no cambia: se corrige con una reversa. TRUNCATE va aparte porque no dispara triggers por
-- fila; basta en entries, porque la FK obliga a truncar transactions junto con entries.
-- +goose StatementBegin
CREATE FUNCTION ledger.forbid_change() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    RAISE EXCEPTION 'ledger.% es de solo inserción: se corrige con una reversa', TG_TABLE_NAME
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
-- aluna_app nunca recibió UPDATE, DELETE ni TRUNCATE en el journal; el REVOKE lo deja escrito y cubre
-- a PUBLIC.
REVOKE UPDATE, DELETE, TRUNCATE ON ledger.transactions, ledger.entries FROM aluna_app, PUBLIC;

-- +goose Down
-- Borra el ledger con todos sus datos: solo sirve en desarrollo y pruebas.
DROP TABLE ledger.balances, ledger.entries, ledger.transactions, ledger.accounts;
DROP FUNCTION ledger.stamp_transaction(), ledger.stamp_entry(), ledger.check_transaction_balanced(),
    ledger.check_entry_balanced(), ledger.assert_balanced(uuid), ledger.forbid_change();
