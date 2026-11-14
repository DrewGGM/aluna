package payments

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/riverqueue/river"

	"github.com/DrewGGM/aluna/internal/deposits"
	"github.com/DrewGGM/aluna/internal/ids"
	"github.com/DrewGGM/aluna/internal/ledger"
	"github.com/DrewGGM/aluna/internal/money"
	pg "github.com/DrewGGM/aluna/internal/payments/internal/adapters/postgres"
	"github.com/DrewGGM/aluna/internal/payments/internal/adapters/simrail"
	"github.com/DrewGGM/aluna/internal/payments/internal/app"
	"github.com/DrewGGM/aluna/internal/payments/internal/domain/postingrules"
	"github.com/DrewGGM/aluna/internal/postgres"
	"github.com/DrewGGM/aluna/internal/pricing"
)

// Types of a rail.
type (
	// Rail is a payment rail: it takes the core's messages and notifies their outcome.
	Rail = app.RailPort
	// OutboundMessage is what the core hands a rail of a credit transfer.
	OutboundMessage = app.OutboundMessage
	// ReturnMessage is what the core hands a rail to send a transfer it received back to its bank.
	ReturnMessage = app.ReturnMessage
	// SendAck is a rail taking a message.
	SendAck = app.SendAck
	// RailOutcome is a rail's word on how a message ended.
	RailOutcome = app.RailOutcome
	// Notification is a rail's notification as it reached the core.
	Notification = app.Notification
)

// How a message ended at the rail.
const (
	RailAccepted = app.RailAccepted
	RailRejected = app.RailRejected
	RailReturned = app.RailReturned
)

var (
	// ErrRailRefused means the rail did not take the message: the payment did not leave.
	ErrRailRefused = app.ErrRailRefused
	// ErrOrderNotFound means no order has that id or that end-to-end id.
	ErrOrderNotFound = app.ErrOrderNotFound
	// ErrUnverifiedNotification means a notification the rail did not sign, or signed too long ago (TH-7).
	ErrUnverifiedNotification = app.ErrUnverifiedNotification
	// ErrInvalidNotification means a notification of the rail whose outcome the core cannot read.
	ErrInvalidNotification = app.ErrInvalidNotification
	// ErrOutcomeConflict means the rail notified an order that already took another outcome.
	ErrOutcomeConflict = app.ErrOutcomeConflict
	// ErrNoAnswer means the rail did not answer whether it took an order or a return: a payment stays sent, with its
	// hold, and a transfer returned to its bank stays returned, until reconciliation (FR-34) learns what the rail did.
	ErrNoAnswer = errors.New("the rail did not answer")
)

// What the ledger records of a payment's capture and charges.
const (
	channelRail    = "rail"
	channelAPI     = "api" // a person, through the API, as an approver
	sourcePayments = "payments"
	codeCharges    = "payments.charges"
)

// NewSimulatedRail is the simulated Bre-B rail (AR-2): it notifies the core at baseURL with client, signing with
// secret, and logs to log the notifications the core did not take; the core verifies them with the same secret.
func NewSimulatedRail(baseURL string, secret []byte, client *http.Client, log *slog.Logger) Rail { //nolint:ireturn // the simulator is internal to payments: outside it is only a Rail
	return simrail.New(baseURL, secret, client, log)
}

// Send hands held order id to its rail (FR-31), the work of payments.send, within uow. It first moves the order
// to sent and publishes it, in its own transaction: the rail's outcome may arrive before the rail answers. An
// order that is not held is not sent. If the rail does not take it, the order is rejected and its hold
// released; if the rail does not answer within 15 seconds, the order stays sent with its hold, and it is
// ErrNoAnswer.
func (s *Service) Send(ctx context.Context, uow *postgres.UnitOfWork, id ids.ID) error {
	var (
		o       app.Order
		started bool
	)
	if err := uow.Do(ctx, func(tx postgres.Tx) error {
		var err error
		o, started, err = app.StartSending(ctx, orders{s: s, tx: tx}, id)
		return err
	}); err != nil || !started {
		return err
	}
	_, err := app.Send(ctx, s.rail, app.Message(o))
	switch {
	case errors.Is(err, app.ErrRailRefused):
		refused := app.RailOutcome{EndToEndID: o.EndToEndID, TxID: o.TxID, Status: app.RailRejected, Reason: app.ReasonRailRefused}
		return uow.Do(ctx, func(tx postgres.Tx) error { return app.TakeOutcome(ctx, orders{s: s, tx: tx}, refused, s.policy) })
	case err != nil:
		return fmt.Errorf("%w: order %s: %w", ErrNoAnswer, o.ID, err)
	}
	return nil
}

// ReadNotification is the outcome the rail's notification n carries (TH-7), read without touching the database:
// one the rail did not sign within 5 minutes is ErrUnverifiedNotification, and one whose body is not a
// notification ErrInvalidNotification.
func (s *Service) ReadNotification(n Notification) (RailOutcome, error) { return s.rail.Outcome(n) }

