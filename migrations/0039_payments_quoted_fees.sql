-- A payment that waits for compliance with a quote keeps the quote's fees, so that it is not charged, once cleared,
-- more than its customer was shown (ADR-0016). They are set when the order is made and never change; null, it named
-- no quote (or was made before this migration) and is charged as it is cleared.

-- +goose Up
ALTER TABLE payments.orders ADD COLUMN quoted_fees jsonb CHECK (jsonb_typeof(quoted_fees) = 'array');

-- +goose StatementBegin
CREATE OR REPLACE FUNCTION payments.guard_order() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF TG_OP <> 'UPDATE' THEN
        RAISE EXCEPTION 'payment orders are never deleted or truncated'
            USING ERRCODE = 'prohibited_sql_statement_attempted', TABLE = 'orders';
    END IF;
    IF (NEW.id, NEW.legal_entity_id, NEW.direction, NEW.rail, NEW.end_to_end_id, NEW.instr_id, NEW.tx_id,
        NEW.currency, NEW.amount_units, NEW.creditor_number, NEW.creditor_key, NEW.external_ref, NEW.debtor_agent, NEW.created_at,
        NEW.quoted_fees)
        IS DISTINCT FROM (OLD.id, OLD.legal_entity_id, OLD.direction, OLD.rail, OLD.end_to_end_id, OLD.instr_id,
        OLD.tx_id, OLD.currency, OLD.amount_units, OLD.creditor_number, OLD.creditor_key, OLD.external_ref, OLD.debtor_agent,
        OLD.created_at, OLD.quoted_fees)
        OR (NEW.account_id IS DISTINCT FROM OLD.account_id
            AND NOT (OLD.direction = 'incoming' AND OLD.status = 'suspended' AND NEW.status = 'settled' AND NEW.account_id IS NOT NULL))
        OR (NOT (OLD.status = 'pending_screening' AND NEW.status = 'held')
            AND (NEW.hold_id, NEW.held_units, NEW.fees, NEW.gmf_units, NEW.debit_counted_on, NEW.gmf_exempt_units, NEW.gmf_exempt_month)
            IS DISTINCT FROM (OLD.hold_id, OLD.held_units, OLD.fees, OLD.gmf_units, OLD.debit_counted_on, OLD.gmf_exempt_units,
            OLD.gmf_exempt_month))
        OR (NEW.returned_at IS DISTINCT FROM OLD.returned_at
            AND NOT (OLD.returned_at IS NULL AND OLD.status <> 'returned' AND NEW.status = 'returned')) THEN
        RAISE EXCEPTION 'only where payment order % stands changes', OLD.id
            USING ERRCODE = 'prohibited_sql_statement_attempted', TABLE = 'orders';
    END IF;
    RETURN NEW;
END
$$;
-- +goose StatementEnd

GRANT INSERT (quoted_fees) ON payments.orders TO aluna_app;

-- +goose Down
REVOKE INSERT (quoted_fees) ON payments.orders FROM aluna_app;

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
            AND (NEW.hold_id, NEW.held_units, NEW.fees, NEW.gmf_units, NEW.debit_counted_on, NEW.gmf_exempt_units, NEW.gmf_exempt_month)
            IS DISTINCT FROM (OLD.hold_id, OLD.held_units, OLD.fees, OLD.gmf_units, OLD.debit_counted_on, OLD.gmf_exempt_units,
            OLD.gmf_exempt_month))
        OR (NEW.returned_at IS DISTINCT FROM OLD.returned_at
            AND NOT (OLD.returned_at IS NULL AND OLD.status <> 'returned' AND NEW.status = 'returned')) THEN
        RAISE EXCEPTION 'only where payment order % stands changes', OLD.id
            USING ERRCODE = 'prohibited_sql_statement_attempted', TABLE = 'orders';
    END IF;
    RETURN NEW;
END
$$;
-- +goose StatementEnd

ALTER TABLE payments.orders DROP COLUMN quoted_fees;
