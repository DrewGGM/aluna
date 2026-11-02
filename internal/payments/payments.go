// Package payments is the facade of payment orders (FR-30, FR-31): an outgoing payment is screened, held on its
// account with its charges and queued to be sent through its rail, whose outcome settles it against the rail's
// clearing account or rejects it and releases its hold.
package payments

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/riverdriver/riverpgxv5"

	"github.com/DrewGGM/aluna/internal/compliance"
	"github.com/DrewGGM/aluna/internal/deposits"
	"github.com/DrewGGM/aluna/internal/ids"
	"github.com/DrewGGM/aluna/internal/ledger"
	"github.com/DrewGGM/aluna/internal/money"
	"github.com/DrewGGM/aluna/internal/outbox"
	pg "github.com/DrewGGM/aluna/internal/payments/internal/adapters/postgres"
	"github.com/DrewGGM/aluna/internal/payments/internal/app"
	"github.com/DrewGGM/aluna/internal/payments/internal/domain"
	"github.com/DrewGGM/aluna/internal/postgres"
	"github.com/DrewGGM/aluna/internal/pricing"
)

// Types published by payments.
type (
	// Order is a payment order with its beneficiary, its hold and the charges it covers.
	Order = app.Order
	// InitiateInput is an outgoing payment to initiate.
	InitiateInput = app.InitiateInput
	// AccountRef is a party of a payment by account number or by key.
	AccountRef = app.AccountRef
	// Status is where an order stands.
	Status = domain.Status
)

// Statuses of an order.
const (
	StatusPendingScreening = domain.StatusPendingScreening
	StatusHeld             = domain.StatusHeld
	StatusSent             = domain.StatusSent
	StatusSettled          = domain.StatusSettled
	StatusRejected         = domain.StatusRejected
	StatusSuspended        = domain.StatusSuspended
)

// RailSimBreB is the simulated Bre-B rail, the only one for now.
const RailSimBreB = app.RailSimBreB

var (
	// ErrInvalidOrder means the payment's data is invalid.
	ErrInvalidOrder = domain.ErrInvalidOrder
	// ErrDuplicatePayment means the entity already made a payment with that reference.
	ErrDuplicatePayment = app.ErrDuplicatePayment
	// ErrRailNotConfigured means the entity has no clearing or suspense account for the rail in the payment's
	// currency: a payment through it cannot be held, nor a transfer received booked.
	ErrRailNotConfigured = errors.New("the rail has no clearing or suspense account in the entity")
)

// holdLife is how long a payment's hold lasts: the rail's outcome comes much sooner, and an order without one
// is reconciled, not released by its hold expiring.
const holdLife = 30 * 24 * time.Hour

// Queue is the River queue of the payments jobs.
const Queue = "payments"

// undoTimeout bounds undoing a payment that failed, which runs even if the request was canceled.
const undoTimeout = 5 * time.Second

// typeInitiated is the event of a new order; the others are of where it stands (eventType).
const typeInitiated = "aluna.payments.order.initiated.v1"

// eventType is the event of an order that moved to status (architecture §8): aluna.payments.order.held.v1, and
// so on.
func eventType(status Status) string { return "aluna.payments.order." + string(status) + ".v1" }

// Events publishes payments events; outbox.Outbox implements it.
type Events interface {
	Emit(ctx context.Context, tx postgres.Tx, e outbox.Event) error
}

// Service initiates payment orders, sends them through their rail and takes its outcome.
type Service struct {
	ledger     *ledger.Service
	deposits   *deposits.Service
	compliance *compliance.Service
	pricing    *pricing.Service
	events     Events
	jobs       *river.Client[pgx.Tx] // only inserts jobs: the worker's client works them
	rail       Rail
}

