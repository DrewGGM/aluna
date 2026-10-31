// Package ledger is the facade of the double-entry ledger (ADR-0003): create accounts, post balanced
// transactions and read balances. The other modules import only this package.
package ledger

import (
	"context"
	"fmt"
	"time"

	"github.com/DrewGGM/aluna/internal/calendar"
	"github.com/DrewGGM/aluna/internal/ids"
	pg "github.com/DrewGGM/aluna/internal/ledger/internal/adapters/postgres"
	"github.com/DrewGGM/aluna/internal/ledger/internal/app"
	"github.com/DrewGGM/aluna/internal/ledger/internal/domain"
	"github.com/DrewGGM/aluna/internal/outbox"
	"github.com/DrewGGM/aluna/internal/postgres"
)

// Types published by the ledger.
type (
	// Account is a ledger account.
	Account = domain.Account
	// Kind is an account's accounting kind.
	Kind = domain.Kind
	// BalanceMode says how an account's balance is maintained.
	BalanceMode = domain.BalanceMode
	// EntryInput is one line of a posting: debit positive, credit negative.
	EntryInput = domain.EntryInput
	// CreateAccountInput is the data for a new account.
	CreateAccountInput = app.CreateAccountInput
	// PostInput is a posting: header and entries.
	PostInput = app.PostInput
	// ReverseInput asks to reverse a transaction.
	ReverseInput = app.ReverseInput
	// Hold is a two-phase reservation of funds on an account, with its counterpart.
	Hold = app.Hold
	// HoldStatus is a hold's status.
	HoldStatus = domain.HoldStatus
	// PlaceHoldInput asks to hold funds.
	PlaceHoldInput = app.PlaceHoldInput
	// CaptureInput asks to post part or all of a hold.
	CaptureInput = app.CaptureInput
	// ReleaseInput asks to release a hold.
	ReleaseInput = app.ReleaseInput
	// Transaction is a posted transaction: header, the database's posted_at and entries.
	Transaction = app.Transaction
	// Balance is an account's materialized, signed balance.
	Balance = app.Balance
	// BalanceAsOf is the bitemporal balance cut: value date and the moment it was known.
	BalanceAsOf = app.BalanceAsOf
)

// Accounting kinds and balance modes.
const (
	KindAsset        = domain.KindAsset
	KindLiability    = domain.KindLiability
	KindEquity       = domain.KindEquity
	KindIncome       = domain.KindIncome
	KindExpense      = domain.KindExpense
	BalanceModeSync  = domain.BalanceModeSync
	BalanceModeAsync = domain.BalanceModeAsync
	HoldPending      = domain.HoldPending
	HoldCaptured     = domain.HoldCaptured
	HoldReleased     = domain.HoldReleased
	HoldExpired      = domain.HoldExpired
)

// Errors the ledger returns.
var (
	ErrInsufficientFunds    = domain.ErrInsufficientFunds
	ErrUnbalanced           = domain.ErrUnbalanced
	ErrTooFewEntries        = domain.ErrTooFewEntries
	ErrZeroAmount           = domain.ErrZeroAmount
	ErrAccountNotFound      = app.ErrAccountNotFound
	ErrInvalidPosting       = app.ErrInvalidPosting
	ErrCurrencyMismatch     = app.ErrCurrencyMismatch
	ErrInvalidAccount       = app.ErrInvalidAccount
	ErrTransactionNotFound  = app.ErrTransactionNotFound
	ErrDuplicateExternalRef = app.ErrDuplicateExternalRef
	ErrAlreadyReversed      = app.ErrAlreadyReversed
	ErrHoldNotFound         = app.ErrHoldNotFound
	ErrInvalidHold          = domain.ErrInvalidHold
	ErrHoldNotPending       = domain.ErrHoldNotPending
	ErrAmountExceedsHold    = domain.ErrAmountExceedsHold
	ErrHoldExpired          = domain.ErrHoldExpired
	ErrPeriodClosed         = calendar.ErrPeriodClosed
	ErrLegalEntityNotFound  = calendar.ErrLegalEntityNotFound
)

// NaturalBalance converts a signed balance to the account's normal side: a customer deposit (liability)
// shows as positive.
func NaturalBalance(k Kind, posted int64) int64 { return domain.NaturalBalance(k, posted) }

// Service is the entry point to the ledger for the other modules and the inbound adapters.
type Service struct {
	cal    Calendar
	events Events
}

