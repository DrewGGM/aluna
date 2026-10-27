-- Agregación de saldos de cuentas async (FR-9, ADR-0004): un posting no bloquea ni actualiza el saldo
-- de una cuenta async; el agregador suma después sus entries y guarda hasta dónde llegó.
--
-- El cursor es (posted_xact, entries.id) y no solo entries.id: los ids se asignan al insertar pero las
-- transacciones confirman en otro orden, así que un posting lento con un id menor confirmaría detrás
-- de un cursor por id y quedaría fuera del saldo para siempre. El agregador solo toma transacciones de
-- base anteriores a la más vieja en curso (pg_snapshot_xmin), que ya no pueden sumar entries; las que
-- confirmen después tienen un posted_xact mayor que el cursor.

-- +goose Up
CREATE TABLE ledger.async_cursor (
    singleton   boolean PRIMARY KEY DEFAULT true CHECK (singleton), -- una sola fila
    posted_xact xid8 NOT NULL DEFAULT '0',
    entry_id    bigint NOT NULL DEFAULT 0
);
INSERT INTO ledger.async_cursor DEFAULT VALUES;

CREATE INDEX transactions_posted_xact_idx ON ledger.transactions (posted_xact); -- lotes del agregador

-- El cursor solo avanza: retrocederlo volvería a sumar entries ya agregados.
-- +goose StatementBegin
CREATE FUNCTION ledger.forbid_cursor_rewind() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF (NEW.posted_xact, NEW.entry_id) <= (OLD.posted_xact, OLD.entry_id) THEN
        RAISE EXCEPTION 'el cursor de agregación solo avanza'
            USING ERRCODE = 'prohibited_sql_statement_attempted', TABLE = 'async_cursor';
    END IF;
    RETURN NEW;
END
$$;
-- +goose StatementEnd

CREATE TRIGGER async_cursor_forward
    BEFORE UPDATE ON ledger.async_cursor
    FOR EACH ROW EXECUTE FUNCTION ledger.forbid_cursor_rewind();

GRANT SELECT ON ledger.async_cursor TO aluna_app;
GRANT UPDATE (posted_xact, entry_id) ON ledger.async_cursor TO aluna_app;

-- +goose Down
DROP TABLE ledger.async_cursor;
DROP FUNCTION ledger.forbid_cursor_rewind();
DROP INDEX ledger.transactions_posted_xact_idx;
