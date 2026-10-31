// Package app holds the use cases of deposit accounts: opening one, moving its money and moving it through
// its life cycle (FR-17, FR-19, FR-22, FR-23).
package app

import (
	"context"
	"errors"
	"fmt"
	"slices"

	"github.com/DrewGGM/aluna/internal/calendar"
	"github.com/DrewGGM/aluna/internal/deposits/internal/domain"
	"github.com/DrewGGM/aluna/internal/ids"
	"github.com/DrewGGM/aluna/internal/ledger"
	"github.com/DrewGGM/aluna/internal/party"
	"github.com/DrewGGM/aluna/internal/products"
)

var (
	// ErrKYCNotSatisfied means the party's KYC status is not one the product allows (FR-17).
	ErrKYCNotSatisfied = errors.New("the party's KYC status does not satisfy the product")
	// ErrAccountNotFound means the deposit account does not exist.
	ErrAccountNotFound = errors.New("deposit account not found")
)

// OpenInput is a deposit account to open: for party PartyID of the entity, with the version in force of
// product ProductID.
type OpenInput struct {
	LegalEntityID, PartyID, ProductID ids.ID
}

// Opening is what opening an account needs, within the transaction it runs in.
type Opening interface {
	CurrentVersion(ctx context.Context, productID ids.ID) (products.Version, error)
	KYCStatus(ctx context.Context, entity, partyID ids.ID) (party.KYCStatus, error)
	BusinessDate(ctx context.Context, entity ids.ID) (calendar.Date, error)
	CreateLedgerAccount(ctx context.Context, in ledger.CreateAccountInput) (ledger.Account, error)
	InsertAccount(ctx context.Context, a domain.Account) error
}

// Open opens a deposit account (FR-19, FR-22): anchored to the product's version in force, which must be
// the entity's, if the party's KYC is one the product allows (else ErrKYCNotSatisfied), active if the KYC
// is approved and pending otherwise, held in a new liability ledger account of the product's currency
// that cannot go below 0, opened on the entity's business date.
func Open(ctx context.Context, s Opening, in OpenInput) (domain.Account, error) {
	version, err := s.CurrentVersion(ctx, in.ProductID)
	if err != nil {
		return domain.Account{}, err
	}
	if version.LegalEntityID != in.LegalEntityID { // the version is read by id, of any entity
		return domain.Account{}, fmt.Errorf("%w: %s", products.ErrProductNotFound, in.ProductID)
	}
	kyc, err := s.KYCStatus(ctx, in.LegalEntityID, in.PartyID)
	if err != nil {
		return domain.Account{}, err
	}
	// one read decides both, so they cannot see two different statuses
	if !slices.Contains(version.Definition.AllowedKYC, string(kyc)) {
		return domain.Account{}, fmt.Errorf("%w: party %s is %s", ErrKYCNotSatisfied, in.PartyID, kyc)
	}
	openedOn, err := s.BusinessDate(ctx, in.LegalEntityID)
	if err != nil {
		return domain.Account{}, err
	}
	a := domain.Account{
		ID: ids.New(), LegalEntityID: in.LegalEntityID, PartyID: in.PartyID, ProductVersionID: version.ID,
		Currency: version.Definition.Currency, Status: domain.OpeningStatus(kyc == party.KYCApproved), OpenedOn: openedOn, LastCustomerActivityOn: openedOn,
	}
	held, err := s.CreateLedgerAccount(ctx, ledger.CreateAccountInput{
		LegalEntityID: in.LegalEntityID, Code: "deposits/" + a.ID.String(), Kind: ledger.KindLiability,
		Currency: a.Currency, Mode: ledger.BalanceModeSync, LowerLimit: 0,
	})
	if err != nil {
		return domain.Account{}, err
	}
	a.LedgerAccountID = held.ID
	if err := s.InsertAccount(ctx, a); err != nil {
		return domain.Account{}, err
	}
	return a, nil
}

// StatusChanges is what changing an account's status needs, within the transaction it runs in.
type StatusChanges interface {
	// LockAccount reads account id of the entity and keeps it locked (FOR UPDATE) until the transaction
	// ends.
	LockAccount(ctx context.Context, entity, id ids.ID) (domain.Account, error)
	Balance(ctx context.Context, ledgerAccountID ids.ID) (ledger.Balance, error)
	KYCStatus(ctx context.Context, entity, partyID ids.ID) (party.KYCStatus, error)
	BusinessDate(ctx context.Context, entity ids.ID) (calendar.Date, error)
	// SetStatus moves account a to status to, with a's last customer activity, and records the change with
	// its actor.
	SetStatus(ctx context.Context, a domain.Account, to domain.Status, actor string) error
}

// ChangeStatus moves account id of the entity to status to, recording actor as who did it, if the diagram
// allows it (else domain.ErrInvalidTransition). Activating a pending account needs its party's KYC
// approved (else ErrKYCNotSatisfied); closing it needs no pending holds and a posted balance of 0. Moving it
// to active records the business date as its last customer activity.
//
// The close reads the ledger balance under the account's lock, not the balance's: it holds only if every
// movement of the account's money (postings, holds, captures) first locks the account the same way, checks
// its status under that lock and only then posts. Movements lock the accounts before the ledger balances,
// and several accounts in id order.
func ChangeStatus(ctx context.Context, s StatusChanges, entity, id ids.ID, to domain.Status, actor string) error {
	a, err := s.LockAccount(ctx, entity, id)
	if err != nil {
		return err
	}
	if err := checkChange(ctx, s, a, to); err != nil {
		return err
	}
	if to == domain.Active {
		// the inactivity restarts: otherwise the dormancy job would take an account just reactivated, or
		// unfrozen after a long freeze, back to dormant before its customer could move it (FR-27)
		if a.LastCustomerActivityOn, err = s.BusinessDate(ctx, a.LegalEntityID); err != nil {
			return err
		}
	}
	return s.SetStatus(ctx, a, to, actor)
}

// checkChange reports whether account a can go to status to: the diagram, an approved KYC to activate a
// pending account, and no balance or pending holds to close.
func checkChange(ctx context.Context, s StatusChanges, a domain.Account, to domain.Status) error {
	if err := domain.CheckTransition(a.Status, to); err != nil {
		return err
	}
	switch {
	case a.Status == domain.Pending && to == domain.Active:
		kyc, err := s.KYCStatus(ctx, a.LegalEntityID, a.PartyID)
		if err != nil {
			return err
		}
		if kyc != party.KYCApproved {
			return fmt.Errorf("%w: party %s is %s, not approved", ErrKYCNotSatisfied, a.PartyID, kyc)
		}
	case to == domain.Closed:
		b, err := s.Balance(ctx, a.LedgerAccountID)
		if err != nil {
			return err
		}
		return domain.CheckClosable(b)
	}
	return nil
}
