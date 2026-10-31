// Package app holds the ledger use cases. Each use case declares the persistence port it needs and does
// not know the database: the facade hands it an adapter bound to the database transaction in progress.
package app

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/DrewGGM/aluna/internal/ids"
	"github.com/DrewGGM/aluna/internal/ledger/internal/domain"
	"github.com/DrewGGM/aluna/internal/money"
)

var (
	// ErrAccountNotFound means an account that does not exist or belongs to another legal entity.
	ErrAccountNotFound = errors.New("account not found")
	// ErrCurrencyMismatch means an entry in a currency other than its account's.
	ErrCurrencyMismatch = errors.New("the entry is not in its account's currency")
	// ErrInvalidPosting means an incomplete posting header.
	ErrInvalidPosting = errors.New("invalid posting")
	// ErrTransactionNotFound means a transaction that does not exist or belongs to another legal entity.
	ErrTransactionNotFound = errors.New("transaction not found")
	// ErrDuplicateExternalRef means the source already posted a transaction with that reference: Post
	// returns it together with this error (FR-11).
	ErrDuplicateExternalRef = errors.New("the external reference was already posted")
)

// PostInput is a posting: its header and its entries. The dates count by their calendar day as written,
// regardless of the time.Time's time zone. ReversesID is the transaction the posting reverses (set by
// Reverse); the zero value means none.
type PostInput struct {
	LegalEntityID           ids.ID
	Code                    string
	ValueDate, BusinessDate time.Time
	Source, ExternalRef     string
	Actor, Channel          string
	RuleVersion             string
	ReversesID              ids.ID
	Metadata                map[string]string
	Entries                 []domain.EntryInput
}

// TransactionRecord is the header persisted for a transaction.
type TransactionRecord struct {
	ID                      ids.ID
	LegalEntityID           ids.ID
	Code                    string
	ValueDate, BusinessDate time.Time // UTC midnight of the calendar day
	Source, ExternalRef     string
	Actor, Channel          string
	RuleVersion             string
	ReversesID              ids.ID // zero if it is not a reversal
	Metadata                map[string]string
}

// Transaction is a posted transaction: its header, the instant the database set and its entries.
type Transaction struct {
	TransactionRecord
	PostedAt time.Time
	Entries  []domain.EntryInput
}

// LockedBalance is the locked, signed balance of a sync account, against which a posting is checked.
type LockedBalance struct {
	Posted, PendingDebits, PendingCredits int64
}

// PostingStore is the persistence of Post, all within the same database transaction.
type PostingStore interface {
	// TransactionByRef returns the entity's transaction with (source, ref); found says whether there is one.
	TransactionByRef(ctx context.Context, entity ids.ID, source, ref string) (tx Transaction, found bool, err error)
	Accounts(ctx context.Context, accountIDs []ids.ID) (map[ids.ID]domain.Account, error)
	LockBalances(ctx context.Context, accountIDs []ids.ID) (map[ids.ID]LockedBalance, error)
	// InsertTransaction inserts the header and entries, and returns the posted_at the database set. If the
	// entity already has (source, external_ref), it inserts nothing and returns ErrDuplicateExternalRef.
	InsertTransaction(ctx context.Context, rec TransactionRecord, entries []domain.EntryInput) (time.Time, error)
	// AddToBalance adds d to the account's balance in a single statement.
	AddToBalance(ctx context.Context, accountID ids.ID, d BalanceDelta) error
}

// BalanceDelta is what changes in an account's balance: the posted amount (signed) and the pending ones
// (positive when holding, negative when releasing).
type BalanceDelta struct {
	Posted, PendingDebits, PendingCredits int64
}

// pendingRelease is the pending amount a posting releases in the same transaction, that of the hold being
// captured: it counts as released when checking funds and is written together with the posting.
type pendingRelease struct {
	account         ids.ID
	debits, credits int64
}

// from deducts what is released from the locked balance of account id.
func (r pendingRelease) from(id ids.ID, b LockedBalance) LockedBalance {
	if id == r.account {
		b.PendingDebits -= r.debits
		b.PendingCredits -= r.credits
	}
	return b
}

// Post validates the header and double entry, locks the sync accounts' balances, checks that each
// account's net fits in it and posts the transaction, entries and balances. If the entity already posted
// (source, external_ref) it returns that original transaction with ErrDuplicateExternalRef and writes or
// emits nothing (FR-11); if it posts, it emits transaction.posted. Retrying something that already went
// through must not fail because of a balance that changed afterwards, so the reference is looked up
// before validating, again if funds fall short (see available) and on insert, in case another producer
// posted it on other accounts in the meantime.
func Post(ctx context.Context, store PostingStore, guard PeriodGuard, events Events, in PostInput) (Transaction, error) {
	if strings.HasPrefix(in.Source, reservedSourcePrefix) || in.ReversesID != (ids.ID{}) {
		return Transaction{}, fmt.Errorf("%w: %s* sources and reversals belong to the ledger", ErrInvalidPosting, reservedSourcePrefix)
	}
	posted, err := post(ctx, store, guard, in, pendingRelease{})
	if err != nil {
		return posted, err
	}
	if err := events.Emit(ctx, transactionEvent(typeTransactionPosted, posted)); err != nil {
		return Transaction{}, err
	}
	return posted, nil
}

