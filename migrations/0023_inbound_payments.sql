-- Transfers received (FR-32): an incoming order names the bank that sent it (its debtor agent), and its
-- end-to-end id is unique per direction and per that bank, the one that issued it, so that the same id from two
-- banks are two transfers and the same transfer twice is one. One for no account the core can credit has none
-- and is suspended, held in the rail's suspense account; the debtor agent, once set, does not change. A
-- transfer received is stored with the rail's reference.

-- +goose Up
ALTER TABLE payments.orders
    ALTER COLUMN account_id DROP NOT NULL,
    ADD COLUMN debtor_agent text CHECK (char_length(debtor_agent) BETWEEN 1 AND 35),
    DROP CONSTRAINT orders_end_to_end_id_unique,
    DROP CONSTRAINT orders_status_check,
    ADD CONSTRAINT orders_status_check CHECK (status IN ('initiated', 'screening', 'pending_screening', 'held', 'sent', 'settled',
                                                         'rejected', 'returned', 'suspended')),
    ADD CONSTRAINT orders_outgoing_account CHECK (direction = 'incoming' OR account_id IS NOT NULL),
    ADD CONSTRAINT orders_debtor_agent CHECK ((direction = 'incoming') = (debtor_agent IS NOT NULL));
-- end_to_end_id first: an outgoing outcome is looked up by it and its direction alone
CREATE UNIQUE INDEX orders_end_to_end_id_unique ON payments.orders (end_to_end_id, direction, coalesce(debtor_agent, ''));

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

GRANT INSERT (debtor_agent, rail_ref) ON payments.orders TO aluna_app; -- a transfer received comes with its reference

-- +goose Down
-- it fails while an incoming order exists without an account, suspended, or with the end-to-end id of another
-- order (an outgoing one or another bank's): those have no place before 0023
-- +goose StatementBegin
CREATE OR REPLACE FUNCTION payments.guard_order() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF TG_OP <> 'UPDATE' THEN
        RAISE EXCEPTION 'payment orders are never deleted or truncated'
            USING ERRCODE = 'prohibited_sql_statement_attempted', TABLE = 'orders';
    END IF;
    IF (NEW.id, NEW.legal_entity_id, NEW.account_id, NEW.direction, NEW.rail, NEW.end_to_end_id, NEW.instr_id, NEW.tx_id,
        NEW.currency, NEW.amount_units, NEW.creditor_number, NEW.creditor_key, NEW.external_ref, NEW.created_at)
        IS DISTINCT FROM (OLD.id, OLD.legal_entity_id, OLD.account_id, OLD.direction, OLD.rail, OLD.end_to_end_id, OLD.instr_id,
        OLD.tx_id, OLD.currency, OLD.amount_units, OLD.creditor_number, OLD.creditor_key, OLD.external_ref, OLD.created_at)
        OR (NOT (OLD.status = 'pending_screening' AND NEW.status = 'held')
            AND (NEW.hold_id, NEW.held_units, NEW.fees, NEW.gmf_units) IS DISTINCT FROM (OLD.hold_id, OLD.held_units, OLD.fees, OLD.gmf_units)) THEN
        RAISE EXCEPTION 'only where payment order % stands changes', OLD.id
            USING ERRCODE = 'prohibited_sql_statement_attempted', TABLE = 'orders';
    END IF;
    RETURN NEW;
END
$$;
-- +goose StatementEnd
REVOKE INSERT (rail_ref) ON payments.orders FROM aluna_app;
DROP INDEX payments.orders_end_to_end_id_unique;
ALTER TABLE payments.orders
    DROP CONSTRAINT orders_debtor_agent,
    DROP CONSTRAINT orders_outgoing_account,
    DROP COLUMN debtor_agent,
    ALTER COLUMN account_id SET NOT NULL,
    DROP CONSTRAINT orders_status_check,
    ADD CONSTRAINT orders_status_check CHECK (status IN ('initiated', 'screening', 'pending_screening', 'held', 'sent', 'settled',
                                                         'rejected', 'returned')),
    ADD CONSTRAINT orders_end_to_end_id_unique UNIQUE (end_to_end_id);
