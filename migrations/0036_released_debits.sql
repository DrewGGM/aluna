-- A payment the rail rejects never took its money, so the debit it counted against its account's limits when it
-- was held is released. The counters still only grow: what is released is a column of its own, never above the
-- debits, and what the account used is its debits less what was released. The order keeps the business date it
-- counted on, to release the debit from that day's and that month's counters; an order already held when this
-- runs has none, and keeps its debit counted if the rail rejects it.

-- +goose Up
ALTER TABLE deposits.counters
    ADD COLUMN released_debits bigint NOT NULL DEFAULT 0 CHECK (released_debits >= 0),
    ADD CONSTRAINT counters_released_within_debits CHECK (released_debits <= debits);

-- +goose StatementBegin
CREATE OR REPLACE FUNCTION deposits.guard_counter() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF TG_OP <> 'UPDATE' THEN
        RAISE EXCEPTION 'the counters of deposit accounts are never deleted or truncated'
            USING ERRCODE = 'prohibited_sql_statement_attempted', TABLE = 'counters';
    END IF;
    IF (NEW.account_id, NEW.period) IS DISTINCT FROM (OLD.account_id, OLD.period)
        OR NEW.debits < OLD.debits OR NEW.credits < OLD.credits OR NEW.released_debits < OLD.released_debits THEN
        RAISE EXCEPTION 'the counters of deposit account % only grow', OLD.account_id
            USING ERRCODE = 'prohibited_sql_statement_attempted', TABLE = 'counters';
    END IF;
    RETURN NEW;
END
$$;
-- +goose StatementEnd

GRANT UPDATE (released_debits) ON deposits.counters TO aluna_app;

ALTER TABLE payments.orders ADD COLUMN debit_counted_on date;

-- +goose StatementBegin
CREATE OR REPLACE FUNCTION payments.guard_order() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF TG_OP <> 'UPDATE' THEN
        RAISE EXCEPTION 'payment orders are never deleted or truncated'
            USING ERRCODE = 'prohibited_sql_statement_attempted', TABLE = 'orders';
    END IF;
    IF (NEW.id, NEW.legal_entity_id, NEW.direction, NEW.rail, NEW.end_to_end_id, NEW.instr_id, NEW.tx_id,
        NEW.currency, NEW.amount_units, NEW.creditor_number, NEW.creditor_key, NEW.external_ref, NEW.debtor_agent, NEW.created_at)
        IS DISTINCT FROM (OLD.id, OLD.legal_entity_id, OLD.direction, OLD.rail, OLD.end_to_end_id, OLD.instr_id,
        OLD.tx_id, OLD.currency, OLD.amount_units, OLD.creditor_number, OLD.creditor_key, OLD.external_ref, OLD.debtor_agent,
        OLD.created_at)
        OR (NEW.account_id IS DISTINCT FROM OLD.account_id
            AND NOT (OLD.direction = 'incoming' AND OLD.status = 'suspended' AND NEW.status = 'settled' AND NEW.account_id IS NOT NULL))
        OR (NOT (OLD.status = 'pending_screening' AND NEW.status = 'held')
            AND (NEW.hold_id, NEW.held_units, NEW.fees, NEW.gmf_units, NEW.debit_counted_on)
            IS DISTINCT FROM (OLD.hold_id, OLD.held_units, OLD.fees, OLD.gmf_units, OLD.debit_counted_on)) THEN
        RAISE EXCEPTION 'only where payment order % stands changes', OLD.id
            USING ERRCODE = 'prohibited_sql_statement_attempted', TABLE = 'orders';
    END IF;
    RETURN NEW;
END
$$;
-- +goose StatementEnd

GRANT INSERT (debit_counted_on), UPDATE (debit_counted_on) ON payments.orders TO aluna_app;

-- +goose Down
REVOKE INSERT (debit_counted_on), UPDATE (debit_counted_on) ON payments.orders FROM aluna_app;

-- +goose StatementBegin
CREATE OR REPLACE FUNCTION payments.guard_order() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF TG_OP <> 'UPDATE' THEN
        RAISE EXCEPTION 'payment orders are never deleted or truncated'
            USING ERRCODE = 'prohibited_sql_statement_attempted', TABLE = 'orders';
    END IF;
    IF (NEW.id, NEW.legal_entity_id, NEW.direction, NEW.rail, NEW.end_to_end_id, NEW.instr_id, NEW.tx_id,
        NEW.currency, NEW.amount_units, NEW.creditor_number, NEW.creditor_key, NEW.external_ref, NEW.debtor_agent, NEW.created_at)
        IS DISTINCT FROM (OLD.id, OLD.legal_entity_id, OLD.direction, OLD.rail, OLD.end_to_end_id, OLD.instr_id,
        OLD.tx_id, OLD.currency, OLD.amount_units, OLD.creditor_number, OLD.creditor_key, OLD.external_ref, OLD.debtor_agent,
        OLD.created_at)
        OR (NEW.account_id IS DISTINCT FROM OLD.account_id
            AND NOT (OLD.direction = 'incoming' AND OLD.status = 'suspended' AND NEW.status = 'settled' AND NEW.account_id IS NOT NULL))
        OR (NOT (OLD.status = 'pending_screening' AND NEW.status = 'held')
            AND (NEW.hold_id, NEW.held_units, NEW.fees, NEW.gmf_units) IS DISTINCT FROM (OLD.hold_id, OLD.held_units, OLD.fees, OLD.gmf_units)) THEN
        RAISE EXCEPTION 'only where payment order % stands changes', OLD.id
            USING ERRCODE = 'prohibited_sql_statement_attempted', TABLE = 'orders';
    END IF;
    RETURN NEW;
END
$$;
-- +goose StatementEnd

ALTER TABLE payments.orders DROP COLUMN debit_counted_on;

REVOKE UPDATE (released_debits) ON deposits.counters FROM aluna_app;

-- +goose StatementBegin
CREATE OR REPLACE FUNCTION deposits.guard_counter() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF TG_OP <> 'UPDATE' THEN
        RAISE EXCEPTION 'the counters of deposit accounts are never deleted or truncated'
            USING ERRCODE = 'prohibited_sql_statement_attempted', TABLE = 'counters';
    END IF;
    IF (NEW.account_id, NEW.period) IS DISTINCT FROM (OLD.account_id, OLD.period)
        OR NEW.debits < OLD.debits OR NEW.credits < OLD.credits THEN
        RAISE EXCEPTION 'the counters of deposit account % only grow', OLD.account_id
            USING ERRCODE = 'prohibited_sql_statement_attempted', TABLE = 'counters';
    END IF;
    RETURN NEW;
END
$$;
-- +goose StatementEnd

ALTER TABLE deposits.counters DROP CONSTRAINT counters_released_within_debits, DROP COLUMN released_debits;
