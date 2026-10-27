// Package domain son las reglas puras del ledger (ADR-0003): la transacción balanceada y el saldo
// disponible de una cuenta. No conoce la base de datos ni la hora.
package domain

import (
	"errors"
	"fmt"

	"github.com/DrewGGM/aluna/internal/ids"
	"github.com/DrewGGM/aluna/internal/money"
)

var (
	// ErrInsufficientFunds indica que un movimiento deja el saldo disponible de una cuenta bajo su límite.
	ErrInsufficientFunds = errors.New("fondos insuficientes")
	// ErrUnknownKind indica una cuenta con una clase contable fuera de las cinco definidas.
	ErrUnknownKind = errors.New("clase contable desconocida")
)

// Kind es la clase contable de una cuenta; decide su naturaleza (débito o crédito).
type Kind string

// Clases contables.
const (
	KindAsset     Kind = "asset"
	KindLiability Kind = "liability"
	KindEquity    Kind = "equity"
	KindIncome    Kind = "income"
	KindExpense   Kind = "expense"
)

// BalanceMode dice cómo se mantiene el saldo materializado de una cuenta (ADR-0004).
type BalanceMode string

// Modos de saldo: sync se actualiza en la misma transacción del posting; async lo agrega un proceso
// aparte (cuentas internas muy concurridas).
const (
	BalanceModeSync  BalanceMode = "sync"
	BalanceModeAsync BalanceMode = "async"
)

// Account es una cuenta del ledger. LowerLimit está en unidades menores y en términos naturales: 0 no
// permite saldo negativo; -5_000_000 en COP permite un sobregiro de 50.000 pesos.
type Account struct {
	ID            ids.ID
	LegalEntityID ids.ID
	Code          string
	Kind          Kind
	Currency      money.Currency
	Mode          BalanceMode
	LowerLimit    int64
}

// NaturalBalance convierte el saldo con signo (débito +, crédito −) al saldo en la naturaleza de la
// cuenta: activos y gastos son de naturaleza débito; pasivos, patrimonio e ingresos, de crédito. Un
// depósito de 100.000 pesos deja posted = -10_000_000 en la cuenta del cliente y su saldo natural es
// 10_000_000. En una cuenta de crédito, posted = MinInt64 no tiene opuesto en int64.
func NaturalBalance(k Kind, posted int64) int64 {
	if debit, _ := debitNormal(k); debit {
		return posted
	}
	return -posted
}

// debitNormal dice si la clase es de naturaleza débito; ok es falso si la clase no existe.
func debitNormal(k Kind) (debit, ok bool) {
	switch k {
	case KindAsset, KindExpense:
		return true, true
	case KindLiability, KindEquity, KindIncome:
		return false, true
	}
	return false, false
}

// CheckAvailable decide si la cuenta admite delta, el movimiento neto (con signo) de una transacción
// sobre ella, dado su saldo posted y sus holds pendientes (importes no negativos). El disponible es el
// saldo natural menos los holds salientes (los débitos pendientes en una cuenta de crédito, los
// créditos pendientes en una de débito); los entrantes no suman. Solo se rechaza un movimiento que
// reduce el saldo natural: un ingreso a una cuenta que ya está bajo su límite la mejora. Todo se
// calcula con money: un overflow es un error y el movimiento no pasa.
func (a Account) CheckAvailable(posted, pendingDebits, pendingCredits, delta int64) error {
	debit, ok := debitNormal(a.Kind)
	if !ok { // sin naturaleza no se sabe hacia dónde sale el dinero: no se mueve
		return fmt.Errorf("%w: cuenta %s, clase %q", ErrUnknownKind, a.Code, a.Kind)
	}
	if reduces := (delta < 0) == debit && delta != 0; !reduces {
		return nil
	}
	available, err := a.availableAfter(debit, posted, pendingDebits, pendingCredits, delta)
	if err != nil {
		return fmt.Errorf("cuenta %s: %w", a.Code, err)
	}
	if available.Units() < a.LowerLimit {
		return fmt.Errorf("%w: cuenta %s, disponible %d, límite %d", ErrInsufficientFunds, a.Code, available.Units(), a.LowerLimit)
	}
	return nil
}

// availableAfter solo se llama con un movimiento que reduce el saldo natural. En una cuenta de
// crédito eso es un débito (delta > 0), así que posted + delta > MinInt64 y el saldo natural siempre
// tiene opuesto.
func (a Account) availableAfter(debit bool, posted, pendingDebits, pendingCredits, delta int64) (money.Amount, error) {
	after, err := money.New(posted, a.Currency).Add(money.New(delta, a.Currency))
	if err != nil {
		return money.Amount{}, err
	}
	outgoing := pendingCredits
	if !debit {
		outgoing = pendingDebits
	}
	natural := money.New(NaturalBalance(a.Kind, after.Units()), a.Currency)
	return natural.Sub(money.New(outgoing, a.Currency))
}
