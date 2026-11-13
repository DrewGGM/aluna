-- The application writes only an entry's own columns: its transaction, account, currency and amount. The id
-- comes from the identity and the entity and the dates from the transaction (entries_stamp), so with INSERT on
-- every column a writer could override the identity (OVERRIDING SYSTEM VALUE) and take future ids.

-- +goose Up
REVOKE INSERT ON ledger.entries FROM aluna_app;
GRANT INSERT (transaction_id, account_id, currency, amount) ON ledger.entries TO aluna_app;

-- +goose Down
REVOKE INSERT (transaction_id, account_id, currency, amount) ON ledger.entries FROM aluna_app;
GRANT INSERT ON ledger.entries TO aluna_app;
