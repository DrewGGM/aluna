-- Solicitudes de maker-checker (FR-39, ADR-0014): un comando sensible se guarda con su payload y se
-- ejecuta cuando otra persona lo aprueba. La segregación la garantiza la base (checker <> maker) y una
-- solicitud decidida no cambia más: es evidencia de quién pidió y quién aprobó.

-- +goose Up
CREATE TABLE approvals.requests (
    id              uuid PRIMARY KEY,
    legal_entity_id uuid NOT NULL,
    kind            text NOT NULL CHECK (kind <> ''),
    payload         jsonb NOT NULL,
    maker           text NOT NULL CHECK (maker <> ''),
    status          text NOT NULL DEFAULT 'pending' CHECK (status IN ('pending', 'approved', 'rejected', 'failed')),
    checker         text,
    reason          text,  -- por qué se rechazó
    error           text,  -- por qué falló la ejecución aprobada; no sale por la API
    result          jsonb, -- lo que devolvió la ejecución aprobada
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
        RAISE EXCEPTION 'las solicitudes de aprobación no se borran'
            USING ERRCODE = 'prohibited_sql_statement_attempted', TABLE = 'requests';
    END IF;
    IF OLD.status <> 'pending' THEN
        RAISE EXCEPTION 'la solicitud % ya se decidió', OLD.id
            USING ERRCODE = 'prohibited_sql_statement_attempted', TABLE = 'requests';
    END IF;
    IF (NEW.id, NEW.legal_entity_id, NEW.kind, NEW.payload, NEW.maker, NEW.created_at, NEW.expires_at)
        IS DISTINCT FROM (OLD.id, OLD.legal_entity_id, OLD.kind, OLD.payload, OLD.maker, OLD.created_at, OLD.expires_at) THEN
        RAISE EXCEPTION 'de la solicitud % solo cambia su decisión', OLD.id
            USING ERRCODE = 'prohibited_sql_statement_attempted', TABLE = 'requests';
    END IF;
    IF OLD.expires_at <= now() THEN
        RAISE EXCEPTION 'la solicitud % venció', OLD.id
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
-- solo las columnas de la decisión: lo que se pidió y quién lo pidió no cambian
GRANT UPDATE (status, checker, reason, error, result, decided_at) ON approvals.requests TO aluna_app;

-- +goose Down
DROP TABLE approvals.requests;
DROP FUNCTION approvals.guard_request();
