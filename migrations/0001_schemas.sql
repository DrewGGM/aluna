-- Un esquema por módulo (ADR-0002) y el rol de la aplicación. aluna_app no es dueño de nada: las
-- tablas son del usuario que migra, así que la aplicación no puede alterarlas ni saltarse los REVOKE
-- de cada módulo. Al desplegar, el usuario con el que se conecta la aplicación se hace miembro de
-- aluna_app (GRANT aluna_app TO <usuario>); el que migra es el dueño de las tablas.

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

-- Los roles son del clúster, no de la base: puede existir ya por otra base del mismo servidor, o
-- crearlo otra base que migra al mismo tiempo (por eso se atrapa el error y no basta un IF NOT EXISTS;
-- la carrera da unique_violation en pg_authid y no duplicate_object).
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
-- Sin CASCADE: si una migración posterior dejó tablas, el down debe fallar y no borrarlas.
DROP SCHEMA webhooks, access, audit, approvals, accounting, recon, compliance, payments, deposits,
    pricing, products, party, calendar, ledger, infra;
-- El rol se conserva: puede estar en uso por otra base del clúster.
