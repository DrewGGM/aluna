-- A transfer held in suspense is credited, once someone finds whose it is, to the account found: its order goes
-- from suspended to settled and takes that account, the only time an order's account changes.

-- +goose Up
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

GRANT UPDATE (account_id) ON payments.orders TO aluna_app;

-- +goose Down
REVOKE UPDATE (account_id) ON payments.orders FROM aluna_app;

-- +goose StatementBegin
CREATE OR REPLACE FUNCTION payments.guard_order() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF TG_OP <> 'UPDATE' THEN
        RAISE EXCEPTION 'payment orders are never deleted or truncated'
            USING ERRCODE = 'prohibited_sql_statement_attempted', TABLE = 'orders';
    END IF;
    IF (NEW.id, NEW.legal_entity_id, NEW.account_id, NEW.direction, NEW.rail, NEW.end_to_end_id, NEW.instr_id, NEW.tx_id,
        NEW.currency, NEW.amount_units, NEW.creditor_number, NEW.creditor_key, NEW.external_ref, NEW.debtor_agent, NEW.created_at)
        IS DISTINCT FROM (OLD.id, OLD.legal_entity_id, OLD.account_id, OLD.direction, OLD.rail, OLD.end_to_end_id, OLD.instr_id,
        OLD.tx_id, OLD.currency, OLD.amount_units, OLD.creditor_number, OLD.creditor_key, OLD.external_ref, OLD.debtor_agent,
        OLD.created_at)
        OR (NOT (OLD.status = 'pending_screening' AND NEW.status = 'held')
            AND (NEW.hold_id, NEW.held_units, NEW.fees, NEW.gmf_units) IS DISTINCT FROM (OLD.hold_id, OLD.held_units, OLD.fees, OLD.gmf_units)) THEN
        RAISE EXCEPTION 'only where payment order % stands changes', OLD.id
            USING ERRCODE = 'prohibited_sql_statement_attempted', TABLE = 'orders';
    END IF;
    RETURN NEW;
END
$$;
-- +goose StatementEnd
