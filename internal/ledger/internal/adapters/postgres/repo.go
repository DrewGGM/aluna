// Package postgres is the ledger adapter on PostgreSQL: it implements the app ports with the code sqlc
// generates from queries.sql. Each query translates its own errors that carry domain meaning and wraps
// the rest with %w and the operation name: 40001 and 40P01 stay recognizable so that UnitOfWork.Do
// retries (ADR-0009).
package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/DrewGGM/aluna/internal/ids"
	"github.com/DrewGGM/aluna/internal/ledger/internal/adapters/postgres/sqlcgen"
	"github.com/DrewGGM/aluna/internal/ledger/internal/app"
	"github.com/DrewGGM/aluna/internal/ledger/internal/domain"
	"github.com/DrewGGM/aluna/internal/money"
)

// Store implements the app ports on a database connection or transaction.
type Store struct{ q *sqlcgen.Queries }

var (
	_ app.PostingStore  = (*Store)(nil)
	_ app.AccountWriter = (*Store)(nil)
)

// NewStore creates the Store on db: a postgres.Tx inside UnitOfWork.Do or the pool for reads.
func NewStore(db sqlcgen.DBTX) *Store { return &Store{q: sqlcgen.New(db)} }

// InsertAccount registers the account and its zero balance row (async ones too: the T-023 aggregator
// updates it).
func (s *Store) InsertAccount(ctx context.Context, a domain.Account) error {
	err := s.q.InsertAccount(ctx, sqlcgen.InsertAccountParams{
		ID: a.ID, LegalEntityID: a.LegalEntityID, Code: a.Code, Kind: string(a.Kind),
		Currency: a.Currency.Code(), BalanceMode: string(a.Mode), LowerLimit: a.LowerLimit,
	})
	if isUniqueViolation(err, "accounts_legal_entity_id_code_key") {
		return fmt.Errorf("%w: code %q already exists in the entity", app.ErrInvalidAccount, a.Code)
	}
	if err != nil {
		return fmt.Errorf("insert account: %w", err)
	}
	if err := s.q.InsertBalance(ctx, a.ID); err != nil {
		return fmt.Errorf("create account balance: %w", err)
	}
	return nil
}

// Accounts returns the accounts that exist among accountIDs.
func (s *Store) Accounts(ctx context.Context, accountIDs []ids.ID) (map[ids.ID]domain.Account, error) {
	rows, err := s.q.AccountsByID(ctx, accountIDs)
	if err != nil {
		return nil, fmt.Errorf("read accounts: %w", err)
	}
	accounts := make(map[ids.ID]domain.Account, len(rows))
	for _, r := range rows {
		a, err := toAccount(r.LedgerAccount)
		if err != nil {
			return nil, err
		}
		accounts[a.ID] = a
	}
	return accounts, nil
}

// AccountOf reads one of the entity's accounts; another entity's account is ErrAccountNotFound.
func (s *Store) AccountOf(ctx context.Context, entity, id ids.ID) (domain.Account, error) {
	r, err := s.q.AccountOfEntity(ctx, sqlcgen.AccountOfEntityParams{ID: id, LegalEntityID: entity})
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.Account{}, fmt.Errorf("%w: %s", app.ErrAccountNotFound, id)
	}
	if err != nil {
		return domain.Account{}, fmt.Errorf("read account: %w", err)
	}
	return toAccount(r.LedgerAccount)
}

func toAccount(r sqlcgen.LedgerAccount) (domain.Account, error) {
	cur, err := money.CurrencyOf(r.Currency)
	if err != nil {
		return domain.Account{}, fmt.Errorf("account %s: %w", r.Code, err)
	}
	return domain.Account{
		ID: r.ID, LegalEntityID: r.LegalEntityID, Code: r.Code, Kind: domain.Kind(r.Kind),
		Currency: cur, Mode: domain.BalanceMode(r.BalanceMode), LowerLimit: r.LowerLimit,
	}, nil
}

