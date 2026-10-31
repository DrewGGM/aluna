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

// Querier is what an adapter can do with the database: the same method set the sqlc code expects. Tx
// satisfies it and, for reads outside a transaction, so does *pgxpool.Pool.
type Querier interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

var (
	_ Querier = Tx{}
	_ Querier = (*pgxpool.Pool)(nil)
)

// Tx is the transaction the modules share within UnitOfWork.Do. It allows querying, but not closing:
// commit and rollback belong to Do, so no module can close the others' transaction.
type Tx struct{ tx pgx.Tx }

// Exec runs a statement within the transaction.
func (t Tx) Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error) {
	return t.tx.Exec(ctx, sql, args...)
}

// Query runs a query within the transaction.
func (t Tx) Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error) { //nolint:ireturn // same contract as pgx.Tx
	return t.tx.Query(ctx, sql, args...)
}

// QueryRow runs a single-row query within the transaction.
func (t Tx) QueryRow(ctx context.Context, sql string, args ...any) pgx.Row { //nolint:ireturn // same contract as pgx.Tx
	return t.tx.QueryRow(ctx, sql, args...)
}

// ErrSharedTx means a Commit or Rollback on Tx.Pgx(): only UnitOfWork.Do closes the transaction.
var ErrSharedTx = errors.New("the transaction is closed by UnitOfWork.Do")

// Pgx is the transaction as a pgx.Tx, for libraries that write to it with their own client: River
// inserts, in the same transaction, the job that publishes an outbox event. Commit and Rollback return
// ErrSharedTx; savepoints (Begin) can be closed.
func (t Tx) Pgx() pgx.Tx { return sharedTx{t.tx} } //nolint:ireturn // pgx.Tx is the interface those libraries ask for

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

// UnitOfWork runs a business command in a single PostgreSQL transaction.
type UnitOfWork struct {
	pool *pgxpool.Pool
}

// NewUnitOfWork creates the Unit of Work over p.
func NewUnitOfWork(p *pgxpool.Pool) *UnitOfWork { return &UnitOfWork{pool: p} }

// Do runs fn in a READ COMMITTED transaction: it commits if fn returns nil and rolls back in any other
// case (error, panic or runtime.Goexit; the panic carries on). On a serialization failure (40001) or a
// deadlock (40P01) it retries the whole transaction up to maxAttempts attempts, with backoff and jitter.
// That is why fn must be repeatable, and adapters must not translate those two errors into domain
// errors: they return them wrapped with %w so that Do recognizes them.
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
		return fmt.Errorf("begin transaction: %w", err)
	}
	// The rollback is not canceled with the request (it must reach the database anyway), but it has its
	// own limit in case the network dropped.
	rollbackCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), rollbackTimeout)
	defer cancel()
	finished := false
	defer func() {
		if !finished { // fn panicked or called runtime.Goexit (t.Fatal): the panic carries on after the rollback
			tx.Rollback(rollbackCtx) //nolint:errcheck,gosec // if it fails, pgx discards the connection; what matters is what interrupted fn
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

// backoff doubles the wait on each attempt and adds up to as much again at random, so that two
// transactions that collided do not collide again at the same time.
func backoff(attempt int) time.Duration {
	wait := baseBackoff << (attempt - 1)
	return wait + rand.N(wait) //nolint:gosec // jitter does not need cryptographic randomness
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
