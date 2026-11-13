-- Monthly statements (FR-45, US-16): one per deposit account and closed period, its canonical JSON byte for byte and
-- the SHA-256 of those bytes, which the database checks. A closed period takes no more postings (T-069), so its
-- statements are final and never change. statement_periods records the closed periods whose statements are all
-- generated, so the job does not go through them again.

-- +goose Up
CREATE TABLE deposits.statements (
    account_id   uuid NOT NULL REFERENCES deposits.accounts (id),
    period       char(7) NOT NULL CHECK (period ~ '^[0-9]{4}-(0[1-9]|1[0-2])$'), -- yyyy-mm
    document     bytea NOT NULL,
    content_hash char(64) NOT NULL,
    generated_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    PRIMARY KEY (account_id, period),
    CONSTRAINT statements_hash CHECK (content_hash = encode(sha256(document), 'hex'))
);

CREATE TABLE deposits.statement_periods (
    legal_entity_id uuid NOT NULL REFERENCES calendar.legal_entities (id),
    period          char(7) NOT NULL CHECK (period ~ '^[0-9]{4}-(0[1-9]|1[0-2])$'),
    completed_at    timestamptz NOT NULL DEFAULT clock_timestamp(),
    PRIMARY KEY (legal_entity_id, period)
);

CREATE TRIGGER statements_kept BEFORE UPDATE OR DELETE ON deposits.statements
    FOR EACH ROW EXECUTE FUNCTION deposits.forbid_change();
CREATE TRIGGER statements_not_truncated BEFORE TRUNCATE ON deposits.statements
    FOR EACH STATEMENT EXECUTE FUNCTION deposits.forbid_change();
CREATE TRIGGER statement_periods_kept BEFORE UPDATE OR DELETE ON deposits.statement_periods
    FOR EACH ROW EXECUTE FUNCTION deposits.forbid_change();
CREATE TRIGGER statement_periods_not_truncated BEFORE TRUNCATE ON deposits.statement_periods
    FOR EACH STATEMENT EXECUTE FUNCTION deposits.forbid_change();

GRANT SELECT, INSERT (account_id, period, document, content_hash) ON deposits.statements TO aluna_app;
GRANT SELECT, INSERT (legal_entity_id, period) ON deposits.statement_periods TO aluna_app;

-- +goose Down
DROP TABLE deposits.statement_periods;
DROP TABLE deposits.statements;