// New creates the service of rail, the only one; it publishes its events to events and queues its jobs in
// pool's River.
func New(l *ledger.Service, d *deposits.Service, c *compliance.Service, p *pricing.Service, events Events, pool *pgxpool.Pool, rail Rail,
) (*Service, error) {
	jobs, err := river.NewClient(riverpgxv5.New(pool), &river.Config{Schema: outbox.RiverSchema})
	if err != nil {
		return nil, fmt.Errorf("payments jobs: %w", err)
	}
	return &Service{ledger: l, deposits: d, compliance: c, pricing: p, events: events, jobs: jobs, rail: rail}, nil
}

// Initiate starts, within tx, an outgoing payment (FR-31): its beneficiary is screened (FR-42), and if screening
// does not clear it the order is pending_screening and nothing moves; if it does, its amount and its charges
// (by its quote if it names one) are held against the rail's clearing account and the order is queued to be
// sent (payments.send). An unknown rail, a beneficiary not by one account or one key, or another currency is
// ErrInvalidOrder; an account that is not active, deposits.ErrAccountNotActive; a reference already used,
// ErrDuplicatePayment; without funds for all of it, ledger.ErrInsufficientFunds. A rejection leaves nothing in
// tx, not even the screening case or the quote's use.
func (s *Service) Initiate(ctx context.Context, tx postgres.Tx, in InitiateInput) (Order, error) {
	if _, err := tx.Exec(ctx, "SAVEPOINT payment_initiation"); err != nil {
		return Order{}, fmt.Errorf("payment savepoint: %w", err)
	}
	o, err := app.Initiate(ctx, initiation{s: s, tx: tx}, in)
	if err != nil {
		return Order{}, undo(ctx, tx, err)
	}
	if _, err := tx.Exec(ctx, "RELEASE SAVEPOINT payment_initiation"); err != nil {
		return Order{}, fmt.Errorf("release the payment savepoint: %w", err)
	}
	return o, nil
}

// undo rolls tx back to the payment's savepoint, which failed with err, and releases it. If it cannot, it
// returns that error and not err: the payment may still be in tx, so it must not pass for a clean rejection.
func undo(ctx context.Context, tx postgres.Tx, err error) error {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), undoTimeout)
	defer cancel()
	if _, undoErr := tx.Exec(ctx, "ROLLBACK TO SAVEPOINT payment_initiation; RELEASE SAVEPOINT payment_initiation"); undoErr != nil {
		return fmt.Errorf("undo the payment that failed with %v: %w", err, undoErr) //nolint:errorlint // err must not match: the payment may not be undone
	}
	return err
}

// initiation is app.Initiation within tx.
type initiation struct {
	s  *Service
	tx postgres.Tx
}

func (i initiation) DepositAccount(ctx context.Context, entity, id ids.ID) (deposits.Account, error) {
	return i.s.deposits.Account(ctx, i.tx, entity, id)
}

func (i initiation) ScreenBeneficiary(ctx context.Context, o app.Order) (bool, error) {
	ref := o.Creditor.Key
	if ref == "" {
		ref = o.Creditor.Number
	}
	outcome, err := i.s.compliance.ScreenPayment(ctx, i.tx, o.LegalEntityID, o.ID,
		compliance.Subject{Kind: compliance.SubjectBeneficiary, ID: o.ID, ExternalRef: ref})
	return outcome == compliance.OutcomeClear, err
}

func (i initiation) Charges(ctx context.Context, a deposits.Account, amount money.Amount, quoteID ids.ID) (pricing.Charges, error) {
	if _, err := i.s.deposits.LockActive(ctx, i.tx, a.LegalEntityID, a.ID); err != nil { // only to lock and check: a's data do not change
		return pricing.Charges{}, err
	}
	if quoteID == (ids.ID{}) {
		in, err := i.s.deposits.QuoteInput(ctx, i.tx, a.LegalEntityID, a.ID, pricing.Transfer, amount)
		if err != nil {
			return pricing.Charges{}, err
		}
		return i.s.pricing.Charge(ctx, i.tx, in)
	}
	return i.s.pricing.ChargesFor(ctx, i.tx, a.LegalEntityID, pricing.QuoteUse{
		QuoteID: quoteID, AccountID: a.ID, PartyID: a.PartyID, Operation: pricing.Transfer, Amount: amount,
	})
}

