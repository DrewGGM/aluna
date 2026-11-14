package payments

import (
	"context"
	"errors"
	"fmt"

	"github.com/riverqueue/river"

	"github.com/DrewGGM/aluna/internal/ids"
	"github.com/DrewGGM/aluna/internal/ledger"
	"github.com/DrewGGM/aluna/internal/payments/internal/app"
	"github.com/DrewGGM/aluna/internal/payments/internal/domain"
	"github.com/DrewGGM/aluna/internal/payments/internal/domain/postingrules"
	"github.com/DrewGGM/aluna/internal/postgres"
)

// ErrNotSuspended means an order that is not a transfer held in suspense: there is nothing to credit or return.
var ErrNotSuspended = app.ErrNotSuspended

// Suspended is order id of entity if it is a transfer held in suspense, locked within tx: ErrOrderNotFound if the
// entity has no such order, ErrNotSuspended if it is any other.
func (s *Service) Suspended(ctx context.Context, tx postgres.Tx, entity, id ids.ID) (Order, error) {
	return app.LockSuspended(ctx, orders{s: s, tx: tx}, entity, id)
}

// CreditSuspended credits, within tx, transfer id of entity, held in suspense, to deposit account account, which
// someone found it was for, by actor, the person who approved it (FR-32): its amount moves from the rail's suspense
// account to the account, and the order settles for it, with its event. ErrOrderNotFound or ErrNotSuspended as
// Suspended; an account that is not active, deposits.ErrAccountNotActive, or unknown,
// deposits.ErrAccountNotFound; one in another currency, ErrInvalidOrder; over a limit of its product,
// deposits.ErrLimitExceeded.
func (s *Service) CreditSuspended(ctx context.Context, tx postgres.Tx, entity, id, account ids.ID, actor string) (Order, error) {
	return app.CreditSuspended(ctx, crediting{orders{s: s, tx: tx}}, entity, id, account, actor)
}

// ReturnSuspended returns, within tx, transfer id of entity, held in suspense and of no customer of it, to the bank
// that sent it, for reason, by actor, the person who approved it (FR-32): its amount moves from the rail's suspense
// account back to its clearing account, the order is returned with its event, and a payments.return job hands the
// return to the rail once tx commits. ErrOrderNotFound or ErrNotSuspended as Suspended; a reason
// CheckReturnReason refuses, ErrInvalidOrder.
func (s *Service) ReturnSuspended(ctx context.Context, tx postgres.Tx, entity, id ids.ID, reason, actor string) (Order, error) {
	return app.ReturnSuspended(ctx, returningToSender{orders{s: s, tx: tx}}, entity, id, reason, actor)
}

// CheckReturnReason is ErrInvalidOrder, wrapped, unless reason is what the rail carries as the reason of a return to
// the sender: an ISO 20022 code, such as AC01, or a short text of its restricted set.
func CheckReturnReason(reason string) error { return domain.CheckReturnReason(reason) }

// returningToSender is app.ReturningToSender within tx: the orders, returned to their banks.
type returningToSender struct{ orders }

// Book posts entries on the entity's business date, by actor through the API, with the order's return as their
// reference, stores and publishes o, and queues its return to the rail.
func (r returningToSender) Book(ctx context.Context, o app.Order, actor string, entries []ledger.EntryInput) error {
	if err := r.postToday(ctx, o, postingrules.CodeReturnToSender, "return", actor, channelAPI, entries); err != nil {
		return err
	}
	if err := r.Update(ctx, o); err != nil {
		return err
	}
	if _, err := r.s.jobs.InsertTx(ctx, r.tx.Pgx(), returnArgs{OrderID: o.ID}, nil); err != nil {
		return fmt.Errorf("queue the return of transfer %s: %w", o.ID, err)
	}
	return nil
}

// HandReturn hands the return of transfer id, returned to its bank, to its rail (the work of payments.return),
// reading it within uow. If the rail does not take it, it is ErrRailRefused; if it does not answer within 15
// seconds, ErrNoAnswer: the rail may have taken it.
func (s *Service) HandReturn(ctx context.Context, uow *postgres.UnitOfWork, id ids.ID) error {
	var o app.Order
	if err := uow.Do(ctx, func(tx postgres.Tx) error {
		var err error
		o, err = orders{s: s, tx: tx}.LockOrder(ctx, id)
		return err
	}); err != nil {
		return err
	}
	err := app.HandReturn(ctx, s.rail, app.ReturnMessageOf(o))
	if err != nil && !errors.Is(err, app.ErrRailRefused) {
		return fmt.Errorf("%w: return of transfer %s: %w", ErrNoAnswer, id, err)
	}
	return err
}

// returnArgs is the job that hands a transfer returned to its bank to the rail (HandReturn).
type returnArgs struct {
	OrderID ids.ID `json:"order_id"`
}

func (returnArgs) Kind() string { return "payments.return" }

func (returnArgs) InsertOpts() river.InsertOpts { return river.InsertOpts{Queue: Queue} }

// returnWorker works payments.return. A return the rail refused is canceled: handed again, it is refused again. One
// the rail did not answer is canceled too, not retried: the rail may have taken it, and handing it again could pay
// the bank twice. Either way the rail's statement of the day lacks it, and reconciliation opens a break.
type returnWorker struct {
	river.WorkerDefaults[returnArgs]
	s   *Service
	uow *postgres.UnitOfWork
}

func (w *returnWorker) Work(ctx context.Context, job *river.Job[returnArgs]) error {
	err := w.s.HandReturn(ctx, w.uow, job.Args.OrderID)
	if errors.Is(err, ErrRailRefused) || errors.Is(err, ErrNoAnswer) {
		return river.JobCancel(err)
	}
	return err
}

// crediting is app.Crediting within tx: the orders, credited from suspense.
type crediting struct{ orders }

// Settle posts entries on the entity's business date, by actor through the API, with the order's credit as their
// reference, and stores o with its account and publishes it.
func (c crediting) Settle(ctx context.Context, o app.Order, actor string, entries []ledger.EntryInput) error {
	if err := c.postToday(ctx, o, postingrules.CodeSuspenseCredit, "credit", actor, channelAPI, entries); err != nil {
		return err
	}
	return c.Update(ctx, o)
}
