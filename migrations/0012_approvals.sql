-- Maker-checker requests (FR-39, ADR-0014): a sensitive command is stored with its payload and runs
-- when another person approves it. The database guarantees segregation (checker <> maker), and a
-- decided request never changes again: it is evidence of who asked and who approved.

-- +goose Up
CREATE TABLE approvals.requests (
    id              uuid PRIMARY KEY,
    legal_entity_id uuid NOT NULL,
    kind            text NOT NULL CHECK (kind <> ''),
    payload         jsonb NOT NULL,
    maker           text NOT NULL CHECK (maker <> ''),
    status          text NOT NULL DEFAULT 'pending' CHECK (status IN ('pending', 'approved', 'rejected', 'failed')),
    checker         text,
    reason          text,  -- why it was rejected
    error           text,  -- why the approved execution failed; never exposed through the API
    result          jsonb, -- what the approved execution returned
    created_at      timestamptz NOT NULL DEFAULT now(),
    expires_at      timestamptz NOT NULL DEFAULT now() + interval '7 days',
    decided_at      timestamptz,
    CHECK (checker IS NULL OR checker <> maker AND checker <> ''),
    CHECK ((status = 'pending') = (checker IS NULL AND decided_at IS NULL)),
    CHECK (status <> 'rejected' OR coalesce(reason, '') <> ''),
    CHECK (status <> 'failed' OR coalesce(error, '') <> ''),
    CHECK (status <> 'approved' OR result IS NOT NULL)
);

CREATE INDEX requests_by_entity ON approvals.requests (legal_entity_id, created_at DESC);

-- +goose StatementBegin
CREATE FUNCTION approvals.guard_request() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF TG_OP <> 'UPDATE' THEN
        RAISE EXCEPTION 'approval requests are never deleted'
            USING ERRCODE = 'prohibited_sql_statement_attempted', TABLE = 'requests';
    END IF;
    IF OLD.status <> 'pending' THEN
        RAISE EXCEPTION 'request % was already decided', OLD.id
            USING ERRCODE = 'prohibited_sql_statement_attempted', TABLE = 'requests';
    END IF;
    IF (NEW.id, NEW.legal_entity_id, NEW.kind, NEW.payload, NEW.maker, NEW.created_at, NEW.expires_at)
        IS DISTINCT FROM (OLD.id, OLD.legal_entity_id, OLD.kind, OLD.payload, OLD.maker, OLD.created_at, OLD.expires_at) THEN
        RAISE EXCEPTION 'only the decision of request % can change', OLD.id
            USING ERRCODE = 'prohibited_sql_statement_attempted', TABLE = 'requests';
    END IF;
    IF OLD.expires_at <= now() THEN
        RAISE EXCEPTION 'request % has expired', OLD.id
            USING ERRCODE = 'prohibited_sql_statement_attempted', TABLE = 'requests';
    END IF;
    RETURN NEW;
END
$$;
-- +goose StatementEnd

CREATE TRIGGER requests_decided_once
    BEFORE UPDATE OR DELETE ON approvals.requests
    FOR EACH ROW EXECUTE FUNCTION approvals.guard_request();

CREATE TRIGGER requests_not_truncated
    BEFORE TRUNCATE ON approvals.requests
    FOR EACH STATEMENT EXECUTE FUNCTION approvals.guard_request();

GRANT SELECT ON approvals.requests TO aluna_app;
GRANT INSERT (id, legal_entity_id, kind, payload, maker) ON approvals.requests TO aluna_app;
-- only the decision columns: what was requested and who requested it never change
GRANT UPDATE (status, checker, reason, error, result, decided_at) ON approvals.requests TO aluna_app;

-- +goose Down
DROP TABLE approvals.requests;
DROP FUNCTION approvals.guard_request();
