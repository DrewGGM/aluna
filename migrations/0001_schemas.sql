-- One schema per module (ADR-0002) and the application role. aluna_app owns nothing: the tables
-- belong to the migrating user, so the application can neither alter them nor bypass each module's
-- REVOKEs. On deployment, the user the application connects as is made a member of aluna_app
-- (GRANT aluna_app TO <user>); the migrating user owns the tables.

-- +goose Up
CREATE SCHEMA infra;
CREATE SCHEMA ledger;
CREATE SCHEMA calendar;
CREATE SCHEMA party;
CREATE SCHEMA products;
CREATE SCHEMA pricing;
CREATE SCHEMA deposits;
CREATE SCHEMA payments;
CREATE SCHEMA compliance;
CREATE SCHEMA recon;
CREATE SCHEMA accounting;
CREATE SCHEMA approvals;
CREATE SCHEMA audit;
CREATE SCHEMA access;
CREATE SCHEMA webhooks;

-- Roles belong to the cluster, not the database: it may already exist because of another database on
-- the same server, or another database migrating at the same time may create it (hence the error is
-- caught and an IF NOT EXISTS is not enough; the race raises unique_violation on pg_authid, not
-- duplicate_object).
-- +goose StatementBegin
DO $$
BEGIN
    CREATE ROLE aluna_app NOLOGIN;
EXCEPTION WHEN duplicate_object OR unique_violation THEN
    NULL;
END
$$;
-- +goose StatementEnd

GRANT USAGE ON SCHEMA infra, ledger, calendar, party, products, pricing, deposits, payments,
    compliance, recon, accounting, approvals, audit, access, webhooks TO aluna_app;

-- +goose Down
-- No CASCADE: if a later migration left tables behind, the down must fail rather than drop them.
DROP SCHEMA webhooks, access, audit, approvals, accounting, recon, compliance, payments, deposits,
    pricing, products, party, calendar, ledger, infra;
-- The role is kept: another database in the cluster may be using it.
