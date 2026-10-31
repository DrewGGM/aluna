-- name: InsertAccount :exec
INSERT INTO ledger.accounts (id, legal_entity_id, code, kind, currency, balance_mode, lower_limit)
VALUES ($1, $2, $3, $4, $5, $6, $7);

-- name: InsertBalance :exec
INSERT INTO ledger.balances (account_id) VALUES ($1);

-- name: AccountsByID :many
SELECT sqlc.embed(a) FROM ledger.accounts a WHERE a.id = ANY(@ids::uuid[]);

-- Locks the balances in account_id order (ADR-0004): two postings on the same accounts take the locks
-- in the same order and never deadlock.
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

-- Entries carry no entity, value date or posted_at: entries_stamp copies them from the transaction.
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

-- Bitemporal balance (FR-8): entries with a value date up to as_of_value_date and posted up to
-- as_known_at; a null parameter does not filter that axis. It walks the account's entries by index and
-- reads each one from the heap (amount is not in the index): it costs as much as the account's history.
-- name: PostedAsOf :one
SELECT coalesce(sum(amount), 0)::bigint AS posted
FROM ledger.entries
WHERE account_id = @account_id
  AND value_date <= coalesce(sqlc.narg(as_of_value_date)::date, 'infinity'::date)
  AND posted_at <= coalesce(sqlc.narg(as_known_at)::timestamptz, 'infinity'::timestamptz);

-- Reads by ID filter by entity: another entity's rows look the same as missing ones.
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

-- Locks the hold before the balances (ADR-0004). Another entity's hold looks the same as a missing one.
-- name: LockHold :one
SELECT id, account_id, counter_account_id, currency, amount, captured_amount, status, expires_at
FROM ledger.holds
WHERE id = @id AND legal_entity_id = @legal_entity_id
FOR UPDATE;

-- Expiry batch: overdue holds (expires_at before now, as in Hold.Expire), oldest first, through the
-- partial index on pending. SKIP LOCKED skips those locked by a capture or release in progress, which
-- decides their outcome.
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

-- Serializes the aggregators: the second one waits and, once it gets the lock, reads the cursor the
-- first one left.
-- name: LockAsyncCursor :one
SELECT posted_xact, entry_id FROM ledger.async_cursor FOR UPDATE;

-- Async account entries after the cursor, in (posted_xact, id) order, only from database transactions
-- older than the oldest one in progress: those have already committed or aborted and add no entries. It
-- walks transactions by posted_xact and reads each one's entries by transaction_id: with a direct join,
-- the planner enters through the async account and reads its whole history on every batch. OFFSET 0
-- keeps it from flattening the subquery and going back to that plan.
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