// Events publishes an event within the command's transaction (ADR-0007). outbox.Outbox implements it;
// it is wired in cmd/aluna.
type Events interface {
	Emit(ctx context.Context, tx postgres.Tx, e outbox.Event) error
}

// Calendar is what the ledger needs from the calendar: each entity's business date and the period guard.
// calendar.Service implements it; it is wired in cmd/aluna.
type Calendar interface {
	PeriodGuard
	// BusinessDate is the entity's business date, or calendar.ErrLegalEntityNotFound.
	BusinessDate(ctx context.Context, q postgres.Querier, legalEntity ids.ID) (calendar.Date, error)
}

// PeriodGuard rejects posting with a business date inside a closed period (FR-15).
type PeriodGuard interface {
	// EnsureOpen is calendar.ErrPeriodClosed if d falls in a closed period of the entity.
	EnsureOpen(ctx context.Context, q postgres.Querier, legalEntity ids.ID, d calendar.Date) error
}

// New creates the ledger service on top of the entities' calendar; it publishes its events to events.
func New(cal Calendar, events Events) *Service { return &Service{cal: cal, events: events} }

// CreateAccount creates an account within tx.
func (s *Service) CreateAccount(ctx context.Context, tx postgres.Tx, in CreateAccountInput) (Account, error) {
	return app.CreateAccount(ctx, pg.NewStore(tx), in)
}

// Post posts a balanced transaction within tx: it locks the balances of its sync accounts, checks the
// limits and updates the balances in the same database transaction. If the entity already posted
// (source, external_ref), it returns that original transaction with ErrDuplicateExternalRef and writes
// nothing: an internal producer that retries takes it as done.
func (s *Service) Post(ctx context.Context, tx postgres.Tx, in PostInput) (Transaction, error) {
	var err error
	if in.BusinessDate, err = s.businessDay(ctx, tx, in.LegalEntityID, in.BusinessDate); err != nil {
		return Transaction{}, err
	}
	return app.Post(ctx, pg.NewStore(tx), s.guard(tx), s.emitter(tx), in)
}

// Reverse posts within tx the reversal of one of the entity's transactions: its entries with the
// opposite sign, subject to the same limits as a posting. If it already has a reversal, it returns it
// with ErrAlreadyReversed and writes nothing.
func (s *Service) Reverse(ctx context.Context, tx postgres.Tx, in ReverseInput) (Transaction, error) {
	var err error
	if in.BusinessDate, err = s.businessDay(ctx, tx, in.LegalEntityID, in.BusinessDate); err != nil {
		return Transaction{}, err
	}
	return app.Reverse(ctx, pg.NewStore(tx), s.guard(tx), s.emitter(tx), in)
}

// PlaceHold holds funds of a sync account within tx: it checks the available balance and adds the hold
// to its pending balance.
func (s *Service) PlaceHold(ctx context.Context, tx postgres.Tx, in PlaceHoldInput) (Hold, error) {
	return app.PlaceHold(ctx, pg.NewStore(tx), s.emitter(tx), in)
}

// CaptureHold posts within tx part or all of a pending hold and releases the rest; it returns the hold
// and the actual transaction.
func (s *Service) CaptureHold(ctx context.Context, tx postgres.Tx, in CaptureInput) (Hold, Transaction, error) {
	var err error
	if in.BusinessDate, err = s.businessDay(ctx, tx, in.LegalEntityID, in.BusinessDate); err != nil {
		return Hold{}, Transaction{}, err
	}
	return app.CaptureHold(ctx, pg.NewStore(tx), s.guard(tx), s.emitter(tx), in)
}

// businessDay is the business date to post with: given, or the entity's in the calendar if given is zero
// (read at the start, in the same tx, without blocking the rollover).
func (s *Service) businessDay(ctx context.Context, tx postgres.Tx, entity ids.ID, given time.Time) (time.Time, error) {
	if !given.IsZero() {
		return given, nil
	}
	d, err := s.cal.BusinessDate(ctx, tx, entity)
	if err != nil {
		return time.Time{}, err
	}
	return time.Date(d.Year, d.Month, d.Day, 0, 0, 0, 0, time.UTC), nil
}

// guard is the calendar's period guard within tx.
func (s *Service) guard(tx postgres.Tx) periodGuard { return periodGuard{s.cal, tx} }

type periodGuard struct {
	cal PeriodGuard
	q   postgres.Querier
}

