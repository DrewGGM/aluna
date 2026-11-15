-- Why an approved command was rejected (Q-97 a): a request that ends up failed keeps, next to its error (which the
-- API never shows: it may name internals), the problem code its command answered, such as already_reversed, which
-- the API shows. Only a failed request has one.

-- +goose Up
ALTER TABLE approvals.requests
    ADD COLUMN failure text CHECK (failure ~ '^[a-z][a-z0-9_]{0,63}$'),
    ADD CONSTRAINT requests_failure_only_failed CHECK (failure IS NULL OR status = 'failed');

GRANT UPDATE (failure) ON approvals.requests TO aluna_app;

-- +goose Down
REVOKE UPDATE (failure) ON approvals.requests FROM aluna_app;
ALTER TABLE approvals.requests DROP CONSTRAINT requests_failure_only_failed, DROP COLUMN failure;