// TransactionOf reads one of the entity's transactions with its entries, in the order they were posted.
func (s *Store) TransactionOf(ctx context.Context, entity, id ids.ID) (app.Transaction, error) {
	r, err := s.q.TransactionOfEntity(ctx, sqlcgen.TransactionOfEntityParams{ID: id, LegalEntityID: entity})
	if errors.Is(err, pgx.ErrNoRows) {
		return app.Transaction{}, fmt.Errorf("%w: %s", app.ErrTransactionNotFound, id)
	}
	if err != nil {
		return app.Transaction{}, fmt.Errorf("read transaction: %w", err)
	}
	rows, err := s.q.EntriesOf(ctx, id)
	if err != nil {
		return app.Transaction{}, fmt.Errorf("read entries: %w", err)
	}
	entries := make([]domain.EntryInput, len(rows))
	for i, e := range rows {
		cur, err := money.CurrencyOf(e.Currency)
		if err != nil {
			return app.Transaction{}, fmt.Errorf("entry of %s: %w", id, err)
		}
		entries[i] = domain.EntryInput{AccountID: e.AccountID, Amount: money.New(e.Amount, cur)}
	}
	rec, err := recordOfRow(r)
	if err != nil {
		return app.Transaction{}, err
	}
	return app.Transaction{TransactionRecord: rec, PostedAt: r.PostedAt, Entries: entries}, nil
}

func recordOfRow(r sqlcgen.TransactionOfEntityRow) (app.TransactionRecord, error) {
	var metadata map[string]string
	if err := json.Unmarshal(r.Metadata, &metadata); err != nil {
		return app.TransactionRecord{}, fmt.Errorf("metadata of %s: %w", r.ID, err)
	}
	if len(metadata) == 0 {
		metadata = nil // as Post wrote it: what is read equals what Post returned
	}
	rec := app.TransactionRecord{
		ID: r.ID, LegalEntityID: r.LegalEntityID, Code: r.Code, ValueDate: r.ValueDate, BusinessDate: r.BusinessDate,
		Source: r.Source, ExternalRef: r.ExternalRef, Actor: r.Actor, Channel: r.Channel, RuleVersion: r.RuleVersion,
		Metadata: metadata,
	}
	if r.ReversesID != nil {
		rec.ReversesID = *r.ReversesID
	}
	return rec, nil
}

// LockBalances locks (FOR UPDATE, in account_id order) and returns the balances of accountIDs.
func (s *Store) LockBalances(ctx context.Context, accountIDs []ids.ID) (map[ids.ID]app.LockedBalance, error) {
	rows, err := s.q.LockBalances(ctx, accountIDs)
	if err != nil {
		return nil, fmt.Errorf("lock balances: %w", err)
	}
	balances := make(map[ids.ID]app.LockedBalance, len(rows))
	for _, r := range rows {
		balances[r.AccountID] = app.LockedBalance{Posted: r.Posted, PendingDebits: r.PendingDebits, PendingCredits: r.PendingCredits}
	}
	return balances, nil
}

// TransactionByRef reads the entity's transaction with (source, ref), with its entries; found says
// whether there is one.
func (s *Store) TransactionByRef(ctx context.Context, entity ids.ID, source, ref string) (app.Transaction, bool, error) {
	id, err := s.q.TransactionIDByRef(ctx, sqlcgen.TransactionIDByRefParams{LegalEntityID: entity, Source: source, ExternalRef: ref})
	if errors.Is(err, pgx.ErrNoRows) {
		return app.Transaction{}, false, nil
	}
	if err != nil {
		return app.Transaction{}, false, fmt.Errorf("find transaction by reference: %w", err)
	}
	tx, err := s.TransactionOf(ctx, entity, id)
	return tx, err == nil, err
}

