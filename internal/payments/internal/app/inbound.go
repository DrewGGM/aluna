package app

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/DrewGGM/aluna/internal/deposits"
	"github.com/DrewGGM/aluna/internal/ids"
	"github.com/DrewGGM/aluna/internal/ledger"
	"github.com/DrewGGM/aluna/internal/money"
	"github.com/DrewGGM/aluna/internal/payments/internal/domain"
	"github.com/DrewGGM/aluna/internal/payments/internal/domain/postingrules"
)

// Why a transfer received goes to suspense (the order's reason).
const (
	ReasonKeyNotResolved           = "KEY_NOT_RESOLVED" // the directory of Bre-B keys comes in wave 2
	ReasonUnknownAccount           = "UNKNOWN_ACCOUNT"
	ReasonAccountNotActive         = "ACCOUNT_NOT_ACTIVE"
	ReasonAccountInAnotherCurrency = "ACCOUNT_IN_ANOTHER_CURRENCY"
)

// Receiving is what receiving a transfer needs, within its transaction.
type Receiving interface {
	// LockActive locks and returns deposit account id of the entity if it is active: deposits.ErrAccountNotActive
	// if it is not, deposits.ErrAccountNotFound if the entity has no such account.
	LockActive(ctx context.Context, entity, id ids.ID) (deposits.Account, error)
	// RailAccounts are rail's clearing and suspense accounts in the entity, in currency c.
	RailAccounts(ctx context.Context, entity ids.ID, rail string, c money.Currency) (clearing, suspense ids.ID, err error)
	// Record stores o and reports whether it is new: false if the entity already received its transfer.
	Record(ctx context.Context, o Order) (bool, error)
	// Book posts entries, o's movement, as a transaction of code valued on valueDate, and publishes o.
	Book(ctx context.Context, o Order, code string, valueDate time.Time, entries []ledger.EntryInput) error
}

// Receive takes a transfer received through rail (FR-32). One for an active account of the entity, by its
// number, in its currency, is credited to it from the rail's clearing account and the order settles; any other
// goes to the rail's suspense account and the order is suspended, saying why: that order is the exception to
// resolve. The same transfer again (the same bank's end-to-end id) changes nothing. One whose identifiers,
// debtor agent, amount, settlement date, reference or creditor (one account or one key, of text) are invalid
// is ErrInvalidNotification.
func Receive(ctx context.Context, p Receiving, rail string, in InboundTransfer) error {
	o, err := newIncoming(rail, in)
	if err != nil {
		return err
	}
	clearing, suspense, err := p.RailAccounts(ctx, in.LegalEntityID, rail, in.Amount.Currency())
	if err != nil {
		return err
	}
	c, err := creditable(ctx, p, in)
	if err != nil {
		return err
	}
	code, entries, err := creditOrSuspend(&o, c, clearing, suspense)
	if err != nil {
		return err
	}
	fresh, err := p.Record(ctx, o)
	if err != nil || !fresh {
		return err
	}
	return p.Book(ctx, o, code, in.SettlementDate, entries)
}

// newIncoming is in's order, received through rail, referenced by its debtor agent and its end-to-end id, which
// the agent's letters and digits keep apart.
func newIncoming(rail string, in InboundTransfer) (Order, error) {
	o, err := domain.NewIncoming(domain.IncomingInput{
		ID: ids.New(), LegalEntityID: in.LegalEntityID, Amount: in.Amount, Rail: rail, EndToEndID: in.EndToEndID, InstrID: in.InstrID,
		TxID: in.TxID, DebtorAgent: in.DebtorAgent,
	})
	if err != nil {
		return Order{}, fmt.Errorf("%w: %v", ErrInvalidNotification, err) //nolint:errorlint // a transfer's data is the rail's, not an order the caller made
	}
	if err := checkCreditor(in.Creditor); err != nil {
		return Order{}, fmt.Errorf("%w: %v", ErrInvalidNotification, err) //nolint:errorlint // as above
	}
	switch {
	case in.SettlementDate.IsZero():
		return Order{}, fmt.Errorf("%w: missing the settlement date", ErrInvalidNotification)
	case !validText(in.RailRef, maxRailRef):
		return Order{}, fmt.Errorf("%w: the rail's reference is text of up to %d characters", ErrInvalidNotification, maxRailRef)
	}
	zero := money.New(0, in.Amount.Currency())
	return Order{
		Order: o, Creditor: in.Creditor, ExternalRef: in.DebtorAgent + "/" + in.EndToEndID, Held: zero, GMF: zero, RailRef: in.RailRef,
	}, nil
}

// creditor is what a transfer credits: the ledger account of deposit account, or, if there is a reason not to,
// the rail's suspense account, with the deposit account if it exists.
type creditor struct {
	account, ledger ids.ID
	reason          string
}

// creditable is the transfer's creditor: an active deposit account of the entity in the transfer's currency, or
// why there is none: a key (there is no directory yet), a number that is no account of the entity, an account
// that is not active or one in another currency.
func creditable(ctx context.Context, p Receiving, in InboundTransfer) (creditor, error) {
	if in.Creditor.Key != "" {
		return creditor{reason: ReasonKeyNotResolved}, nil
	}
	id, err := ids.Parse(in.Creditor.Number)
	if err != nil {
		return creditor{reason: ReasonUnknownAccount}, nil //nolint:nilerr // a number that is no id is no account: suspense, not an error
	}
	a, err := p.LockActive(ctx, in.LegalEntityID, id)
	switch {
	case errors.Is(err, deposits.ErrAccountNotFound):
		return creditor{reason: ReasonUnknownAccount}, nil
	case errors.Is(err, deposits.ErrAccountNotActive):
		return creditor{account: id, reason: ReasonAccountNotActive}, nil
	case err != nil:
		return creditor{}, err
	case a.Currency != in.Amount.Currency():
		return creditor{account: id, reason: ReasonAccountInAnotherCurrency}, nil
	}
	return creditor{account: id, ledger: a.LedgerAccountID}, nil
}

// creditOrSuspend moves o to settled, crediting c, if there is no reason not to, and to suspended for c's reason
// otherwise; o keeps c's deposit account if it exists. It returns the transaction code and the entries of
// either.
func creditOrSuspend(o *Order, c creditor, clearing, suspense ids.ID) (string, []ledger.EntryInput, error) {
	o.AccountID = c.account
	if c.reason != "" {
		if err := o.Suspend(c.reason); err != nil {
			return "", nil, err
		}
		entries, err := postingrules.InboundSuspense(o.Amount, clearing, suspense)
		return postingrules.CodeInboundSuspense, entries, err
	}
	if err := o.Credit(); err != nil {
		return "", nil, err
	}
	entries, err := postingrules.InboundCredit(o.Amount, clearing, c.ledger)
	return postingrules.CodeInboundCredit, entries, err
}