// TakeOutcome applies, within tx, the rail's outcome to its order, which it finds by its end-to-end id (FR-31):
// accepted, the order settles and its hold is captured against the rail's clearing account, with its charges
// posted to their accounts; rejected, the order is rejected for the rail's reason and its hold released, with what
// it used of the GMF exemption; returned, a settled order is returned for the rail's reason and its amount comes
// back from the clearing account to its account, or to the rail's suspense account if the account is no longer
// active, with the charges the entity's policy refunds (ADR-0016). An order that already took that outcome does
// not change; another outcome is ErrOutcomeConflict, an unknown order ErrOrderNotFound, and an outcome that is none
// of them, or whose reference or reason is not text of up to 255 characters, ErrInvalidNotification.
func (s *Service) TakeOutcome(ctx context.Context, tx postgres.Tx, out RailOutcome) error {
	if out.Status == RailReturned {
		return app.TakeReturn(ctx, returns{orders{s: s, tx: tx}}, out, s.policy)
	}
	return app.TakeOutcome(ctx, orders{s: s, tx: tx}, out, s.policy)
}

// returns is app.Returning within tx: the outgoing orders.
type returns struct{ orders }

// Book posts entries on the entity's business date, by the rail, with the order's return as their reference,
// restores restore of the GMF exemption o used, under its account's lock taken before the ledger's (the lock of an
// active account is already held), and stores and publishes o.
func (r returns) Book(ctx context.Context, o app.Order, code string, entries []ledger.EntryInput, restore pricing.ExemptionUse) error {
	if restore.Amount.Sign() > 0 {
		a, err := r.s.deposits.LockAccount(ctx, r.tx, o.LegalEntityID, o.AccountID)
		if err != nil {
			return err
		}
		if err := r.s.pricing.RestoreExemption(ctx, r.tx, o.LegalEntityID, a.PartyID, restore); err != nil {
			return err
		}
	}
	if err := r.postToday(ctx, o, code, "return", o.Rail, channelRail, entries); err != nil {
		return err
	}
	return r.Update(ctx, o)
}

// AddWorkers registers in w the workers of payments.send and payments.return, which work within uow. A send without
// the rail's answer is canceled, not retried: the rail may have taken it, and reconciliation finds out.
func (s *Service) AddWorkers(w *river.Workers, uow *postgres.UnitOfWork) {
	river.AddWorker(w, &sendWorker{s: s, uow: uow})
	river.AddWorker(w, &returnWorker{s: s, uow: uow})
}

// sendArgs is the job that sends a held order through its rail (Send).
type sendArgs struct {
	OrderID ids.ID `json:"order_id"`
}

func (sendArgs) Kind() string { return "payments.send" }

func (sendArgs) InsertOpts() river.InsertOpts { return river.InsertOpts{Queue: Queue} }

type sendWorker struct {
	river.WorkerDefaults[sendArgs]
	s   *Service
	uow *postgres.UnitOfWork
}

func (w *sendWorker) Work(ctx context.Context, job *river.Job[sendArgs]) error {
	err := w.s.Send(ctx, w.uow, job.Args.OrderID)
	if errors.Is(err, ErrNoAnswer) {
		return river.JobCancel(err)
	}
	return err
}

// orders is the payment orders within tx: app.Sending, app.Settling and app.Expiring, and what the adapters that
// embed it share.
type orders struct {
	s  *Service
	tx postgres.Tx
}

func (r orders) LockActive(ctx context.Context, entity, id ids.ID) (deposits.Account, error) {
	return r.s.deposits.LockActive(ctx, r.tx, entity, id)
}

func (r orders) CountCredit(ctx context.Context, a deposits.Account, amount money.Amount) error {
	return r.s.deposits.CountCredit(ctx, r.tx, a, amount)
}

func (r orders) RailAccounts(ctx context.Context, entity ids.ID, rail string, c money.Currency) (clearing, suspense ids.ID, err error) {
	if clearing, err = r.s.railAccount(ctx, r.tx, entity, clearingAccount, rail, c); err != nil {
		return ids.ID{}, ids.ID{}, err
	}
	if suspense, err = r.s.railAccount(ctx, r.tx, entity, suspenseAccount, rail, c); err != nil {
		return ids.ID{}, ids.ID{}, err
	}
	return clearing, suspense, nil
}

