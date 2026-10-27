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

-- Saldo bitemporal (FR-8): entries con fecha valor hasta as_of_value_date y asentados hasta as_known_at;
-- un parámetro nulo no filtra ese eje. Recorre por índice los entries de la cuenta y lee cada uno del
-- heap (amount no está en el índice): cuesta lo que la historia de la cuenta.
-- name: PostedAsOf :one
SELECT coalesce(sum(amount), 0)::bigint AS posted
FROM ledger.entries
WHERE account_id = @account_id
  AND value_date <= coalesce(sqlc.narg(as_of_value_date)::date, 'infinity'::date)
  AND posted_at <= coalesce(sqlc.narg(as_known_at)::timestamptz, 'infinity'::timestamptz);

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

-- Lote de la expiración: los vencidos (expires_at anterior a now, como en Hold.Expire) más antiguos
-- primero, por el índice parcial de pending. SKIP LOCKED salta los que tiene bloqueados un capture o un
-- release en curso, que deciden su resultado.
-- name: LockDueHolds :many
SELECT id, account_id, counter_account_id, currency, amount, captured_amount, status, expires_at
FROM ledger.holds
WHERE status = 'pending' AND expires_at < @now
ORDER BY expires_at
LIMIT sqlc.arg(batch)::bigint
FOR UPDATE SKIP LOCKED;

-- name: CloseHold :exec
UPDATE ledger.holds SET status = @status, captured_amount = @captured_amount, capture_tx_id = @capture_tx_id
WHERE id = @id;

-- Serializa los agregadores: el segundo espera y, al bloquear, lee el cursor que dejó el primero.
-- name: LockAsyncCursor :one
SELECT posted_xact, entry_id FROM ledger.async_cursor FOR UPDATE;

-- Entries de cuentas async después del cursor, en orden de (posted_xact, id), solo de transacciones de
-- base anteriores a la más vieja en curso: esas ya confirmaron o abortaron y no suman entries. Recorre
-- transactions por posted_xact y lee los entries de cada una por transaction_id: unidos directo, el
-- planner entra por la cuenta async y lee su historia completa en cada lote. OFFSET 0 impide que
-- aplane la subconsulta y vuelva a ese plan.
-- name: AsyncEntriesAfter :many
SELECT t.posted_xact, e.id, e.account_id, e.currency, e.amount
FROM ledger.transactions t
CROSS JOIN LATERAL (
    SELECT id, account_id, currency, amount FROM ledger.entries WHERE transaction_id = t.id OFFSET 0
) e
JOIN ledger.accounts a ON a.id = e.account_id
WHERE a.balance_mode = 'async'
  AND t.posted_xact >= sqlc.arg(posted_xact)::xid8
  AND (t.posted_xact > sqlc.arg(posted_xact)::xid8 OR e.id > sqlc.arg(entry_id)::bigint)
  AND t.posted_xact < pg_snapshot_xmin(pg_current_snapshot())
ORDER BY t.posted_xact, e.id
LIMIT sqlc.arg(batch)::bigint;

-- name: MoveAsyncCursor :exec
UPDATE ledger.async_cursor SET posted_xact = @posted_xact, entry_id = @entry_id;