// EnsureOpen is calendar.ErrPeriodClosed if businessDate's calendar day falls in a closed period.
func (g periodGuard) EnsureOpen(ctx context.Context, entity ids.ID, businessDate time.Time) error {
	return g.cal.EnsureOpen(ctx, g.q, entity, calendar.Date{Year: businessDate.Year(), Month: businessDate.Month(), Day: businessDate.Day()})
}

// ReleaseHold releases a pending hold within tx.
func (s *Service) ReleaseHold(ctx context.Context, tx postgres.Tx, in ReleaseInput) (Hold, error) {
	return app.ReleaseHold(ctx, pg.NewStore(tx), s.emitter(tx), in)
}

// ExpireDueHolds expires the pending holds due at now, from every entity, in batches of batch: each batch
// in its own transaction, until one comes out incomplete. Holds locked by a capture or a release in
// progress are skipped; if they are still pending, a later run takes them. It returns how many it
// expired, also when a batch fails (the earlier ones are already committed).
func (s *Service) ExpireDueHolds(ctx context.Context, uow *postgres.UnitOfWork, now time.Time, batch int) (int, error) {
	if batch < 1 {
		return 0, fmt.Errorf("expiry batch %d: must be positive", batch)
	}
	expired := 0
	for {
		var n int
		err := uow.Do(ctx, func(tx postgres.Tx) error {
			var err error
			n, err = app.ExpireDueHolds(ctx, pg.NewStore(tx), s.emitter(tx), now, batch)
			return err
		})
		if err != nil {
			return expired, err
		}
		expired += n
		if n < batch {
			return expired, nil
		}
	}
}

// AggregateAsyncBalances adds to the async accounts' balances the entries not yet aggregated (FR-9), in
// batches of batch entries, each in its own transaction, until one comes out incomplete. It returns how
// many entries it aggregated, also when a batch fails (the earlier ones are already committed).
func (s *Service) AggregateAsyncBalances(ctx context.Context, uow *postgres.UnitOfWork, batch int) (int, error) {
	if batch < 1 {
		return 0, fmt.Errorf("aggregation batch %d: must be positive", batch)
	}
	aggregated := 0
	for {
		var n int
		err := uow.Do(ctx, func(tx postgres.Tx) error {
			var err error
			n, err = app.AggregateAsyncBalances(ctx, pg.NewStore(tx), batch)
			return err
		})
		if err != nil {
			return aggregated, err
		}
		aggregated += n
		if n < batch {
			return aggregated, nil
		}
	}
}

// Account reads one of the entity's accounts: another entity's account does not exist for it. q can be
// the pool or a Tx.
func (s *Service) Account(ctx context.Context, q postgres.Querier, entity, id ids.ID) (Account, error) {
	return pg.NewStore(q).AccountOf(ctx, entity, id)
}

// Transaction reads one of the entity's posted transactions, with its entries. q can be the pool or a
// Tx.
func (s *Service) Transaction(ctx context.Context, q postgres.Querier, entity, id ids.ID) (Transaction, error) {
	return pg.NewStore(q).TransactionOf(ctx, entity, id)
}

// PostedAsOf sums an account's entries with a value date up to at.ValueDate and posted up to at.KnownAt
// (FR-8): the signed balance as it was known at that moment. It checks neither the account nor its
// entity: one that does not exist sums zero, so the caller reads it first with Account, as the handler
// does. q can be the pool or a Tx.
func (s *Service) PostedAsOf(ctx context.Context, q postgres.Querier, accountID ids.ID, at BalanceAsOf) (int64, error) {
	return pg.NewStore(q).PostedAsOf(ctx, accountID, at)
}

// Balance reads an account's materialized balance, without locking it. q can be the pool or a Tx.
func (s *Service) Balance(ctx context.Context, q postgres.Querier, accountID ids.ID) (Balance, error) {
	return pg.NewStore(q).Balance(ctx, accountID)
}

// emitter is the ledger's events within tx.
func (s *Service) emitter(tx postgres.Tx) emitter { return emitter{s.events, tx} }

type emitter struct {
	events Events
	tx     postgres.Tx
}

// Emit publishes e within the command's transaction.
func (e emitter) Emit(ctx context.Context, ev app.Event) error {
	return e.events.Emit(ctx, e.tx, outbox.Event{
		Type: ev.Type, Subject: ev.Subject, LegalEntityID: ev.LegalEntityID, SchemaVersion: ev.SchemaVersion, Data: ev.Data,
	})
}
