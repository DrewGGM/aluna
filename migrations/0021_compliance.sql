-- Screening cases (FR-42): every screening of a party being registered or of an outgoing payment's
-- beneficiary, cleared by the vendor or pending until compliance resolves it. A case is resolved once, from
-- pending to cleared or confirmed, and nothing is deleted. Its subject has no foreign key: compliance does not
-- own parties or payment orders. A party that screening did not clear is pending_screening.

-- +goose Up
CREATE TABLE compliance.cases (
    id              uuid PRIMARY KEY,
    legal_entity_id uuid NOT NULL,
    kind            text NOT NULL CHECK (kind IN ('party', 'payment')),
    subject_id      uuid NOT NULL,
    status          text NOT NULL CHECK (status IN ('pending', 'cleared', 'confirmed')),
    vendor_ref      text CHECK (char_length(vendor_ref) BETWEEN 1 AND 255), -- null if the vendor did not answer
    created_at      timestamptz NOT NULL DEFAULT clock_timestamp(),
    resolved_by     text CHECK (char_length(resolved_by) BETWEEN 1 AND 255), -- who resolved a pending case
    resolved_at     timestamptz,
    CONSTRAINT cases_resolution CHECK ((resolved_by IS NULL) = (resolved_at IS NULL)
        AND (status <> 'pending' OR resolved_by IS NULL))
);

-- +goose StatementBegin
-- guard_case also stamps resolved_at: when a case was resolved is not what the caller says.
CREATE FUNCTION compliance.guard_case() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF TG_OP <> 'UPDATE' THEN
        RAISE EXCEPTION 'screening cases are never deleted or truncated'
            USING ERRCODE = 'prohibited_sql_statement_attempted', TABLE = 'cases';
    END IF;
    -- cases_resolution, with the resolved_at stamped below, wants a resolved_by and no pending one
    IF OLD.status <> 'pending'
        OR (NEW.id, NEW.legal_entity_id, NEW.kind, NEW.subject_id, NEW.vendor_ref, NEW.created_at)
            IS DISTINCT FROM (OLD.id, OLD.legal_entity_id, OLD.kind, OLD.subject_id, OLD.vendor_ref, OLD.created_at) THEN
        RAISE EXCEPTION 'a screening case is only resolved, once: case %', OLD.id
            USING ERRCODE = 'prohibited_sql_statement_attempted', TABLE = 'cases';
    END IF;
    NEW.resolved_at := clock_timestamp();
    RETURN NEW;
END
$$;
-- +goose StatementEnd

CREATE TRIGGER cases_guarded BEFORE UPDATE OR DELETE ON compliance.cases
    FOR EACH ROW EXECUTE FUNCTION compliance.guard_case();
CREATE TRIGGER cases_not_truncated BEFORE TRUNCATE ON compliance.cases
    FOR EACH STATEMENT EXECUTE FUNCTION compliance.guard_case();

GRANT SELECT, INSERT (id, legal_entity_id, kind, subject_id, status, vendor_ref), UPDATE (status, resolved_by)
    ON compliance.cases TO aluna_app;

ALTER TABLE party.versions DROP CONSTRAINT versions_kyc_status_check,
    ADD CONSTRAINT versions_kyc_status_check
        CHECK (kyc_status IN ('pending', 'approved', 'rejected', 'suspended', 'pending_screening'));

-- +goose Down
-- Once a party version is pending_screening, putting the old constraint back fails: versions are immutable.
ALTER TABLE party.versions DROP CONSTRAINT versions_kyc_status_check,
    ADD CONSTRAINT versions_kyc_status_check CHECK (kyc_status IN ('pending', 'approved', 'rejected', 'suspended'));
DROP TABLE compliance.cases;
DROP FUNCTION compliance.guard_case();
