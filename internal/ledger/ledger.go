// Package ledger es la fachada del ledger de doble partida (ADR-0003): crear cuentas, asentar
// transacciones balanceadas y leer saldos. Los otros módulos solo importan este paquete.
package ledger

import (
	"context"
	"fmt"
	"time"

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
	// ReverseInput pide revertir una transacción.
	ReverseInput = app.ReverseInput
	// Hold es una reserva de fondos de dos fases sobre una cuenta, con su contrapartida.
	Hold = app.Hold
	// HoldStatus es el estado de un hold.
	HoldStatus = domain.HoldStatus
	// PlaceHoldInput pide retener fondos.
	PlaceHoldInput = app.PlaceHoldInput
	// CaptureInput pide asentar parte o todo un hold.
	CaptureInput = app.CaptureInput
	// ReleaseInput pide liberar un hold.
	ReleaseInput = app.ReleaseInput
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
	HoldPending      = domain.HoldPending
	HoldCaptured     = domain.HoldCaptured
	HoldReleased     = domain.HoldReleased
	HoldExpired      = domain.HoldExpired
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
	ErrAlreadyReversed      = app.ErrAlreadyReversed
	ErrHoldNotFound         = app.ErrHoldNotFound
	ErrInvalidHold          = domain.ErrInvalidHold
	ErrHoldNotPending       = domain.ErrHoldNotPending
	ErrAmountExceedsHold    = domain.ErrAmountExceedsHold
	ErrHoldExpired          = domain.ErrHoldExpired
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

// Reverse asienta dentro de tx la reversa de una transacción de la entidad: sus entries con el signo
// contrario, sujetos a los mismos límites que un posting. Si ya tiene reversa, la devuelve con
// ErrAlreadyReversed y no escribe nada.
func (s *Service) Reverse(ctx context.Context, tx postgres.Tx, in ReverseInput) (Transaction, error) {
	return app.Reverse(ctx, pg.NewStore(tx), in)
}

// PlaceHold retiene fondos de una cuenta sync dentro de tx: valida el disponible y suma el hold a su
// pendiente.
func (s *Service) PlaceHold(ctx context.Context, tx postgres.Tx, in PlaceHoldInput) (Hold, error) {
	return app.PlaceHold(ctx, pg.NewStore(tx), in)
}

// CaptureHold asienta dentro de tx parte o todo un hold pending y libera el resto; devuelve el hold y
// la transacción real.
func (s *Service) CaptureHold(ctx context.Context, tx postgres.Tx, in CaptureInput) (Hold, Transaction, error) {
	return app.CaptureHold(ctx, pg.NewStore(tx), in)
}

// ReleaseHold libera dentro de tx un hold pending.
func (s *Service) ReleaseHold(ctx context.Context, tx postgres.Tx, in ReleaseInput) (Hold, error) {
	return app.ReleaseHold(ctx, pg.NewStore(tx), in)
}

// ExpireDueHolds expira los holds pending vencidos a now, de todas las entidades, en lotes de batch:
// cada lote en su propia transacción, hasta que uno sale incompleto. Los holds que tiene bloqueados un
// capture o un release en curso se saltan; si siguen pending, los toma una corrida posterior. Devuelve
// cuántos expiró, también cuando falla un lote (los anteriores ya quedaron confirmados).
func (s *Service) ExpireDueHolds(ctx context.Context, uow *postgres.UnitOfWork, now time.Time, batch int) (int, error) {
	if batch < 1 {
		return 0, fmt.Errorf("lote de expiración %d: debe ser positivo", batch)
	}
	expired := 0
	for {
		var n int
		err := uow.Do(ctx, func(tx postgres.Tx) error {
			var err error
			n, err = app.ExpireDueHolds(ctx, pg.NewStore(tx), now, batch)
			return err
		})
		if err != nil {
			return expired, err
		}
		expired += n
		if n < batch {
			return expired, nil
		}
	}
}

// AggregateAsyncBalances suma a los saldos de las cuentas async los entries que aún no agregó (FR-9),
// en lotes de batch entries, cada uno en su propia transacción, hasta que uno sale incompleto. Devuelve
// cuántos entries agregó, también cuando falla un lote (los anteriores ya quedaron confirmados).
func (s *Service) AggregateAsyncBalances(ctx context.Context, uow *postgres.UnitOfWork, batch int) (int, error) {
	if batch < 1 {
		return 0, fmt.Errorf("lote de agregación %d: debe ser positivo", batch)
	}
	aggregated := 0
	for {
		var n int
		err := uow.Do(ctx, func(tx postgres.Tx) error {
			var err error
			n, err = app.AggregateAsyncBalances(ctx, pg.NewStore(tx), batch)
			return err
		})
		if err != nil {
			return aggregated, err
		}
		aggregated += n
		if n < batch {
			return aggregated, nil
		}
	}
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