// InsertTransaction inserts the header and its entries and returns the posted_at the database set. A
// repeated reference is not inserted: that way the database transaction stays usable to reread the
// original, without the 23505 that would abort it. The ON CONFLICT names no columns so that it also
// covers transactions_single_reversal: two concurrent reversals of the same transaction conflict on both
// indexes, and without balance locks (async accounts) the second one would see that one's 23505. If
// another transaction inserted the same row and has not finished, the INSERT waits for it and decides
// according to its commit or rollback.
func (s *Store) InsertTransaction(ctx context.Context, rec app.TransactionRecord, entries []domain.EntryInput) (time.Time, error) {
	metadata := []byte("{}") // json.Marshal of a nil map gives null, and the column expects an object
	if len(rec.Metadata) > 0 {
		var err error
		if metadata, err = json.Marshal(rec.Metadata); err != nil {
			return time.Time{}, fmt.Errorf("transaction metadata: %w", err)
		}
	}
	var reverses *ids.ID
	if rec.ReversesID != (ids.ID{}) {
		reverses = &rec.ReversesID
	}
	postedAt, err := s.q.InsertTransaction(ctx, sqlcgen.InsertTransactionParams{
		ID: rec.ID, LegalEntityID: rec.LegalEntityID, Code: rec.Code, ValueDate: rec.ValueDate,
		BusinessDate: rec.BusinessDate, Source: rec.Source, ExternalRef: rec.ExternalRef, Actor: rec.Actor,
		Channel: rec.Channel, RuleVersion: rec.RuleVersion, ReversesID: reverses, Metadata: metadata,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return time.Time{}, fmt.Errorf("%w: %s/%s", app.ErrDuplicateExternalRef, rec.Source, rec.ExternalRef)
	}
	if err != nil {
		return time.Time{}, fmt.Errorf("insert transaction: %w", err)
	}
	if err := s.insertEntries(ctx, rec.ID, entries); err != nil {
		return time.Time{}, err
	}
	return postedAt, nil
}

// insertEntries inserts the entries of transaction txID in a single statement.
func (s *Store) insertEntries(ctx context.Context, txID ids.ID, entries []domain.EntryInput) error {
	params := sqlcgen.InsertEntriesParams{
		TransactionID: txID,
		AccountIds:    make([]ids.ID, len(entries)),
		Currencies:    make([]string, len(entries)),
		Amounts:       make([]int64, len(entries)),
	}
	for i, e := range entries {
		params.AccountIds[i], params.Currencies[i], params.Amounts[i] = e.AccountID, e.Amount.Currency().Code(), e.Amount.Units()
	}
	if err := s.q.InsertEntries(ctx, params); err != nil {
		return fmt.Errorf("insert entries: %w", err)
	}
	return nil
}

// AddToBalance adds d to the account's balance and bumps its version.
func (s *Store) AddToBalance(ctx context.Context, accountID ids.ID, d app.BalanceDelta) error {
	err := s.q.AddToBalance(ctx, sqlcgen.AddToBalanceParams{
		Posted: d.Posted, PendingDebits: d.PendingDebits, PendingCredits: d.PendingCredits, AccountID: accountID,
	})
	if err != nil {
		return fmt.Errorf("update balance of %s: %w", accountID, err)
	}
	return nil
}

// Balance returns an account's materialized balance, without locking it.
func (s *Store) Balance(ctx context.Context, accountID ids.ID) (app.Balance, error) {
	r, err := s.q.Balance(ctx, accountID)
	if errors.Is(err, pgx.ErrNoRows) {
		return app.Balance{}, fmt.Errorf("%w: %s", app.ErrAccountNotFound, accountID)
	}
	if err != nil {
		return app.Balance{}, fmt.Errorf("read balance: %w", err)
	}
	return app.Balance{Posted: r.Posted, PendingDebits: r.PendingDebits, PendingCredits: r.PendingCredits, Version: r.Version}, nil
}

// PostedAsOf sums the account's entries within the bitemporal cut at.
func (s *Store) PostedAsOf(ctx context.Context, accountID ids.ID, at app.BalanceAsOf) (int64, error) {
	arg := sqlcgen.PostedAsOfParams{AccountID: accountID}
	if at.ValueDate != nil {
		arg.AsOfValueDate = pgtype.Date{Time: *at.ValueDate, Valid: true}
	}
	if at.KnownAt != nil {
		arg.AsKnownAt = pgtype.Timestamptz{Time: *at.KnownAt, Valid: true}
	}
	posted, err := s.q.PostedAsOf(ctx, arg)
	if err != nil {
		return 0, fmt.Errorf("sum bitemporal balance: %w", err)
	}
	return posted, nil
}

func isUniqueViolation(err error, constraint string) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23505" && pgErr.ConstraintName == constraint
}

// DatabaseNow is the database time (clock_timestamp), the same one that sets posted_at.
func (s *Store) DatabaseNow(ctx context.Context) (time.Time, error) {
	now, err := s.q.DatabaseNow(ctx)
	if err != nil {
		return time.Time{}, fmt.Errorf("read database time: %w", err)
	}
	return now, nil
}

// InsertHold stores a new hold of the entity.
func (s *Store) InsertHold(ctx context.Context, entity ids.ID, h app.Hold) error {
	err := s.q.InsertHold(ctx, sqlcgen.InsertHoldParams{
		ID: h.ID(), LegalEntityID: entity, AccountID: h.AccountID(), CounterAccountID: h.CounterAccountID(),
		Currency: h.Amount().Currency().Code(), Amount: h.Amount().Units(), ExpiresAt: h.ExpiresAt(),
	})
	if err != nil {
		return fmt.Errorf("insert hold: %w", err)
	}
	return nil
}

// LockHold locks the entity's hold (FOR UPDATE) and restores it.
func (s *Store) LockHold(ctx context.Context, entity, id ids.ID) (app.Hold, error) {
	r, err := s.q.LockHold(ctx, sqlcgen.LockHoldParams{ID: id, LegalEntityID: entity})
	if errors.Is(err, pgx.ErrNoRows) {
		return app.Hold{}, fmt.Errorf("%w: %s", app.ErrHoldNotFound, id)
	}
	if err != nil {
		return app.Hold{}, fmt.Errorf("lock hold: %w", err)
	}
	return restoredHold(r)
}