func (i initiation) Hold(ctx context.Context, a deposits.Account, rail string, amount money.Amount) (ids.ID, error) {
	clearing, err := i.s.railAccount(ctx, i.tx, a.LegalEntityID, clearingAccount, rail, a.Currency)
	if err != nil {
		return ids.ID{}, err
	}
	h, err := i.s.ledger.PlaceHold(ctx, i.tx, ledger.PlaceHoldInput{
		LegalEntityID: a.LegalEntityID, AccountID: a.LedgerAccountID, CounterAccountID: clearing, Amount: amount,
		ExpiresAt: time.Now().Add(holdLife),
	})
	if err != nil {
		return ids.ID{}, err
	}
	return h.ID(), nil
}

// The accounts of a rail in an entity's ledger (railAccount).
const (
	clearingAccount = "CLEARING" // what the entity owes the rail, or the rail owes it, until they settle
	suspenseAccount = "SUSPENSE" // transfers received for no account the entity can credit
)

// railAccount is rail's account of kind in the entity's ledger, in currency c: L.<KIND>.<RAIL>, as
// L.CLEARING.SIMBREB for simbreb's clearing account. One that is missing or in another currency is
// ErrRailNotConfigured, the entity's misconfiguration and not the caller's.
func (s *Service) railAccount(ctx context.Context, tx postgres.Tx, entity ids.ID, kind, rail string, c money.Currency) (ids.ID, error) {
	code := "L." + kind + "." + strings.ToUpper(rail)
	account, err := s.ledger.AccountByCode(ctx, tx, entity, code)
	switch {
	case errors.Is(err, ledger.ErrAccountNotFound):
		return ids.ID{}, fmt.Errorf("%w: %s", ErrRailNotConfigured, code)
	case err != nil:
		return ids.ID{}, err
	case account.Currency != c:
		return ids.ID{}, fmt.Errorf("%w: %s is in %s", ErrRailNotConfigured, code, account.Currency.Code())
	}
	return account.ID, nil
}

func (i initiation) Record(ctx context.Context, o app.Order) error {
	if err := pg.NewStore(i.tx).InsertOrder(ctx, o); err != nil {
		return err
	}
	if err := i.s.emit(ctx, i.tx, typeInitiated, o); err != nil {
		return err
	}
	if o.Status != StatusHeld {
		return nil
	}
	if err := i.s.emit(ctx, i.tx, eventType(StatusHeld), o); err != nil {
		return err
	}
	if _, err := i.s.jobs.InsertTx(ctx, i.tx.Pgx(), sendArgs{OrderID: o.ID}, nil); err != nil {
		return fmt.Errorf("queue payment order %s: %w", o.ID, err)
	}
	return nil
}

// orderEvent is the data of an order's event: where it stands, nothing about its beneficiary or its payer. A
// transfer received for no account has none.
type orderEvent struct {
	OrderID    ids.ID           `json:"order_id"`
	AccountID  ids.ID           `json:"account_id,omitzero"`
	Direction  domain.Direction `json:"direction"`
	Status     Status           `json:"status"`
	EndToEndID string           `json:"end_to_end_id"`
	Amount     int64            `json:"amount"`
	Currency   string           `json:"currency"`
}

func (s *Service) emit(ctx context.Context, tx postgres.Tx, typ string, o Order) error {
	return s.events.Emit(ctx, tx, outbox.Event{
		Type: typ, Subject: "payment-orders/" + o.ID.String(), LegalEntityID: o.LegalEntityID, SchemaVersion: 1,
		Data: orderEvent{
			OrderID: o.ID, AccountID: o.AccountID, Direction: o.Direction, Status: o.Status, EndToEndID: o.EndToEndID, Amount: o.Amount.Units(),
			Currency: o.Amount.Currency().Code(),
		},
	})
}
