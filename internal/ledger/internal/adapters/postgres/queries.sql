-- name: InsertAccount :exec
INSERT INTO ledger.accounts (id, legal_entity_id, code, kind, currency, balance_mode, lower_limit)
VALUES ($1, $2, $3, $4, $5, $6, $7);

-- name: InsertBalance :exec
INSERT INTO ledger.balances (account_id) VALUES ($1);

-- name: AccountsByID :many
SELECT id, legal_entity_id, code, kind, currency, balance_mode, lower_limit
FROM ledger.accounts
WHERE id = ANY(@ids::uuid[]);

-- Bloquea los saldos en orden de account_id (ADR-0004): dos postings sobre las mismas cuentas toman
-- los locks en el mismo orden y no se bloquean en ciclo.
-- name: LockBalances :many
SELECT account_id, posted, pending_debits, pending_credits
FROM ledger.balances
WHERE account_id = ANY(@account_ids::uuid[])
ORDER BY account_id
FOR UPDATE;

-- name: InsertTransaction :one
INSERT INTO ledger.transactions
    (id, legal_entity_id, code, value_date, business_date, source, external_ref, actor, channel, rule_version)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)
RETURNING posted_at;

-- Los entries no llevan entidad, fecha valor ni posted_at: los copia entries_stamp de la transacción.
-- name: InsertEntries :exec
INSERT INTO ledger.entries (transaction_id, account_id, currency, amount)
SELECT @transaction_id::uuid, unnest(@account_ids::uuid[]), unnest(@currencies::text[]), unnest(@amounts::bigint[]);

-- name: AddToBalance :exec
UPDATE ledger.balances SET posted = posted + @delta, version = version + 1 WHERE account_id = @account_id;

-- name: Balance :one
SELECT posted, pending_debits, pending_credits, version FROM ledger.balances WHERE account_id = $1;
