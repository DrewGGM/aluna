package app

import (
	"context"
	"errors"
	"fmt"

	"github.com/DrewGGM/aluna/internal/deposits"
	"github.com/DrewGGM/aluna/internal/ids"
	"github.com/DrewGGM/aluna/internal/ledger"
	"github.com/DrewGGM/aluna/internal/money"
	"github.com/DrewGGM/aluna/internal/payments/internal/domain"
	"github.com/DrewGGM/aluna/internal/payments/internal/domain/postingrules"
	"github.com/DrewGGM/aluna/internal/pricing"
)

// Returning is what taking a payment's return needs, within its transaction.
type Returning interface {
	// LockByEndToEndID locks and returns the order with end-to-end id e2e, or ErrOrderNotFound.
	LockByEndToEndID(ctx context.Context, e2e string) (Order, error)
	railCrediting
	// Accounts are o's charge accounts; one that o does not charge is the zero ID and is not looked up.
	Accounts(ctx context.Context, o Order) (ChargeAccounts, error)
	// Book posts entries, o's return, as a transaction of code on the entity's business date, restores restore of
	// the GMF exemption o used, and stores and publishes o.
	Book(ctx context.Context, o Order, code string, entries []ledger.EntryInput, restore pricing.ExemptionUse) error
}

// TakeReturn takes the rail's word that the beneficiary's bank returned a settled payment (FR-31): the order is
// returned for the rail's reason and its amount comes back from the rail's clearing account to its account, or,
// if the account is no longer active, to the rail's suspense account, an exception to resolve. With it come back,
// as policy says (ADR-0016), its fees, and its GMF and what it used of the GMF exemption if the reason annuls the
// payment. The same return again changes nothing; the return of an order that is not settled is
// ErrOutcomeConflict, and an outcome that is not a return, or whose reference or reason is not text of up to 255
// characters, ErrInvalidNotification.
func TakeReturn(ctx context.Context, p Returning, out RailOutcome, policy Policy) error {
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
	code, to, entries, err := returnEntries(ctx, p, o)
	if err != nil {
		return err
	}
	annulled := policy.annuls(out.Reason)
	fees, gmf := policy.refunded(o, annulled)
	refunds, err := chargeRefunds(ctx, p, o, to, fees, gmf)
	if err != nil {
		return err
	}
	return p.Book(ctx, o, code, append(entries, refunds...), policy.restored(o, !annulled))
}

// returnEntries is the transaction code, the ledger account the money goes back to and the entries of o's
// return: to its account if it is still active, to the rail's suspense account otherwise.
func returnEntries(ctx context.Context, p Returning, o Order) (string, ids.ID, []ledger.EntryInput, error) {
	clearing, suspense, err := p.RailAccounts(ctx, o.LegalEntityID, o.Rail, o.Amount.Currency())
	if err != nil {
		return "", ids.ID{}, nil, err
	}
	a, err := p.LockActive(ctx, o.LegalEntityID, o.AccountID)
	switch {
	case errors.Is(err, deposits.ErrAccountNotActive):
		entries, err := postingrules.ReturnSuspense(o.Amount, clearing, suspense)
		return postingrules.CodeReturnSuspense, suspense, entries, err
	case err != nil:
		return "", ids.ID{}, nil, err
	}
	entries, err := postingrules.ReturnCredit(o.Amount, clearing, a.LedgerAccountID)
	return postingrules.CodeReturnCredit, a.LedgerAccountID, entries, err
}

// chargeRefunds are the entries that give fees and gmf, o's charges a return refunds, back to ledger account to.
func chargeRefunds(ctx context.Context, p Returning, o Order, to ids.ID, fees []pricing.Fee, gmf money.Amount) ([]ledger.EntryInput, error) {
	if len(fees) == 0 && gmf.Sign() <= 0 {
		return nil, nil
	}
	accounts, err := p.Accounts(ctx, o)
	if err != nil {
		return nil, err
	}
	charges := make([]postingrules.Charge, 0, len(fees)+1)
	for _, f := range fees {
		charges = append(charges, postingrules.Charge{Amount: f.Amount, Counter: accounts.FeeIncome})
	}
	if gmf.Sign() > 0 {
		charges = append(charges, postingrules.Charge{Amount: gmf, Counter: accounts.GMFPayable})
	}
	return postingrules.Refunds(to, charges)
}
