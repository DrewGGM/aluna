package domain

import (
	"errors"
	"fmt"
	"slices"

	"github.com/DrewGGM/aluna/internal/ids"
	"github.com/DrewGGM/aluna/internal/money"
)

var (
	// ErrTooFewEntries indica una transacción con menos de dos entries.
	ErrTooFewEntries = errors.New("una transacción necesita al menos dos entries")
	// ErrZeroAmount indica un entry de importe cero.
	ErrZeroAmount = errors.New("un entry no puede tener importe cero")
	// ErrUnbalanced indica que los entries no suman cero en alguna moneda.
	ErrUnbalanced = errors.New("la transacción no suma cero")
)

// EntryInput es una línea de una transacción. Amount lleva signo: débito positivo, crédito negativo.
type EntryInput struct {
	AccountID ids.ID
	Amount    money.Amount
}

// NewTransactionInput son los datos para armar una transacción.
type NewTransactionInput struct {
	ID      ids.ID
	Entries []EntryInput
}

// Transaction es una transacción que ya cumple la partida doble: al menos dos entries, ninguno en
// cero, y suma cero en cada moneda.
type Transaction struct {
	id      ids.ID
	entries []EntryInput
}

// NewTransaction valida la partida doble y devuelve la transacción; el dinero no se crea ni se
// destruye.
func NewTransaction(in NewTransactionInput) (Transaction, error) {
	if len(in.Entries) < 2 {
		return Transaction{}, fmt.Errorf("%w: tiene %d", ErrTooFewEntries, len(in.Entries))
	}
	sums := map[money.Currency]money.Amount{}
	var order []money.Currency // para que el error nombre siempre la misma moneda
	for i, e := range in.Entries {
		if e.Amount.IsZero() {
			return Transaction{}, fmt.Errorf("%w: entry %d", ErrZeroAmount, i)
		}
		cur := e.Amount.Currency()
		sum, ok := sums[cur]
		if !ok {
			sum = money.New(0, cur) // el valor cero de Amount no tiene moneda y Add lo rechaza
			order = append(order, cur)
		}
		next, err := sum.Add(e.Amount)
		if err != nil {
			return Transaction{}, fmt.Errorf("entry %d: %w", i, err)
		}
		sums[cur] = next
	}
	for _, cur := range order {
		if sum := sums[cur]; !sum.IsZero() {
			return Transaction{}, fmt.Errorf("%w: %s suma %d", ErrUnbalanced, cur.Code(), sum.Units())
		}
	}
	return Transaction{id: in.ID, entries: slices.Clone(in.Entries)}, nil
}

// ID es el identificador de la transacción.
func (t Transaction) ID() ids.ID { return t.id }

// Entries devuelve una copia de los entries: la transacción validada no se puede alterar desde afuera.
func (t Transaction) Entries() []EntryInput { return slices.Clone(t.entries) }
