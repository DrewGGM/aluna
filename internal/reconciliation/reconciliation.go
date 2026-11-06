// Package reconciliation is the facade of the continuous controls (FR-33) and of the rail's statements (FR-34): it
// checks the ledger against itself and the payment orders against what the rail reports, keeps every result and
// opens a break for each difference, which stays until someone resolves it with a cause.
package reconciliation

import (
	"context"
	"errors"

	"github.com/DrewGGM/aluna/internal/ids"
	"github.com/DrewGGM/aluna/internal/outbox"
	"github.com/DrewGGM/aluna/internal/postgres"
	pg "github.com/DrewGGM/aluna/internal/reconciliation/internal/adapters/postgres"
	"github.com/DrewGGM/aluna/internal/reconciliation/internal/app"
)

// Events publishes reconciliation events; outbox.Outbox implements it.
type Events interface {
	Emit(ctx context.Context, tx postgres.Tx, e outbox.Event) error
}

// Service runs the controls, imports statements and keeps the breaks.
type Service struct{ events Events }

// New creates the service; it publishes its events to events.
func New(events Events) *Service { return &Service{events: events} }

// RunControls runs every control. Each one reads in its own transaction, which writes nothing, and records in
// another: a control can take long, and a transaction that has written holds back the aggregation of the async
// balances for as long as it lasts. A control that fails stops neither the others nor what they recorded.
func (s *Service) RunControls(ctx context.Context, uow *postgres.UnitOfWork) error {
	var entities []ids.ID
	err := uow.Do(ctx, func(tx postgres.Tx) error {
		var err error
		entities, err = pg.NewStore(tx).Entities(ctx)
		return err
	})
	if err != nil {
		return err
	}
	errs := make([]error, 0, len(app.Controls))
	for _, c := range app.Controls {
		errs = append(errs, s.run(ctx, uow, c, entities))
	}
	return errors.Join(errs...)
}

// run runs control c over entities and records what it finds, all or nothing.
func (s *Service) run(ctx context.Context, uow *postgres.UnitOfWork, c app.Control, entities []ids.ID) error {
	var found []app.Finding
	err := uow.Do(ctx, func(tx postgres.Tx) error {
		var err error
		found, err = pg.NewStore(tx).Findings(ctx, c)
		return err
	})
	if err != nil {
		return err
	}
	return uow.Do(ctx, func(tx postgres.Tx) error { return app.Record(ctx, s.within(tx), c, entities, found) })
}