// LockDueHolds locks a batch of pending holds due at now, skipping the locked ones.
func (s *Store) LockDueHolds(ctx context.Context, now time.Time, limit int) ([]app.Hold, error) {
	rows, err := s.q.LockDueHolds(ctx, sqlcgen.LockDueHoldsParams{Now: now, Batch: int64(limit)})
	if err != nil {
		return nil, fmt.Errorf("lock due holds: %w", err)
	}
	due := make([]app.Hold, len(rows))
	for i, r := range rows {
		if due[i], err = restoredHold(sqlcgen.LockHoldRow(r)); err != nil {
			return nil, err
		}
	}
	return due, nil
}

// restoredHold restores a hold that was read. A row that cannot be restored is corrupt data, not a client
// error: it is reported without the domain error so that it surfaces as a 500 and is not stored as the
// key's response.
func restoredHold(r sqlcgen.LockHoldRow) (app.Hold, error) {
	cur, err := money.CurrencyOf(r.Currency)
	if err != nil {
		return app.Hold{}, fmt.Errorf("corrupt hold %s: %v", r.ID, err) //nolint:errorlint // see restoredHold
	}
	h, err := domain.RestoreHold(domain.HoldSnapshot{
		ID: r.ID, AccountID: r.AccountID, Amount: money.New(r.Amount, cur), Captured: money.New(r.CapturedAmount, cur),
		Status: domain.HoldStatus(r.Status), ExpiresAt: r.ExpiresAt,
	})
	if err != nil {
		return app.Hold{}, fmt.Errorf("corrupt hold %s: %v", r.ID, err) //nolint:errorlint // see restoredHold
	}
	return app.RestoredHold(h, r.CounterAccountID), nil
}

// CloseHold stores the outcome of a hold that left pending.
func (s *Store) CloseHold(ctx context.Context, h app.Hold, captureTxID ids.ID) error {
	var capture *ids.ID
	if captureTxID != (ids.ID{}) {
		capture = &captureTxID
	}
	err := s.q.CloseHold(ctx, sqlcgen.CloseHoldParams{
		Status: string(h.Status()), CapturedAmount: h.Captured().Units(), CaptureTxID: capture, ID: h.ID(),
	})
	if err != nil {
		return fmt.Errorf("close hold %s: %w", h.ID(), err)
	}
	return nil
}

// LockAsyncCursor locks and reads the aggregator's cursor.
func (s *Store) LockAsyncCursor(ctx context.Context) (app.AsyncCursor, error) {
	r, err := s.q.LockAsyncCursor(ctx)
	if err != nil {
		return app.AsyncCursor{}, fmt.Errorf("lock aggregation cursor: %w", err)
	}
	return app.AsyncCursor{Xact: r.PostedXact, EntryID: r.EntryID}, nil
}

// AsyncEntriesAfter reads a batch of async-account entries after c and the cursor of the last one.
func (s *Store) AsyncEntriesAfter(ctx context.Context, c app.AsyncCursor, limit int) ([]domain.EntryInput, app.AsyncCursor, error) {
	rows, err := s.q.AsyncEntriesAfter(ctx, sqlcgen.AsyncEntriesAfterParams{PostedXact: c.Xact, EntryID: c.EntryID, Batch: int64(limit)})
	if err != nil {
		return nil, c, fmt.Errorf("read async entries: %w", err)
	}
	entries := make([]domain.EntryInput, len(rows))
	for i, r := range rows {
		cur, err := money.CurrencyOf(r.Currency)
		if err != nil {
			return nil, c, fmt.Errorf("entry %d: %w", r.ID, err)
		}
		entries[i] = domain.EntryInput{AccountID: r.AccountID, Amount: money.New(r.Amount, cur)}
		c = app.AsyncCursor{Xact: r.PostedXact, EntryID: r.ID}
	}
	return entries, c, nil
}

// MoveAsyncCursor sets the cursor to c.
func (s *Store) MoveAsyncCursor(ctx context.Context, c app.AsyncCursor) error {
	if err := s.q.MoveAsyncCursor(ctx, sqlcgen.MoveAsyncCursorParams{PostedXact: c.Xact, EntryID: c.EntryID}); err != nil {
		return fmt.Errorf("move aggregation cursor: %w", err)
	}
	return nil
}
