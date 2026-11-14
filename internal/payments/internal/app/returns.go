package app

import (
	"context"
	"errors"
	"fmt"

	"github.com/DrewGGM/aluna/internal/deposits"
	"github.com/DrewGGM/aluna/internal/ledger"
	"github.com/DrewGGM/aluna/internal/payments/internal/domain"
	"github.com/DrewGGM/aluna/internal/payments/internal/domain/postingrules"
)

// Returning is what taking a payment's return needs, within its transaction.
type Returning interface {
	// LockByEndToEndID locks and returns the order with end-to-end id e2e, or ErrOrderNotFound.
	LockByEndToEndID(ctx context.Context, e2e string) (Order, error)
	railCrediting
	// Refund posts entries, o's return, as a transaction of code on the entity's business date.
	Refund(ctx context.Context, o Order, code string, entries []ledger.EntryInput) error
	// Update stores where o stands and publishes it.
	Update(ctx context.Context, o Order) error
}

// TakeReturn takes the rail's word that the beneficiary's bank returned a settled payment (FR-31): the order is
// returned for the rail's reason and its amount comes back from the rail's clearing account to its account, or,
// if the account is no longer active, to the rail's suspense account, an exception to resolve; its charges are
// not returned. The same return again changes nothing; the return of an order that is not settled is
// ErrOutcomeConflict, and an outcome that is not a return, or whose reference or reason is not text of up to 255
// characters, ErrInvalidNotification.
func TakeReturn(ctx context.Context, p Returning, out RailOutcome) error {
	if err := checkOutcome(out); err != nil {
		return err
	}
	if out.Status != RailReturned {
		return fmt.Errorf("%w: %s is not a return", ErrInvalidNotification, out.Status)
	}
	o, err := p.LockByEndToEndID(ctx, out.EndToEndID)
	if err != nil {
		return err
	}
	if o.Status == domain.StatusReturned {
		return nil
	}
	if o.Status != domain.StatusSettled {
		return fmt.Errorf("%w: order %s is %s and the rail returns it", ErrOutcomeConflict, o.ID, o.Status)
	}
	if err := o.Return(out.Reason); err != nil {
		return err
	}
	code, entries, err := returnEntries(ctx, p, o)
	if err != nil {
		return err
	}
	if err := p.Refund(ctx, o, code, entries); err != nil {
		return err
	}
	return p.Update(ctx, o)
}

// returnEntries is the transaction code and the entries of o's return: to its account if it is still active, to
// the rail's suspense account otherwise.
func returnEntries(ctx context.Context, p Returning, o Order) (string, []ledger.EntryInput, error) {
	clearing, suspense, err := p.RailAccounts(ctx, o.LegalEntityID, o.Rail, o.Amount.Currency())
	if err != nil {
		return "", nil, err
	}
	a, err := p.LockActive(ctx, o.LegalEntityID, o.AccountID)
	switch {
	case errors.Is(err, deposits.ErrAccountNotActive):
		entries, err := postingrules.ReturnSuspense(o.Amount, clearing, suspense)
		return postingrules.CodeReturnSuspense, entries, err
	case err != nil:
		return "", nil, err
	}
	entries, err := postingrules.ReturnCredit(o.Amount, clearing, a.LedgerAccountID)
	return postingrules.CodeReturnCredit, entries, err
}
