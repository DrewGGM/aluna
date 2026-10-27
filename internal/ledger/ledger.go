// Package ledger es la fachada del ledger de doble partida (ADR-0003): crear cuentas, asentar
// transacciones balanceadas y leer saldos. Los otros módulos solo importan este paquete.
package ledger

import (
	"context"

	"github.com/DrewGGM/aluna/internal/ids"
	pg "github.com/DrewGGM/aluna/internal/ledger/internal/adapters/postgres"
	"github.com/DrewGGM/aluna/internal/ledger/internal/app"
	"github.com/DrewGGM/aluna/internal/ledger/internal/domain"
	"github.com/DrewGGM/aluna/internal/postgres"
)

// Tipos publicados por el ledger.
type (
	// Account es una cuenta del ledger.
	Account = domain.Account
	// Kind es la clase contable de una cuenta.
	Kind = domain.Kind
	// BalanceMode dice cómo se mantiene el saldo de una cuenta.
	BalanceMode = domain.BalanceMode
	// EntryInput es una línea de un posting: débito positivo, crédito negativo.
	EntryInput = domain.EntryInput
	// CreateAccountInput son los datos de una cuenta nueva.
	CreateAccountInput = app.CreateAccountInput
	// PostInput es un posting: cabecera y entries.
	PostInput = app.PostInput
	// Transaction es una transacción asentada: cabecera, posted_at de la base y entries.
	Transaction = app.Transaction
	// Balance es el saldo materializado de una cuenta, con signo.
	Balance = app.Balance
)

// Clases contables y modos de saldo.
const (
	KindAsset        = domain.KindAsset
	KindLiability    = domain.KindLiability
	KindEquity       = domain.KindEquity
	KindIncome       = domain.KindIncome
	KindExpense      = domain.KindExpense
	BalanceModeSync  = domain.BalanceModeSync
	BalanceModeAsync = domain.BalanceModeAsync
)

// Errores que devuelve el ledger.
var (
	ErrInsufficientFunds    = domain.ErrInsufficientFunds
	ErrUnbalanced           = domain.ErrUnbalanced
	ErrTooFewEntries        = domain.ErrTooFewEntries
	ErrZeroAmount           = domain.ErrZeroAmount
	ErrAccountNotFound      = app.ErrAccountNotFound
	ErrInvalidPosting       = app.ErrInvalidPosting
	ErrCurrencyMismatch     = app.ErrCurrencyMismatch
	ErrInvalidAccount       = app.ErrInvalidAccount
	ErrTransactionNotFound  = app.ErrTransactionNotFound
	ErrDuplicateExternalRef = app.ErrDuplicateExternalRef
)

// NaturalBalance convierte un saldo con signo al de la naturaleza de la cuenta: un depósito de cliente
// (pasivo) se ve positivo.
func NaturalBalance(k Kind, posted int64) int64 { return domain.NaturalBalance(k, posted) }

// Service es la entrada al ledger para los demás módulos y los adaptadores de entrada.
type Service struct{}

// New crea el servicio del ledger.
func New() *Service { return &Service{} }

// CreateAccount crea una cuenta dentro de tx.
func (s *Service) CreateAccount(ctx context.Context, tx postgres.Tx, in CreateAccountInput) (Account, error) {
	return app.CreateAccount(ctx, pg.NewStore(tx), in)
}

// Post asienta una transacción balanceada dentro de tx: bloquea los saldos de sus cuentas sync, revisa
// los límites y actualiza los saldos en la misma transacción de base. Si la entidad ya asentó
// (source, external_ref), devuelve esa transacción original con ErrDuplicateExternalRef y no escribe
// nada: un productor interno que reintenta la toma como hecha.
func (s *Service) Post(ctx context.Context, tx postgres.Tx, in PostInput) (Transaction, error) {
	return app.Post(ctx, pg.NewStore(tx), in)
}

// Account lee una cuenta de la entidad: la de otra entidad no existe para ella. q puede ser el pool o
// una Tx.
func (s *Service) Account(ctx context.Context, q postgres.Querier, entity, id ids.ID) (Account, error) {
	return pg.NewStore(q).AccountOf(ctx, entity, id)
}

// Transaction lee una transacción asentada de la entidad, con sus entries. q puede ser el pool o una
// Tx.
func (s *Service) Transaction(ctx context.Context, q postgres.Querier, entity, id ids.ID) (Transaction, error) {
	return pg.NewStore(q).TransactionOf(ctx, entity, id)
}

// Balance lee el saldo materializado de una cuenta, sin bloquearlo. q puede ser el pool o una Tx.
func (s *Service) Balance(ctx context.Context, q postgres.Querier, accountID ids.ID) (Balance, error) {
	return pg.NewStore(q).Balance(ctx, accountID)
}
