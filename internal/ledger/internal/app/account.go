package app

import (
	"context"
	"errors"
	"fmt"

	"github.com/DrewGGM/aluna/internal/ids"
	"github.com/DrewGGM/aluna/internal/ledger/internal/domain"
	"github.com/DrewGGM/aluna/internal/money"
)

// ErrInvalidAccount means account data outside the allowed values or a repeated code.
var ErrInvalidAccount = errors.New("invalid account")

// CreateAccountInput is the data for a new account.
type CreateAccountInput struct {
	LegalEntityID ids.ID
	Code          string
	Kind          domain.Kind
	Currency      money.Currency
	Mode          domain.BalanceMode
	LowerLimit    int64
}

// AccountWriter is the persistence of CreateAccount.
type AccountWriter interface {
	InsertAccount(ctx context.Context, a domain.Account) error
}

// Balance is an account's materialized balance, signed (debit +, credit −). On an async account the
// aggregator maintains it (T-023).
type Balance struct {
	Posted, PendingDebits, PendingCredits, Version int64
}

// CreateAccount registers the account with a zero balance.
func CreateAccount(ctx context.Context, store AccountWriter, in CreateAccountInput) (domain.Account, error) {
	if err := validAccount(in); err != nil {
		return domain.Account{}, err
	}
	a := domain.Account{
		ID: ids.New(), LegalEntityID: in.LegalEntityID, Code: in.Code, Kind: in.Kind,
		Currency: in.Currency, Mode: in.Mode, LowerLimit: in.LowerLimit,
	}
	if err := store.InsertAccount(ctx, a); err != nil {
		return domain.Account{}, err
	}
	return a, nil
}

func validAccount(in CreateAccountInput) error {
	switch {
	case in.Code == "":
		return fmt.Errorf("%w: missing code", ErrInvalidAccount)
	case in.Currency.Code() == "":
		return fmt.Errorf("%w: missing currency", ErrInvalidAccount)
	}
	switch in.Kind {
	case domain.KindAsset, domain.KindLiability, domain.KindEquity, domain.KindIncome, domain.KindExpense:
	default:
		return fmt.Errorf("%w: kind %q", ErrInvalidAccount, in.Kind)
	}
	if in.Mode != domain.BalanceModeSync && in.Mode != domain.BalanceModeAsync {
		return fmt.Errorf("%w: mode %q", ErrInvalidAccount, in.Mode)
	}
	return nil
}
