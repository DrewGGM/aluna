-- A webhook endpoint can be revoked (Q-134): one whose secret leaked, or that its integrator no longer serves, stops
-- getting events, and its pending deliveries fail. A revoked endpoint keeps its row, and who revoked it, as its
-- deliveries keep theirs: the history of what was sent where stays.

-- +goose Up
ALTER TABLE webhooks.endpoints
    ADD COLUMN revoked_at timestamptz,
    ADD COLUMN revoked_by text CHECK (char_length(revoked_by) BETWEEN 1 AND 255),
    ADD CONSTRAINT endpoints_revocation CHECK ((revoked_at IS NULL) = (revoked_by IS NULL));

-- +goose StatementBegin
CREATE FUNCTION webhooks.guard_revocation() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF OLD.revoked_at IS NOT NULL AND (NEW.revoked_at, NEW.revoked_by) IS DISTINCT FROM (OLD.revoked_at, OLD.revoked_by) THEN
        RAISE EXCEPTION 'a revoked webhook endpoint stays revoked: endpoint %', OLD.id
            USING ERRCODE = 'prohibited_sql_statement_attempted', TABLE = 'endpoints';
    END IF;
    RETURN NEW;
END
$$;
-- +goose StatementEnd

CREATE TRIGGER endpoints_revocation_guarded BEFORE UPDATE ON webhooks.endpoints
    FOR EACH ROW EXECUTE FUNCTION webhooks.guard_revocation();

GRANT UPDATE (revoked_at, revoked_by) ON webhooks.endpoints TO aluna_app;

-- +goose Down
-- Without the columns a revoked endpoint would get again every event since its revocation, signed with the secret it
-- was revoked for: the rollback refuses while one exists.
-- +goose StatementBegin
DO $$
BEGIN
    IF EXISTS (SELECT FROM webhooks.endpoints WHERE revoked_at IS NOT NULL) THEN
        RAISE EXCEPTION 'webhooks.endpoints has revoked endpoints: rolling back would deliver to them again';
    END IF;
END
$$;
-- +goose StatementEnd
REVOKE UPDATE (revoked_at, revoked_by) ON webhooks.endpoints FROM aluna_app;
DROP TRIGGER endpoints_revocation_guarded ON webhooks.endpoints;
DROP FUNCTION webhooks.guard_revocation();
ALTER TABLE webhooks.endpoints DROP CONSTRAINT endpoints_revocation, DROP COLUMN revoked_by, DROP COLUMN revoked_at;
