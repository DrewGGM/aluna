package app

import (
	"context"
	"errors"
	"fmt"

	"github.com/DrewGGM/aluna/internal/ids"
	"github.com/DrewGGM/aluna/internal/ledger/internal/domain"
	"github.com/DrewGGM/aluna/internal/money"
)

// ErrInvalidAccount indica datos de cuenta fuera de los valores permitidos o un código repetido.
var ErrInvalidAccount = errors.New("cuenta inválida")

// CreateAccountInput son los datos de una cuenta nueva.
type CreateAccountInput struct {
	LegalEntityID ids.ID
	Code          string
	Kind          domain.Kind
	Currency      money.Currency
	Mode          domain.BalanceMode
	LowerLimit    int64
}

// AccountWriter es la persistencia de CreateAccount.
type AccountWriter interface {
	InsertAccount(ctx context.Context, a domain.Account) error
}

// Balance es el saldo materializado de una cuenta, con signo (débito +, crédito −). En una cuenta async
// lo mantiene el agregador (T-023).
type Balance struct {
	Posted, PendingDebits, PendingCredits, Version int64
}

// CreateAccount registra la cuenta con su saldo en cero.
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
		return fmt.Errorf("%w: falta el código", ErrInvalidAccount)
	case in.Currency.Code() == "":
		return fmt.Errorf("%w: falta la moneda", ErrInvalidAccount)
	}
	switch in.Kind {
	case domain.KindAsset, domain.KindLiability, domain.KindEquity, domain.KindIncome, domain.KindExpense:
	default:
		return fmt.Errorf("%w: clase %q", ErrInvalidAccount, in.Kind)
	}
	if in.Mode != domain.BalanceModeSync && in.Mode != domain.BalanceModeAsync {
		return fmt.Errorf("%w: modo %q", ErrInvalidAccount, in.Mode)
	}
	return nil
}
