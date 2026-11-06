package reconciliation

import (
	"context"
	"fmt"

	"github.com/DrewGGM/aluna/internal/ids"
	"github.com/DrewGGM/aluna/internal/outbox"
	"github.com/DrewGGM/aluna/internal/postgres"
	pg "github.com/DrewGGM/aluna/internal/reconciliation/internal/adapters/postgres"
	"github.com/DrewGGM/aluna/internal/reconciliation/internal/app"
)

const (
	typeBreakOpened   = "aluna.reconciliation.break.opened.v1"
	typeBreakResolved = "aluna.reconciliation.break.resolved.v1"
)

// Types of a break.
type (
	// Break is a difference open until it is resolved.
	Break = app.Break
	// BreakStatus is where a break stands.
	BreakStatus = app.BreakStatus
	// BreakPage is a page of breaks.
	BreakPage = app.BreakPage
)

// Statuses of a break.
const (
	BreakOpen     = app.BreakOpen
	BreakResolved = app.BreakResolved
)

// Errors of the breaks.
var (
	ErrBreakNotFound     = app.ErrBreakNotFound
	ErrBreakResolved     = app.ErrBreakResolved
	ErrInvalidResolution = app.ErrInvalidResolution
	ErrInvalidPage       = app.ErrInvalidPage
)

// Breaks are the entity's breaks, newest first, those in status if it has one, from cursor, up to limit. q can be
// the pool or a Tx.
func (s *Service) Breaks(ctx context.Context, q postgres.Querier, entity ids.ID, status BreakStatus, cursor string, limit int) (BreakPage, error) {
	return app.Breaks(ctx, pg.NewStore(q), entity, status, cursor, limit)
}

// ResolveBreak resolves open break id of the entity within tx, with its cause, by who, and emits it.
func (s *Service) ResolveBreak(ctx context.Context, tx postgres.Tx, entity, id ids.ID, cause, by string) (Break, error) {
	return app.Resolve(ctx, s.within(tx), entity, id, cause, by)
}

// ports are the ports of the use cases within tx: the store and the events.
type ports struct {
	pg.Store
	events Events
	tx     postgres.Tx
}

func (s *Service) within(tx postgres.Tx) ports {
	return ports{Store: pg.NewStore(tx), events: s.events, tx: tx}
}

// PublishOpened emits that b opened.
func (p ports) PublishOpened(ctx context.Context, b app.Break) error {
	err := p.events.Emit(ctx, p.tx, outbox.Event{
		Type: typeBreakOpened, Subject: "reconciliation-breaks/" + b.ID.String(), LegalEntityID: b.LegalEntityID, SchemaVersion: 1,
		Data: openedEvent{
			BreakID: b.ID, Control: b.Control, Subject: b.Subject, Reason: b.Reason,
			Difference: amount{b.Difference.Units(), b.Difference.Currency().Code()},
		},
	})
	if err != nil {
		return fmt.Errorf("publish the break of %s: %w", b.Control, err)
	}
	return nil
}

// PublishResolved emits that b was resolved.
func (p ports) PublishResolved(ctx context.Context, b app.Break) error {
	err := p.events.Emit(ctx, p.tx, outbox.Event{
		Type: typeBreakResolved, Subject: "reconciliation-breaks/" + b.ID.String(), LegalEntityID: b.LegalEntityID, SchemaVersion: 1,
		Data: resolvedEvent{BreakID: b.ID, Control: b.Control, Subject: b.Subject},
	})
	if err != nil {
		return fmt.Errorf("publish the resolution of break %s: %w", b.ID, err)
	}
	return nil
}

// amount is a Money in an event: minor units and a currency, like the API's.
type amount struct {
	Amount   int64  `json:"amount"`
	Currency string `json:"currency"`
}

// openedEvent is the data of aluna.reconciliation.break.opened.v1: what does not square, by how much and, for a
// statement, why.
type openedEvent struct {
	BreakID    ids.ID      `json:"break_id"`
	Control    app.Control `json:"control"`
	Subject    string      `json:"subject"`
	Reason     app.Reason  `json:"reason,omitempty"`
	Difference amount      `json:"difference"`
}

// resolvedEvent is the data of aluna.reconciliation.break.resolved.v1. The cause is free text, so it does not go
// out: it could carry personal data.
type resolvedEvent struct {
	BreakID ids.ID      `json:"break_id"`
	Control app.Control `json:"control"`
	Subject string      `json:"subject"`
}
