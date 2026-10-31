package app

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/DrewGGM/aluna/internal/calendar"
	"github.com/DrewGGM/aluna/internal/deposits/internal/domain"
	"github.com/DrewGGM/aluna/internal/deposits/internal/domain/postingrules"
	"github.com/DrewGGM/aluna/internal/ids"
	"github.com/DrewGGM/aluna/internal/ledger"
	"github.com/DrewGGM/aluna/internal/money"
)

var (
	// ErrAccountNotActive means the account is not active: only an active account moves money (FR-22).
	ErrAccountNotActive = errors.New("the deposit account is not active")
	// ErrProductAccountMissing means the entity has no ledger account with the code the product names, in the
	// deposit account's currency: the product is misconfigured, not the request.
	ErrProductAccountMissing = errors.New("the product's ledger account does not exist")
)

// Source of the deposit movements in the ledger and their transaction codes.
const (
	movementSource = "deposits"
	codeDeposit    = "deposits.cash_deposit"
	codeWithdrawal = "deposits.withdrawal"
	codeTransfer   = "deposits.internal_transfer"
)

// CashInput is cash into or out of deposit account AccountID of the entity. ExternalRef identifies the
// movement among the entity's deposit movements: a retry with the same one is ledger.ErrDuplicateExternalRef
// and posts nothing. Actor (a subject) and Channel are recorded with the transaction.
type CashInput struct {
	LegalEntityID, AccountID ids.ID
	Amount                   money.Amount
	ExternalRef              string
	Actor, Channel           string
}

// TransferInput moves money from deposit account From of the entity to its account To; ExternalRef, Actor and
// Channel as in CashInput.
type TransferInput struct {
	LegalEntityID, From, To ids.ID
	Amount                  money.Amount
	ExternalRef             string
	Actor, Channel          string
}

// header is what a movement's transaction records besides its entries.
type header struct{ code, externalRef, actor, channel string }

// Movements is what moving an account's money needs, within the transaction it runs in.
type Movements interface {
	// LockAccount reads account id of the entity and keeps it locked (FOR UPDATE) until the transaction
	// ends.
	LockAccount(ctx context.Context, entity, id ids.ID) (domain.Account, error)
	// CashAccount is the ledger account of the cash the product of versionID names, in currency c, or
	// ErrProductAccountMissing.
	CashAccount(ctx context.Context, entity, versionID ids.ID, c money.Currency) (ids.ID, error)
	BusinessDate(ctx context.Context, entity ids.ID) (calendar.Date, error)
	Post(ctx context.Context, in ledger.PostInput) (ledger.Transaction, error)
	// RecordActivity sets the account's last customer activity to on.
	RecordActivity(ctx context.Context, id ids.ID, on calendar.Date) error
}

// Deposit posts cash into an active account from the product's cash account (FR-23).
func Deposit(ctx context.Context, s Movements, in CashInput) (ledger.Transaction, error) {
	return cashMovement(ctx, s, in, codeDeposit, postingrules.CashDeposit)
}

// Withdraw posts cash out of an active account to the product's cash account (FR-23); the ledger rejects
// it without funds (ledger.ErrInsufficientFunds).
func Withdraw(ctx context.Context, s Movements, in CashInput) (ledger.Transaction, error) {
	return cashMovement(ctx, s, in, codeWithdrawal, postingrules.Withdrawal)
}

func cashMovement(ctx context.Context, s Movements, in CashInput, code string,
	rule func(postingrules.Movement) ([]ledger.EntryInput, error),
) (ledger.Transaction, error) {
	a, err := lockActive(ctx, s, in.LegalEntityID, in.AccountID)
	if err != nil {
		return ledger.Transaction{}, err
	}
	vault, err := s.CashAccount(ctx, a.LegalEntityID, a.ProductVersionID, a.Currency)
	if err != nil {
		return ledger.Transaction{}, err
	}
	entries, err := rule(postingrules.Movement{Amount: in.Amount, Account: a.LedgerAccountID, Counter: vault})
	if err != nil {
		return ledger.Transaction{}, err
	}
	return post(ctx, s, a, header{code, in.ExternalRef, in.Actor, in.Channel}, entries)
}

// Transfer moves money between two active accounts of the entity (FR-23, US-5); the ledger rejects it
// without funds in From. Only From records customer activity: what To receives is not its customer's doing.
func Transfer(ctx context.Context, s Movements, in TransferInput) (ledger.Transaction, error) {
	from, to, err := lockPair(ctx, s, in.LegalEntityID, in.From, in.To)
	if err != nil {
		return ledger.Transaction{}, err
	}
	entries, err := postingrules.InternalTransfer(postingrules.Movement{Amount: in.Amount, Account: from.LedgerAccountID, Counter: to.LedgerAccountID})
	if err != nil {
		return ledger.Transaction{}, err
	}
	return post(ctx, s, from, header{codeTransfer, in.ExternalRef, in.Actor, in.Channel}, entries)
}

// lockPair locks accounts from and to of the entity, both active, in id order: two opposite transfers do not
// deadlock. The same account twice is locked twice, which PostgreSQL allows.
func lockPair(ctx context.Context, s Movements, entity, from, to ids.ID) (domain.Account, domain.Account, error) {
	swapped := bytes.Compare(from[:], to[:]) > 0
	if swapped {
		from, to = to, from
	}
	first, err := lockActive(ctx, s, entity, from)
	if err != nil {
		return domain.Account{}, domain.Account{}, err
	}
	second, err := lockActive(ctx, s, entity, to)
	if err != nil {
		return domain.Account{}, domain.Account{}, err
	}
	if swapped {
		return second, first, nil
	}
	return first, second, nil
}

// lockActive locks account id of the entity and checks, under the lock, that it is active: the contract of
// ChangeStatus.
func lockActive(ctx context.Context, s Movements, entity, id ids.ID) (domain.Account, error) {
	a, err := s.LockAccount(ctx, entity, id)
	if err != nil {
		return domain.Account{}, err
	}
	if a.Status != domain.Active {
		return domain.Account{}, fmt.Errorf("%w: %s is %s", ErrAccountNotActive, a.ID, a.Status)
	}
	return a, nil
}

// post posts entries on the entity's business date, as value date too, and records it as the last activity
// of customer account a.
func post(ctx context.Context, s Movements, a domain.Account, h header, entries []ledger.EntryInput) (ledger.Transaction, error) {
	today, err := s.BusinessDate(ctx, a.LegalEntityID)
	if err != nil {
		return ledger.Transaction{}, err
	}
	day := time.Date(today.Year, today.Month, today.Day, 0, 0, 0, 0, time.UTC)
	posted, err := s.Post(ctx, ledger.PostInput{
		LegalEntityID: a.LegalEntityID, Code: h.code, ValueDate: day, BusinessDate: day,
		Source: movementSource, ExternalRef: h.externalRef, Actor: h.actor, Channel: h.channel,
		RuleVersion: postingrules.RuleVersion, Entries: entries,
	})
	if err != nil {
		return ledger.Transaction{}, err
	}
	if a.LastCustomerActivityOn != today {
		if err := s.RecordActivity(ctx, a.ID, today); err != nil {
			return ledger.Transaction{}, err
		}
	}
	return posted, nil
}