// PeriodGuard rejects posting with a business date inside a closed period (FR-15).
type PeriodGuard interface {
	EnsureOpen(ctx context.Context, entity ids.ID, businessDate time.Time) error
}

// reservedSourcePrefix marks the sources of the transactions the ledger itself builds (reversals, hold
// captures): their external reference reserves their place, so nobody else may use them, or any posting
// could take the place of another transaction's reversal or capture and Reverse or CaptureHold would
// consider it done.
const reservedSourcePrefix = "ledger."

// post is Post for the ledger's own use cases, which post with a ledger.* source and may release a
// hold's pending amount in the same write.
func post(ctx context.Context, store PostingStore, guard PeriodGuard, in PostInput, release pendingRelease) (Transaction, error) {
	rec, err := recordOf(in)
	if err != nil {
		return Transaction{}, err
	}
	if original, found, err := originalOf(ctx, store, rec); found || err != nil {
		return original, err
	}
	// after looking up the reference: retrying something posted before the close returns the original
	if err := guard.EnsureOpen(ctx, rec.LegalEntityID, rec.BusinessDate); err != nil {
		return Transaction{}, err
	}
	tx, err := domain.NewTransaction(domain.NewTransactionInput{ID: rec.ID, Entries: in.Entries})
	if err != nil {
		return Transaction{}, err
	}
	accounts, err := accountsOf(ctx, store, rec.LegalEntityID, tx.Entries())
	if err != nil {
		return Transaction{}, err
	}
	deltas, err := netByAccount(tx.Entries())
	if err != nil {
		return Transaction{}, err
	}
	syncIDs := syncAccounts(accounts)
	if original, err := available(ctx, store, rec, accounts, syncIDs, deltas, release); err != nil {
		return original, err
	}
	return write(ctx, store, rec, tx.Entries(), syncIDs, deltas, release)
}

// available locks the balances and checks that the net fits. If it does not fit, it looks up the
// reference again: another producer may have posted it on these same accounts while their locks were
// awaited, and with its commit the balance no longer covers repeating it; then the posting is the
// original, not an overdraft. Any other error (a deadlock that aborts the transaction and UnitOfWork
// retries) propagates as is.
func available(ctx context.Context, store PostingStore, rec TransactionRecord, accounts map[ids.ID]domain.Account,
	syncIDs []ids.ID, deltas map[ids.ID]money.Amount, release pendingRelease,
) (Transaction, error) {
	err := checkAvailable(ctx, store, accounts, syncIDs, deltas, release)
	if !errors.Is(err, domain.ErrInsufficientFunds) {
		return Transaction{}, err
	}
	if original, found, lookupErr := originalOf(ctx, store, rec); found || lookupErr != nil {
		return original, lookupErr
	}
	return Transaction{}, err
}

// write posts the already validated transaction and adds its net to the sync balances, together with the
// pending amount it releases.
func write(ctx context.Context, store PostingStore, rec TransactionRecord, entries []domain.EntryInput, syncIDs []ids.ID,
	deltas map[ids.ID]money.Amount, release pendingRelease,
) (Transaction, error) {
	postedAt, err := store.InsertTransaction(ctx, rec, entries)
	if errors.Is(err, ErrDuplicateExternalRef) {
		original, found, lookupErr := originalOf(ctx, store, rec)
		if !found && lookupErr == nil { // the conflict proves it exists: without seeing it, fail closed, not as a duplicate
			lookupErr = fmt.Errorf("reference %s/%s conflicted on insert and the original is not visible", rec.Source, rec.ExternalRef)
		}
		return original, lookupErr
	}
	if err != nil {
		return Transaction{}, err
	}
	for _, id := range syncIDs {
		d := BalanceDelta{Posted: deltas[id].Units()}
		if id == release.account {
			d.PendingDebits, d.PendingCredits = -release.debits, -release.credits
		}
		if err := store.AddToBalance(ctx, id, d); err != nil {
			return Transaction{}, err
		}
	}
	return Transaction{TransactionRecord: rec, PostedAt: postedAt, Entries: entries}, nil
}

