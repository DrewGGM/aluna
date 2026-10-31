-- Party reference data (FR-16): who the entity's customers are, without their personal data. Names and
-- documents live in the onboarding system; the core keeps only external_ref pointing to it and the state
-- it needs to operate (KYC, risk, PEP, consents), versioned: every change is a new version and no version
-- is ever modified.

-- +goose Up
CREATE TABLE party.parties (
    id              uuid PRIMARY KEY,
    legal_entity_id uuid NOT NULL,
    kind            text NOT NULL CHECK (kind IN ('natural', 'legal')),
    external_ref    text NOT NULL CHECK (char_length(external_ref) BETWEEN 1 AND 255),
    created_at      timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT parties_external_ref_unique UNIQUE (legal_entity_id, external_ref),
    UNIQUE (id, legal_entity_id) -- lets a future foreign key require the same entity
);

CREATE TABLE party.versions (
    party_id    uuid NOT NULL REFERENCES party.parties (id),
    version     integer NOT NULL CHECK (version >= 1),
    kyc_status  text NOT NULL CHECK (kyc_status IN ('pending', 'approved', 'rejected', 'suspended')),
    risk_level  text NOT NULL CHECK (risk_level IN ('low', 'medium', 'high')),
    pep         boolean NOT NULL,
    consents    jsonb NOT NULL CHECK (jsonb_typeof(consents) = 'object'),
    valid_from  timestamptz NOT NULL DEFAULT now(),
    recorded_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    PRIMARY KEY (party_id, version)
);

-- +goose StatementBegin
CREATE FUNCTION party.forbid_change() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    RAISE EXCEPTION 'party reference data is never updated or deleted: a change is a new version'
        USING ERRCODE = 'prohibited_sql_statement_attempted', TABLE = TG_TABLE_NAME;
END
$$;
-- +goose StatementEnd

CREATE TRIGGER parties_immutable BEFORE UPDATE OR DELETE ON party.parties
    FOR EACH ROW EXECUTE FUNCTION party.forbid_change();
CREATE TRIGGER parties_not_truncated BEFORE TRUNCATE ON party.parties
    FOR EACH STATEMENT EXECUTE FUNCTION party.forbid_change();
CREATE TRIGGER versions_immutable BEFORE UPDATE OR DELETE ON party.versions
    FOR EACH ROW EXECUTE FUNCTION party.forbid_change();
CREATE TRIGGER versions_not_truncated BEFORE TRUNCATE ON party.versions
    FOR EACH STATEMENT EXECUTE FUNCTION party.forbid_change();

GRANT SELECT ON party.parties, party.versions TO aluna_app;
GRANT INSERT (id, legal_entity_id, kind, external_ref) ON party.parties TO aluna_app;
GRANT INSERT (party_id, version, kyc_status, risk_level, pep, consents) ON party.versions TO aluna_app;

-- +goose Down
DROP TABLE party.versions;
DROP TABLE party.parties;
DROP FUNCTION party.forbid_change();
