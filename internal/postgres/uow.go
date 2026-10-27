package postgres

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Querier es lo que un adapter puede hacer con la base: el mismo conjunto de métodos que espera el
// código de sqlc. Lo cumplen Tx y, para lecturas fuera de transacción, *pgxpool.Pool.
type Querier interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

var (
	_ Querier = Tx{}
	_ Querier = (*pgxpool.Pool)(nil)
)

// Tx es la transacción que comparten los módulos dentro de UnitOfWork.Do. Deja consultar, pero no cerrar:
// el commit y el rollback son de Do, así que ningún módulo puede cerrar la transacción de los demás.
type Tx struct{ tx pgx.Tx }

// Exec ejecuta una sentencia dentro de la transacción.
func (t Tx) Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error) {
	return t.tx.Exec(ctx, sql, args...)
}

// Query ejecuta una consulta dentro de la transacción.
func (t Tx) Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error) { //nolint:ireturn // mismo contrato que pgx.Tx
	return t.tx.Query(ctx, sql, args...)
}

// QueryRow ejecuta una consulta de una fila dentro de la transacción.
func (t Tx) QueryRow(ctx context.Context, sql string, args ...any) pgx.Row { //nolint:ireturn // mismo contrato que pgx.Tx
	return t.tx.QueryRow(ctx, sql, args...)
}

// ErrSharedTx indica un Commit o Rollback sobre Tx.Pgx(): la transacción solo la cierra UnitOfWork.Do.
var ErrSharedTx = errors.New("la transacción la cierra UnitOfWork.Do")

// Pgx es la transacción como pgx.Tx, para las librerías que escriben en ella con su propio cliente: River
// inserta en la misma transacción el job que publica un evento del outbox. Commit y Rollback devuelven
// ErrSharedTx; los savepoints (Begin) sí se pueden cerrar.
func (t Tx) Pgx() pgx.Tx { return sharedTx{t.tx} } //nolint:ireturn // pgx.Tx es la interfaz que piden esas librerías

type sharedTx struct{ pgx.Tx }

func (sharedTx) Commit(context.Context) error   { return ErrSharedTx }
func (sharedTx) Rollback(context.Context) error { return ErrSharedTx }

const (
	maxAttempts     = 3 // ADR-0004
	baseBackoff     = 10 * time.Millisecond
	rollbackTimeout = 5 * time.Second

	serializationFailure = "40001"
	deadlockDetected     = "40P01"
)

// UnitOfWork corre un comando de negocio en una sola transacción de PostgreSQL.
type UnitOfWork struct {
	pool *pgxpool.Pool
}

// NewUnitOfWork crea la Unit of Work sobre p.
func NewUnitOfWork(p *pgxpool.Pool) *UnitOfWork { return &UnitOfWork{pool: p} }

// Do corre fn en una transacción READ COMMITTED: hace commit si fn devuelve nil y rollback en cualquier
// otro caso (error, panic o runtime.Goexit; el panic sigue su curso). Ante un conflicto de serialización (40001) o un
// deadlock (40P01) repite toda la transacción hasta maxAttempts intentos, con backoff y jitter. Por eso
// fn tiene que poder repetirse, y los adapters no deben traducir esos dos errores a errores de dominio:
// los devuelven envueltos con %w para que Do los reconozca.
func (u *UnitOfWork) Do(ctx context.Context, fn func(Tx) error) error {
	for attempt := 1; ; attempt++ {
		err := u.once(ctx, fn)
		if attempt == maxAttempts || !retryable(err) {
			return err
		}
		if waitErr := sleep(ctx, backoff(attempt)); waitErr != nil {
			return errors.Join(waitErr, err)
		}
	}
}

func (u *UnitOfWork) once(ctx context.Context, fn func(Tx) error) error {
	tx, err := u.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return fmt.Errorf("iniciar la transacción: %w", err)
	}
	// El rollback no se cancela con el request (tiene que llegar a la base igual), pero tiene su propio
	// límite por si la red se cortó.
	rollbackCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), rollbackTimeout)
	defer cancel()
	finished := false
	defer func() {
		if !finished { // fn hizo panic o runtime.Goexit (t.Fatal): el panic sigue su curso tras el rollback
			tx.Rollback(rollbackCtx) //nolint:errcheck,gosec // si falla, pgx descarta la conexión; lo que importa es lo que interrumpió a fn
		}
	}()
	err = fn(Tx{tx: tx})
	finished = true
	if err != nil {
		if rbErr := tx.Rollback(rollbackCtx); rbErr != nil {
			return errors.Join(err, fmt.Errorf("rollback: %w", rbErr))
		}
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit: %w", err)
	}
	return nil
}

func retryable(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && (pgErr.Code == serializationFailure || pgErr.Code == deadlockDetected)
}

// backoff duplica la espera en cada intento y le suma hasta otro tanto al azar, para que dos
// transacciones que chocaron no vuelvan a chocar al mismo tiempo.
func backoff(attempt int) time.Duration {
	wait := baseBackoff << (attempt - 1)
	return wait + rand.N(wait) //nolint:gosec // el jitter no necesita aleatoriedad criptográfica
}

func sleep(ctx context.Context, d time.Duration) error {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}