// postToday posts entries, a movement of order o after it was made (its return, its credit from suspense), as a
// transaction of code on the entity's business date, with the payments rules; its reference is the order's id and
// what the movement is, so that each posts once.
func (r orders) postToday(ctx context.Context, o app.Order, code, what, actor, channel string, entries []ledger.EntryInput) error {
	today, err := r.s.ledger.BusinessDate(ctx, r.tx, o.LegalEntityID)
	if err != nil {
		return err
	}
	if _, err := r.s.ledger.Post(ctx, r.tx, ledger.PostInput{
		LegalEntityID: o.LegalEntityID, Code: code, ValueDate: today, BusinessDate: today, Source: sourcePayments,
		ExternalRef: o.ID.String() + "/" + what, Actor: actor, Channel: channel, RuleVersion: postingrules.RuleVersion, Entries: entries,
	}); err != nil {
		return fmt.Errorf("book the %s of order %s: %w", what, o.ID, err)
	}
	return nil
}

func (r orders) LockOrder(ctx context.Context, id ids.ID) (app.Order, error) {
	return pg.NewStore(r.tx).LockOrder(ctx, id)
}

func (r orders) LockStalePending(ctx context.Context, before time.Time, limit int) ([]ids.ID, error) {
	return pg.NewStore(r.tx).LockStalePending(ctx, before, limit)
}

func (r orders) LockByEndToEndID(ctx context.Context, e2e string) (app.Order, error) {
	return pg.NewStore(r.tx).LockByEndToEndID(ctx, e2e)
}

// Update stores where o stands and publishes it as aluna.payments.order.<status>.v1.
func (r orders) Update(ctx context.Context, o app.Order) error {
	if err := pg.NewStore(r.tx).UpdateOrder(ctx, o); err != nil {
		return err
	}
	return r.s.emit(ctx, r.tx, eventType(o.Status), o)
}

func (r orders) Accounts(ctx context.Context, o app.Order) (app.ChargeAccounts, error) {
	a, err := r.s.deposits.Account(ctx, r.tx, o.LegalEntityID, o.AccountID)
	if err != nil {
		return app.ChargeAccounts{}, err
	}
	accounts := app.ChargeAccounts{Customer: a.LedgerAccountID}
	if len(o.Fees) > 0 {
		if accounts.FeeIncome, err = r.s.deposits.FeeIncome(ctx, r.tx, a); err != nil {
			return app.ChargeAccounts{}, err
		}
	}
	if o.GMF.Sign() > 0 {
		if accounts.GMFPayable, err = r.s.deposits.GMFPayable(ctx, r.tx, a); err != nil {
			return app.ChargeAccounts{}, err
		}
	}
	return accounts, nil
}

// Capture posts the capture of o's amount, with the ledger's rule, and then its charges on the same business
// date, with the payments rules, the order as their reference.
func (r orders) Capture(ctx context.Context, o app.Order, charges []ledger.EntryInput) error {
	_, captured, err := r.s.ledger.CaptureHold(ctx, r.tx, ledger.CaptureInput{
		LegalEntityID: o.LegalEntityID, HoldID: o.HoldID, Amount: o.Amount, Actor: o.Rail, Channel: channelRail,
	})
	if err != nil || len(charges) == 0 {
		return err
	}
	_, err = r.s.ledger.Post(ctx, r.tx, ledger.PostInput{
		LegalEntityID: o.LegalEntityID, Code: codeCharges, ValueDate: captured.BusinessDate, BusinessDate: captured.BusinessDate,
		Source: sourcePayments, ExternalRef: o.ID.String(), Actor: o.Rail, Channel: channelRail, RuleVersion: postingrules.RuleVersion,
		Entries: charges,
	})
	return err
}

// Release gives back the debit o counted and restores restore of the GMF exemption it used, under its account's
// lock taken before the ledger's as every movement does, and releases its hold. An order held before migration
// 0036 has no counted date, and one before 0038 no exemption used: there is nothing of them to give back.
func (r orders) Release(ctx context.Context, o app.Order, restore pricing.ExemptionUse) error {
	if !o.DebitCountedOn.IsZero() || restore.Amount.Sign() > 0 {
		a, err := r.s.deposits.LockAccount(ctx, r.tx, o.LegalEntityID, o.AccountID)
		if err != nil {
			return err
		}
		if err := r.giveBack(ctx, a, o, restore); err != nil {
			return err
		}
	}
	_, err := r.s.ledger.ReleaseHold(ctx, r.tx, ledger.ReleaseInput{LegalEntityID: o.LegalEntityID, HoldID: o.HoldID})
	return err
}

// giveBack gives back to a, locked, the debit o counted, if it counted one, and restores restore of its party's
// GMF exemption.
func (r orders) giveBack(ctx context.Context, a deposits.Account, o app.Order, restore pricing.ExemptionUse) error {
	if !o.DebitCountedOn.IsZero() {
		if err := r.s.deposits.ReleaseDebit(ctx, r.tx, a, o.Amount, o.DebitCountedOn); err != nil {
			return err
		}
	}
	return r.s.pricing.RestoreExemption(ctx, r.tx, o.LegalEntityID, a.PartyID, restore)
}
