// Package reconciliation is the facade of the continuous controls (FR-33): it checks the ledger against itself,
// keeps every result and opens a break for each difference.
package reconciliation

import (
	"context"
	"errors"
	"fmt"

	"github.com/DrewGGM/aluna/internal/ids"
	"github.com/DrewGGM/aluna/internal/outbox"
	"github.com/DrewGGM/aluna/internal/postgres"
	pg "github.com/DrewGGM/aluna/internal/reconciliation/internal/adapters/postgres"
	"github.com/DrewGGM/aluna/internal/reconciliation/internal/app"
)

const typeBreakOpened = "aluna.reconciliation.break.opened.v1"

// Events publishes reconciliation events; outbox.Outbox implements it.
type Events interface {
	Emit(ctx context.Context, tx postgres.Tx, e outbox.Event) error
}

// Service runs the controls.
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
	return uow.Do(ctx, func(tx postgres.Tx) error {
		return app.Record(ctx, recorder{Store: pg.NewStore(tx), events: s.events, tx: tx}, c, entities, found)
	})
}

// recorder is app.Recorder within tx.
type recorder struct {
	pg.Store
	events Events
	tx     postgres.Tx
}

// Publish emits that b opened.
func (r recorder) Publish(ctx context.Context, b app.Break) error {
	err := r.events.Emit(ctx, r.tx, outbox.Event{
		Type: typeBreakOpened, Subject: "reconciliation-breaks/" + b.ID.String(), LegalEntityID: b.LegalEntityID, SchemaVersion: 1,
		Data: openedEvent{BreakID: b.ID, Control: b.Control, Subject: b.Subject, Difference: amount{b.Difference.Units(), b.Difference.Currency().Code()}},
	})
	if err != nil {
		return fmt.Errorf("publish the break of %s: %w", b.Control, err)
	}
	return nil
}

// amount is a Money in an event: minor units and a currency, like the API's.
type amount struct {
	Amount   int64  `json:"amount"`
	Currency string `json:"currency"`
}

// openedEvent is the data of aluna.reconciliation.break.opened.v1: what does not square and by how much.
type openedEvent struct {
	BreakID    ids.ID      `json:"break_id"`
	Control    app.Control `json:"control"`
	Subject    string      `json:"subject"`
	Difference amount      `json:"difference"`
}
