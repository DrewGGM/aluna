-- Webhooks (FR-46, US-15): an integrator registers an https endpoint and gets a secret; each event of its entity is
-- queued for it, from the transaction that registered it on, in the Events API's order (after_xact, after_id is how
-- far it got), and is sent signed (Standard Webhooks) until the endpoint answers 2xx, with up to 8 retries in 72
-- hours; then the delivery is failed. A delivery that ended does not change any more.

-- +goose Up
CREATE TABLE webhooks.endpoints (
    id              uuid PRIMARY KEY,
    legal_entity_id uuid NOT NULL REFERENCES calendar.legal_entities (id),
    url             text NOT NULL CHECK (url ~ '^https://' AND char_length(url) <= 2048),
    secret          text NOT NULL CHECK (secret ~ '^whsec_[A-Za-z0-9+/]+=*$'),
    created_by      text NOT NULL CHECK (char_length(created_by) BETWEEN 1 AND 255),
    created_at      timestamptz NOT NULL DEFAULT clock_timestamp(),
    after_xact      xid8 NOT NULL DEFAULT pg_current_xact_id(),
    after_id        uuid NOT NULL DEFAULT '00000000-0000-0000-0000-000000000000'
);

CREATE TABLE webhooks.deliveries (
    id              uuid PRIMARY KEY, -- the webhook-id: the same in every retry
    endpoint_id     uuid NOT NULL REFERENCES webhooks.endpoints (id),
    event_id        uuid NOT NULL,
    body            bytea NOT NULL,   -- the CloudEvent as it is signed and sent
    status          text NOT NULL DEFAULT 'pending' CHECK (status IN ('pending', 'delivered', 'failed')),
    attempts        integer NOT NULL DEFAULT 0 CHECK (attempts BETWEEN 0 AND 9),
    next_attempt_at timestamptz NOT NULL,
    last_status     integer,          -- the endpoint's HTTP status in the last attempt, if it answered
    last_error      text CHECK (char_length(last_error) <= 1000),
    delivered_at    timestamptz,
    created_at      timestamptz NOT NULL DEFAULT clock_timestamp(),
    UNIQUE (endpoint_id, event_id),
    CHECK ((status = 'delivered') = (delivered_at IS NOT NULL))
);

CREATE INDEX deliveries_due ON webhooks.deliveries (next_attempt_at) WHERE status = 'pending';

-- +goose StatementBegin
CREATE FUNCTION webhooks.guard_delivery() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF TG_OP <> 'UPDATE' THEN
        RAISE EXCEPTION 'webhooks.deliveries keeps its rows'
            USING ERRCODE = 'prohibited_sql_statement_attempted', TABLE = 'deliveries';
    END IF;
    IF OLD.status <> 'pending' OR (NEW.id, NEW.endpoint_id, NEW.event_id, NEW.body, NEW.created_at)
        IS DISTINCT FROM (OLD.id, OLD.endpoint_id, OLD.event_id, OLD.body, OLD.created_at) THEN
        RAISE EXCEPTION 'a delivery only goes on while pending: delivery %', OLD.id
            USING ERRCODE = 'prohibited_sql_statement_attempted', TABLE = 'deliveries';
    END IF;
    RETURN NEW;
END
$$;
-- +goose StatementEnd

CREATE TRIGGER deliveries_guarded BEFORE UPDATE OR DELETE ON webhooks.deliveries
    FOR EACH ROW EXECUTE FUNCTION webhooks.guard_delivery();
CREATE TRIGGER deliveries_not_truncated BEFORE TRUNCATE ON webhooks.deliveries
    FOR EACH STATEMENT EXECUTE FUNCTION webhooks.guard_delivery();

GRANT SELECT, INSERT (id, legal_entity_id, url, secret, created_by), UPDATE (after_xact, after_id) ON webhooks.endpoints TO aluna_app;
GRANT SELECT, INSERT (id, endpoint_id, event_id, body, next_attempt_at),
    UPDATE (status, attempts, next_attempt_at, last_status, last_error, delivered_at) ON webhooks.deliveries TO aluna_app;

-- +goose Down
DROP TABLE webhooks.deliveries;
DROP FUNCTION webhooks.guard_delivery();
DROP TABLE webhooks.endpoints;
