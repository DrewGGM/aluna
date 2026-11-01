-- Payment orders (FR-30, FR-31): each payment with its ISO 20022 identifiers, its end-to-end id unique, the
-- beneficiary by account or by key, and, once held, the hold on its account and the charges that hold covers.
-- The caller's reference (from its Idempotency-Key) is one payment per entity, so that a retry after the key
-- store forgot the key does not pay again. The deposit account has no foreign key: payments does not own them.
-- No order is deleted, and only where it stands changes (its status, the rail's reference and why it was
-- rejected): what it is, its identifiers and its amount do not, and what it holds is set once, when an order
-- that waited for compliance is held.

-- +goose Up
CREATE TABLE payments.orders (
    id              uuid PRIMARY KEY,
    legal_entity_id uuid NOT NULL,
    account_id      uuid NOT NULL,
    direction       text NOT NULL CHECK (direction IN ('outgoing', 'incoming')),
    rail            text NOT NULL CHECK (char_length(rail) BETWEEN 1 AND 32),
    end_to_end_id   text NOT NULL CHECK (char_length(end_to_end_id) BETWEEN 1 AND 35),
    instr_id        text NOT NULL CHECK (char_length(instr_id) BETWEEN 1 AND 35),
    tx_id           text NOT NULL CHECK (char_length(tx_id) BETWEEN 1 AND 35),
    currency        char(3) NOT NULL,
    amount_units    bigint NOT NULL CHECK (amount_units > 0),
    creditor_number text CHECK (char_length(creditor_number) BETWEEN 1 AND 64),
    creditor_key    text CHECK (char_length(creditor_key) BETWEEN 1 AND 128),
    status          text NOT NULL CHECK (status IN ('initiated', 'screening', 'pending_screening', 'held', 'sent', 'settled',
                                                    'rejected', 'returned')),
    reason          text CHECK (char_length(reason) BETWEEN 1 AND 255),
    rail_ref        text CHECK (char_length(rail_ref) BETWEEN 1 AND 255), -- the rail's reference, once it took the order
    external_ref    text NOT NULL CHECK (char_length(external_ref) BETWEEN 1 AND 255),
    hold_id         uuid,
    held_units      bigint NOT NULL DEFAULT 0 CHECK (held_units >= 0), -- the amount, its fees and its GMF
    fees            jsonb NOT NULL DEFAULT '[]' CHECK (jsonb_typeof(fees) = 'array'), -- [{"code", "units"}], posted at settlement
    gmf_units       bigint NOT NULL DEFAULT 0 CHECK (gmf_units >= 0),
    created_at      timestamptz NOT NULL DEFAULT clock_timestamp(),
    CONSTRAINT orders_end_to_end_id_unique UNIQUE (end_to_end_id),
    CONSTRAINT orders_external_ref_unique UNIQUE (legal_entity_id, external_ref),
    CONSTRAINT orders_one_creditor CHECK ((creditor_number IS NULL) <> (creditor_key IS NULL)),
    -- an outgoing order that went past screening has its hold; one waiting for compliance holds nothing (AB-6)
    CONSTRAINT orders_held CHECK (direction = 'incoming' OR status IN ('initiated', 'screening', 'pending_screening', 'rejected')
        OR hold_id IS NOT NULL),
    CONSTRAINT orders_pending_holds_nothing CHECK (status <> 'pending_screening'
        OR (hold_id IS NULL AND held_units = 0 AND gmf_units = 0 AND fees = '[]'))
);

-- +goose StatementBegin
CREATE FUNCTION payments.guard_order() RETURNS trigger LANGUAGE plpgsql AS $$
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

CREATE TRIGGER orders_guarded BEFORE UPDATE OR DELETE ON payments.orders
    FOR EACH ROW EXECUTE FUNCTION payments.guard_order();
CREATE TRIGGER orders_not_truncated BEFORE TRUNCATE ON payments.orders
    FOR EACH STATEMENT EXECUTE FUNCTION payments.guard_order();

GRANT SELECT, INSERT (id, legal_entity_id, account_id, direction, rail, end_to_end_id, instr_id, tx_id, currency, amount_units,
    creditor_number, creditor_key, status, reason, external_ref, hold_id, held_units, fees, gmf_units),
    UPDATE (status, reason, rail_ref, hold_id, held_units, fees, gmf_units) ON payments.orders TO aluna_app;

-- +goose Down
DROP TABLE payments.orders;
DROP FUNCTION payments.guard_order();
