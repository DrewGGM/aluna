// Package payments is the facade of payment orders (FR-30, FR-31): an outgoing payment is screened, held on its
// account with its charges and queued to be sent through its rail.
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

// Statuses an order is initiated into.
const (
	StatusPendingScreening = domain.StatusPendingScreening
	StatusHeld             = domain.StatusHeld
)

// RailSimBreB is the simulated Bre-B rail, the only one for now.
const RailSimBreB = app.RailSimBreB

var (
	// ErrInvalidOrder means the payment's data is invalid.
	ErrInvalidOrder = domain.ErrInvalidOrder
	// ErrDuplicatePayment means the entity already made a payment with that reference.
	ErrDuplicatePayment = app.ErrDuplicatePayment
	// ErrRailNotConfigured means the entity has no clearing account for the rail in the payment's currency: a
	// payment through it cannot be held.
	ErrRailNotConfigured = errors.New("the rail has no clearing account in the entity")
)

// holdLife is how long a payment's hold lasts: the rail's outcome comes much sooner, and an order without one
// is reconciled, not released by its hold expiring.
const holdLife = 30 * 24 * time.Hour

// queuePayments is the River queue of the payments jobs.
const queuePayments = "payments"

// undoTimeout bounds undoing a payment that failed, which runs even if the request was canceled.
const undoTimeout = 5 * time.Second

// Events of an order (architecture §8).
const (
	typeInitiated = "aluna.payments.order.initiated.v1"
	typeHeld      = "aluna.payments.order.held.v1"
)

// Events publishes payments events; outbox.Outbox implements it.
type Events interface {
	Emit(ctx context.Context, tx postgres.Tx, e outbox.Event) error
}

// Service initiates payment orders.
type Service struct {
	ledger     *ledger.Service
	deposits   *deposits.Service
	compliance *compliance.Service
	pricing    *pricing.Service
	events     Events
	jobs       *river.Client[pgx.Tx] // only inserts jobs: the worker's client works them
}

// New creates the service; it publishes its events to events and queues its jobs in pool's River.
func New(l *ledger.Service, d *deposits.Service, c *compliance.Service, p *pricing.Service, events Events, pool *pgxpool.Pool) (*Service, error) {
	jobs, err := river.NewClient(riverpgxv5.New(pool), &river.Config{Schema: outbox.RiverSchema})
	if err != nil {
		return nil, fmt.Errorf("payments jobs: %w", err)
	}
	return &Service{ledger: l, deposits: d, compliance: c, pricing: p, events: events, jobs: jobs}, nil
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
	clearing, err := i.clearing(ctx, a, rail)
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

// clearing is rail's clearing account in a's entity and currency: L.CLEARING.SIMBREB for simbreb. One that is
// missing or in another currency is ErrRailNotConfigured, the entity's misconfiguration and not the caller's.
func (i initiation) clearing(ctx context.Context, a deposits.Account, rail string) (ids.ID, error) {
	code := "L.CLEARING." + strings.ToUpper(rail)
	clearing, err := i.s.ledger.AccountByCode(ctx, i.tx, a.LegalEntityID, code)
	switch {
	case errors.Is(err, ledger.ErrAccountNotFound):
		return ids.ID{}, fmt.Errorf("%w: %s", ErrRailNotConfigured, code)
	case err != nil:
		return ids.ID{}, err
	case clearing.Currency != a.Currency:
		return ids.ID{}, fmt.Errorf("%w: %s is in %s", ErrRailNotConfigured, code, clearing.Currency.Code())
	}
	return clearing.ID, nil
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
	if err := i.s.emit(ctx, i.tx, typeHeld, o); err != nil {
		return err
	}
	if _, err := i.s.jobs.InsertTx(ctx, i.tx.Pgx(), sendArgs{OrderID: o.ID}, nil); err != nil {
		return fmt.Errorf("queue payment order %s: %w", o.ID, err)
	}
	return nil
}

// sendArgs is the job that sends a held order through its rail (T-062 works it).
type sendArgs struct {
	OrderID ids.ID `json:"order_id"`
}

func (sendArgs) Kind() string { return "payments.send" }

func (sendArgs) InsertOpts() river.InsertOpts { return river.InsertOpts{Queue: queuePayments} }

// orderEvent is the data of an order's event: where it stands, nothing about its beneficiary.
type orderEvent struct {
	OrderID    ids.ID `json:"order_id"`
	AccountID  ids.ID `json:"account_id"`
	Status     Status `json:"status"`
	EndToEndID string `json:"end_to_end_id"`
	Amount     int64  `json:"amount"`
	Currency   string `json:"currency"`
}

func (s *Service) emit(ctx context.Context, tx postgres.Tx, typ string, o Order) error {
	return s.events.Emit(ctx, tx, outbox.Event{
		Type: typ, Subject: "payment-orders/" + o.ID.String(), LegalEntityID: o.LegalEntityID, SchemaVersion: 1,
		Data: orderEvent{
			OrderID: o.ID, AccountID: o.AccountID, Status: o.Status, EndToEndID: o.EndToEndID, Amount: o.Amount.Units(),
			Currency: o.Amount.Currency().Code(),
		},
	})
}
