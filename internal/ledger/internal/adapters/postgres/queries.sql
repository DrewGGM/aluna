-- name: InsertAccount :exec
INSERT INTO ledger.accounts (id, legal_entity_id, code, kind, currency, balance_mode, lower_limit)
VALUES ($1, $2, $3, $4, $5, $6, $7);

-- name: InsertBalance :exec
INSERT INTO ledger.balances (account_id) VALUES ($1);

-- name: AccountsByID :many
SELECT sqlc.embed(a) FROM ledger.accounts a WHERE a.id = ANY(@ids::uuid[]);

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
    (id, legal_entity_id, code, value_date, business_date, source, external_ref, actor, channel, rule_version,
     reverses_id, metadata)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12)
ON CONFLICT DO NOTHING
RETURNING posted_at;

-- name: TransactionIDByRef :one
SELECT id FROM ledger.transactions WHERE legal_entity_id = $1 AND source = $2 AND external_ref = $3;

-- Los entries no llevan entidad, fecha valor ni posted_at: los copia entries_stamp de la transacción.
-- name: InsertEntries :exec
INSERT INTO ledger.entries (transaction_id, account_id, currency, amount)
SELECT @transaction_id::uuid, unnest(@account_ids::uuid[]), unnest(@currencies::text[]), unnest(@amounts::bigint[]);

-- name: AddToBalance :exec
UPDATE ledger.balances
SET posted = posted + @posted, pending_debits = pending_debits + @pending_debits,
    pending_credits = pending_credits + @pending_credits, version = version + 1
WHERE account_id = @account_id;

-- name: Balance :one
SELECT posted, pending_debits, pending_credits, version FROM ledger.balances WHERE account_id = $1;

-- Las lecturas por ID filtran por entidad: lo ajeno se ve igual que lo que no existe.
-- name: AccountOfEntity :one
SELECT sqlc.embed(a) FROM ledger.accounts a WHERE a.id = @id AND a.legal_entity_id = @legal_entity_id;

-- name: TransactionOfEntity :one
SELECT id, legal_entity_id, code, value_date, business_date, source, external_ref, actor, channel, rule_version,
       reverses_id, metadata, posted_at
FROM ledger.transactions
WHERE id = @id AND legal_entity_id = @legal_entity_id;

-- name: EntriesOf :many
SELECT account_id, currency, amount FROM ledger.entries WHERE transaction_id = $1 ORDER BY id;

-- name: DatabaseNow :one
SELECT clock_timestamp()::timestamptz AS now;

-- name: InsertHold :exec
INSERT INTO ledger.holds (id, legal_entity_id, account_id, counter_account_id, currency, amount, expires_at)
VALUES ($1, $2, $3, $4, $5, $6, $7);

-- Bloquea el hold antes que los saldos (ADR-0004). Lo de otra entidad se ve igual que lo inexistente.
-- name: LockHold :one
SELECT id, account_id, counter_account_id, currency, amount, captured_amount, status, expires_at
FROM ledger.holds
WHERE id = @id AND legal_entity_id = @legal_entity_id
FOR UPDATE;

-- name: CloseHold :exec
UPDATE ledger.holds SET status = @status, captured_amount = @captured_amount, capture_tx_id = @capture_tx_id
WHERE id = @id;
