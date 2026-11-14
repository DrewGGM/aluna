-- A payment the rail rejects never debited its GMF, and one its beneficiary's bank returns for a technical reason
-- was annulled (ADR-0016): what the payment used of its party's monthly GMF exemption is restored while it is still
-- that month. The exemption used still only grows: what is restored is a column of its own, never above what was
-- used, and what the party used is the difference. The order keeps what it used and of which month, set when it is
-- held, as the rest of what it holds.

-- +goose Up
ALTER TABLE pricing.gmf_monthly
    ADD COLUMN restored_units bigint NOT NULL DEFAULT 0 CHECK (restored_units >= 0),
    ADD CONSTRAINT gmf_monthly_restored_within_used CHECK (restored_units <= exempt_used_units);

-- +goose StatementBegin
CREATE OR REPLACE FUNCTION pricing.guard_gmf_monthly() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF TG_OP <> 'UPDATE' THEN
        RAISE EXCEPTION 'the GMF exemption used is never deleted or truncated'
            USING ERRCODE = 'prohibited_sql_statement_attempted', TABLE = 'gmf_monthly';
    END IF;
    IF (NEW.legal_entity_id, NEW.party_id, NEW.month) IS DISTINCT FROM (OLD.legal_entity_id, OLD.party_id, OLD.month)
        OR NEW.exempt_used_units < OLD.exempt_used_units OR NEW.restored_units < OLD.restored_units THEN
        RAISE EXCEPTION 'the GMF exemption used of party % in % only grows', OLD.party_id, OLD.month
            USING ERRCODE = 'prohibited_sql_statement_attempted', TABLE = 'gmf_monthly';
    END IF;
    RETURN NEW;
END
$$;
-- +goose StatementEnd

GRANT UPDATE (restored_units) ON pricing.gmf_monthly TO aluna_app;

ALTER TABLE payments.orders
    ADD COLUMN gmf_exempt_units bigint NOT NULL DEFAULT 0 CHECK (gmf_exempt_units >= 0),
    ADD COLUMN gmf_exempt_month date CHECK (extract(day FROM gmf_exempt_month) = 1),
    ADD CONSTRAINT orders_exempt_of_a_month CHECK ((gmf_exempt_units = 0) = (gmf_exempt_month IS NULL));

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

GRANT INSERT (gmf_exempt_units, gmf_exempt_month), UPDATE (gmf_exempt_units, gmf_exempt_month) ON payments.orders TO aluna_app;

-- +goose Down
REVOKE INSERT (gmf_exempt_units, gmf_exempt_month), UPDATE (gmf_exempt_units, gmf_exempt_month) ON payments.orders FROM aluna_app;

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
            IS DISTINCT FROM (OLD.hold_id, OLD.held_units, OLD.fees, OLD.gmf_units, OLD.debit_counted_on))
        OR (NEW.returned_at IS DISTINCT FROM OLD.returned_at
            AND NOT (OLD.returned_at IS NULL AND OLD.status <> 'returned' AND NEW.status = 'returned')) THEN
        RAISE EXCEPTION 'only where payment order % stands changes', OLD.id
            USING ERRCODE = 'prohibited_sql_statement_attempted', TABLE = 'orders';
    END IF;
    RETURN NEW;
END
$$;
-- +goose StatementEnd

ALTER TABLE payments.orders DROP CONSTRAINT orders_exempt_of_a_month, DROP COLUMN gmf_exempt_month, DROP COLUMN gmf_exempt_units;

REVOKE UPDATE (restored_units) ON pricing.gmf_monthly FROM aluna_app;

-- +goose StatementBegin
CREATE OR REPLACE FUNCTION pricing.guard_gmf_monthly() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF TG_OP <> 'UPDATE' THEN
        RAISE EXCEPTION 'the GMF exemption used is never deleted or truncated'
            USING ERRCODE = 'prohibited_sql_statement_attempted', TABLE = 'gmf_monthly';
    END IF;
    IF (NEW.legal_entity_id, NEW.party_id, NEW.month) IS DISTINCT FROM (OLD.legal_entity_id, OLD.party_id, OLD.month)
        OR NEW.exempt_used_units < OLD.exempt_used_units THEN
        RAISE EXCEPTION 'the GMF exemption used of party % in % only grows', OLD.party_id, OLD.month
            USING ERRCODE = 'prohibited_sql_statement_attempted', TABLE = 'gmf_monthly';
    END IF;
    RETURN NEW;
END
$$;
-- +goose StatementEnd

ALTER TABLE pricing.gmf_monthly DROP CONSTRAINT gmf_monthly_restored_within_used, DROP COLUMN restored_units;