// originalOf looks up the transaction already posted with rec's reference. If there is one (found), err
// is ErrDuplicateExternalRef, which Post returns together with it.
func originalOf(ctx context.Context, store PostingStore, rec TransactionRecord) (Transaction, bool, error) {
	original, found, err := store.TransactionByRef(ctx, rec.LegalEntityID, rec.Source, rec.ExternalRef)
	if err != nil || !found {
		return Transaction{}, false, err
	}
	return original, true, fmt.Errorf("%w: %s/%s is transaction %s", ErrDuplicateExternalRef, rec.Source, rec.ExternalRef, original.ID)
}

// recordOf validates the header and brings it to its persisted form.
func recordOf(in PostInput) (TransactionRecord, error) {
	required := []struct{ field, value string }{
		{"code", in.Code},
		{"source", in.Source},
		{"external_ref", in.ExternalRef},
		{"actor", in.Actor},
		{"channel", in.Channel},
		{"rule_version", in.RuleVersion},
	}
	for _, r := range required {
		if r.value == "" {
			return TransactionRecord{}, fmt.Errorf("%w: missing %s", ErrInvalidPosting, r.field)
		}
	}
	if in.ValueDate.IsZero() || in.BusinessDate.IsZero() {
		return TransactionRecord{}, fmt.Errorf("%w: missing dates", ErrInvalidPosting)
	}
	return TransactionRecord{
		ID: ids.New(), LegalEntityID: in.LegalEntityID, Code: in.Code,
		ValueDate: civilDate(in.ValueDate), BusinessDate: civilDate(in.BusinessDate),
		Source: in.Source, ExternalRef: in.ExternalRef, Actor: in.Actor, Channel: in.Channel, RuleVersion: in.RuleVersion,
		ReversesID: in.ReversesID, Metadata: in.Metadata,
	}, nil
}

// civilDate keeps t's calendar day as UTC midnight: this way the Transaction Post returns equals the one
// read back from the database (zoneless date) and two dates on the same day compare equal even if written
// in different zones.
func civilDate(t time.Time) time.Time {
	y, m, d := t.Date()
	return time.Date(y, m, d, 0, 0, 0, 0, time.UTC)
}

// accountsOf loads the entries' accounts and checks that they belong to the entity and are in the entry's
// currency.
func accountsOf(ctx context.Context, store PostingStore, entity ids.ID, entries []domain.EntryInput) (map[ids.ID]domain.Account, error) {
	accountIDs := make([]ids.ID, 0, len(entries))
	for _, e := range entries {
		accountIDs = append(accountIDs, e.AccountID)
	}
	accounts, err := store.Accounts(ctx, accountIDs)
	if err != nil {
		return nil, err
	}
	for i, e := range entries {
		a, ok := accounts[e.AccountID]
		if !ok || a.LegalEntityID != entity {
			return nil, fmt.Errorf("%w: entry %d, account %s", ErrAccountNotFound, i, e.AccountID)
		}
		if a.Currency != e.Amount.Currency() {
			return nil, fmt.Errorf("%w: entry %d in %s, account %s in %s",
				ErrCurrencyMismatch, i, e.Amount.Currency().Code(), a.Code, a.Currency.Code())
		}
	}
	return accounts, nil
}

// netByAccount sums each account's entries: it is the net movement its balance receives.
func netByAccount(entries []domain.EntryInput) (map[ids.ID]money.Amount, error) {
	deltas := map[ids.ID]money.Amount{}
	for _, e := range entries {
		sum, ok := deltas[e.AccountID]
		if !ok {
			sum = money.New(0, e.Amount.Currency())
		}
		next, err := sum.Add(e.Amount)
		if err != nil {
			return nil, fmt.Errorf("account %s: %w", e.AccountID, err)
		}
		deltas[e.AccountID] = next
	}
	return deltas, nil
}

// syncAccounts returns the sync accounts. Async ones are neither locked nor checked; their balance is
// aggregated by AggregateAsyncBalances. The lock order is set by the query (ORDER BY account_id).
func syncAccounts(accounts map[ids.ID]domain.Account) []ids.ID {
	var syncIDs []ids.ID
	for id, a := range accounts {
		if a.Mode == domain.BalanceModeSync {
			syncIDs = append(syncIDs, id)
		}
	}
	return syncIDs
}

func checkAvailable(ctx context.Context, store PostingStore, accounts map[ids.ID]domain.Account, syncIDs []ids.ID,
	deltas map[ids.ID]money.Amount, release pendingRelease,
) error {
	balances, err := store.LockBalances(ctx, syncIDs)
	if err != nil {
		return err
	}
	for _, id := range syncIDs {
		b, ok := balances[id]
		if !ok { // CreateAccount always creates it; if it were missing, fail instead of checking against zero
			return fmt.Errorf("account %s has no materialized balance", id)
		}
		b = release.from(id, b)
		if err := accounts[id].CheckAvailable(b.Posted, b.PendingDebits, b.PendingCredits, deltas[id].Units()); err != nil {
			return err
		}
	}
	return nil
}
