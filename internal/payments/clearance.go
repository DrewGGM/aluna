package payments

import (
	"context"
	"fmt"

	"github.com/DrewGGM/aluna/internal/ids"
	pg "github.com/DrewGGM/aluna/internal/payments/internal/adapters/postgres"
	"github.com/DrewGGM/aluna/internal/payments/internal/app"
	"github.com/DrewGGM/aluna/internal/postgres"
)

// ResolveScreening applies, within tx, compliance's decision (FR-42) on order orderID of entity, which waited in
// pending_screening: it is the compliance.Resolved of payment cases, which cmd/aluna registers. A confirmed match
// rejects the order (RR04). A cleared one holds its amount and charges as initiating it would, priced as it is
// cleared (a quote it named was never used), and queues it to be sent; if they cannot be held, without funds
// (AM04) or with its account no longer active (AC06), the order is rejected instead and nothing is held.
func (s *Service) ResolveScreening(ctx context.Context, tx postgres.Tx, entity, orderID ids.ID, cleared bool) error {
	const savepoint = "payment_clearance"
	if _, err := tx.Exec(ctx, "SAVEPOINT "+savepoint); err != nil {
		return fmt.Errorf("payment savepoint: %w", err)
	}
	c := clearance{initiation{s: s, tx: tx}}
	err := app.ResolveScreening(ctx, c, entity, orderID, cleared)
	reason := app.HoldRefusal(err)
	if reason != "" {
		if err := rollbackTo(ctx, tx, savepoint); err != nil {
			return err
		}
		return app.RejectPending(ctx, c, entity, orderID, reason)
	}
	if err != nil {
		return undo(ctx, tx, savepoint, err)
	}
	if _, err := tx.Exec(ctx, "RELEASE SAVEPOINT "+savepoint); err != nil {
		return fmt.Errorf("release the payment savepoint: %w", err)
	}
	return nil
}

// clearance is app.Clearance within tx.
type clearance struct{ initiation }

func (c clearance) LockOrder(ctx context.Context, id ids.ID) (app.Order, error) {
	return pg.NewStore(c.tx).LockOrder(ctx, id)
}

// Store stores and publishes o; a held order is stored with its hold and queued to be sent.
func (c clearance) Store(ctx context.Context, o app.Order) error {
	if o.Status != StatusHeld {
		return orders{s: c.s, tx: c.tx}.Update(ctx, o)
	}
	if err := pg.NewStore(c.tx).HoldOrder(ctx, o); err != nil {
		return err
	}
	return c.s.queue(ctx, c.tx, o)
}
