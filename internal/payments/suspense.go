package payments

import (
	"context"

	"github.com/DrewGGM/aluna/internal/ids"
	"github.com/DrewGGM/aluna/internal/ledger"
	"github.com/DrewGGM/aluna/internal/payments/internal/app"
	"github.com/DrewGGM/aluna/internal/payments/internal/domain/postingrules"
	"github.com/DrewGGM/aluna/internal/postgres"
)

// ErrNotSuspended means an order that is not a transfer held in suspense: there is nothing to credit.
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
