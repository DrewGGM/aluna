package payments

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/DrewGGM/aluna/internal/ledger"
	pg "github.com/DrewGGM/aluna/internal/payments/internal/adapters/postgres"
	"github.com/DrewGGM/aluna/internal/payments/internal/app"
	"github.com/DrewGGM/aluna/internal/payments/internal/domain/postingrules"
	"github.com/DrewGGM/aluna/internal/postgres"
)

// InboundTransfer is a credit transfer another bank sent through the rail to a customer of the entity.
type InboundTransfer = app.InboundTransfer

// Why a transfer received is suspended (the order's reason).
const (
	ReasonKeyNotResolved           = app.ReasonKeyNotResolved
	ReasonUnknownAccount           = app.ReasonUnknownAccount
	ReasonAccountNotActive         = app.ReasonAccountNotActive
	ReasonAccountInAnotherCurrency = app.ReasonAccountInAnotherCurrency
)

// ReadTransfer is the transfer received that the rail's notification n carries (TH-7), read without touching the
// database: one the rail did not sign within 5 minutes is ErrUnverifiedNotification, and one the core cannot
// read ErrInvalidNotification.
func (s *Service) ReadTransfer(n Notification) (InboundTransfer, error) { return s.rail.Transfer(n) }

// ReceiveTransfer takes, within tx, a transfer received through the rail (FR-32): one for an active account of
// its entity, by its number (the account's id) and in its currency, is credited to it from the rail's clearing
// account, and its order settles; any other is credited to the rail's suspense account and its order is
// suspended, saying why, an exception to resolve. Both are valued on the transfer's settlement date. The same
// bank's end-to-end id again changes nothing. A transfer with invalid data is ErrInvalidNotification; an entity
// without the rail's clearing or suspense account, ErrRailNotConfigured.
func (s *Service) ReceiveTransfer(ctx context.Context, tx postgres.Tx, in InboundTransfer) error {
	return app.Receive(ctx, receiving{orders{s: s, tx: tx}}, s.rail.Name(), in)
}

// receiving is app.Receiving within tx: the orders, recorded as transfers received.
type receiving struct{ orders }

func (r receiving) Record(ctx context.Context, o app.Order) (bool, error) {
	err := pg.NewStore(r.tx).InsertOrder(ctx, o)
	if errors.Is(err, app.ErrDuplicatePayment) {
		return false, nil
	}
	return err == nil, err
}

// Book posts entries with the payments rules, the order as their reference, and publishes o as
// aluna.payments.order.<status>.v1.
func (r receiving) Book(ctx context.Context, o app.Order, code string, valueDate time.Time, entries []ledger.EntryInput) error {
	if _, err := r.s.ledger.Post(ctx, r.tx, ledger.PostInput{
		LegalEntityID: o.LegalEntityID, Code: code, ValueDate: valueDate, Source: sourcePayments, ExternalRef: o.ID.String(),
		Actor: o.Rail, Channel: channelRail, RuleVersion: postingrules.RuleVersion, Entries: entries,
	}); err != nil {
		return fmt.Errorf("book transfer %s: %w", o.ExternalRef, err)
	}
	return r.s.emit(ctx, r.tx, eventType(o.Status), o)
}
