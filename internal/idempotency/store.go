// Package idempotency stores the response of each command with an idempotency key (architecture §9): a
// retry with the same key and the same payload gets the original response without repeating the effect.
// The key is written in the same transaction as the effect, so both commit or are lost together.
package idempotency

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/DrewGGM/aluna/internal/postgres"
)

var (
	// ErrKeyReused means a key already used with another payload (422 idempotency_key_reused).
	ErrKeyReused = errors.New("idempotency key reused with a different payload")
	// ErrInProgress means another request with the same key has not finished (409 request_in_progress).
	ErrInProgress = errors.New("request with the same key in progress")
	// ErrNotClaimed means a Complete on a key that was not claimed in the transaction, or that was already
	// completed.
	ErrNotClaimed = errors.New("idempotency key not claimed")
	// ErrNotStorable means a response that is not stored: only 2xx and 4xx are stored.
	ErrNotStorable = errors.New("response not storable")
)

const (
	// lockNotAvailable is the SQLSTATE of lock_timeout and of NOWAIT on a locked row.
	lockNotAvailable = "55P03"
	// purgeBatch bounds each Purge DELETE so that it fits in the pool's statement_timeout.
	purgeBatch = 10_000
)

// Claim is the result of claiming a key: Fresh if it is new and the command must run; otherwise, Status
// and Body are the stored response.
type Claim struct {
	Fresh  bool
	Status int
	Body   []byte
}

// Store is the key store over infra.idempotency_keys.
type Store struct {
	db postgres.Querier // for Purge, which runs outside a business transaction
}

// NewStore creates the store.
func NewStore(db postgres.Querier) *Store { return &Store{db: db} }

// Claim claims (scope, key) within tx. A new key stays reserved until the commit, which requires it to
// be completed. A completed key with the same requestHash returns its response; with another one,
// ErrKeyReused. If another transaction inserted the same key and has not finished, the INSERT waits for
// it up to the pool's lock_timeout: if it finishes first, this retry sees its response; otherwise,
// ErrInProgress.
func (s *Store) Claim(ctx context.Context, tx postgres.Tx, scope, key string, requestHash [32]byte) (Claim, error) {
	tag, err := tx.Exec(ctx, `INSERT INTO infra.idempotency_keys (scope, key, request_hash) VALUES ($1, $2, $3)
		ON CONFLICT DO NOTHING`, scope, key, requestHash[:])
	if isLockNotAvailable(err) {
		return Claim{}, ErrInProgress
	}
	if err != nil {
		return Claim{}, fmt.Errorf("claim idempotency key: %w", err)
	}
	if tag.RowsAffected() == 1 {
		return Claim{Fresh: true}, nil
	}
	return stored(ctx, tx, scope, key, requestHash)
}

// stored reads the stored response of a key that already exists. FOR KEY SHARE NOWAIT does not allow
// reading a row another transaction is deleting (a purge), because the response could vanish with its
// commit, but it does let several retries read it at once: with FOR UPDATE, one of them would get 409
// even though the response exists. A row that is gone (the purge committed between the INSERT and this
// read) or that is still in_progress (a second Claim of the same key in the same transaction) is also
// ErrInProgress.
func stored(ctx context.Context, tx postgres.Tx, scope, key string, requestHash [32]byte) (Claim, error) {
	var (
		hash   []byte
		status string
		code   *int
		c      Claim
	)
	err := tx.QueryRow(ctx, `SELECT request_hash, status, response_code, response_body FROM infra.idempotency_keys
		WHERE scope = $1 AND key = $2 FOR KEY SHARE NOWAIT`, scope, key).Scan(&hash, &status, &code, &c.Body)
	if isLockNotAvailable(err) || errors.Is(err, pgx.ErrNoRows) {
		return Claim{}, ErrInProgress
	}
	if err != nil {
		return Claim{}, fmt.Errorf("read idempotency key: %w", err)
	}
	if !bytes.Equal(hash, requestHash[:]) {
		return Claim{}, ErrKeyReused
	}
	if status != "completed" {
		return Claim{}, ErrInProgress
	}
	c.Status = *code
	return c, nil
}

func isLockNotAvailable(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == lockNotAvailable
}

// Complete stores the response of a key claimed with Claim in the same tx. It only stores 2xx and 4xx.
func (s *Store) Complete(ctx context.Context, tx postgres.Tx, scope, key string, status int, body []byte) error {
	if status < 200 || status > 499 {
		return fmt.Errorf("%w: status %d", ErrNotStorable, status)
	}
	if body == nil {
		body = []byte{} // pgx sends nil as NULL, and a completed key stores a body (even an empty one)
	}
	tag, err := tx.Exec(ctx, `UPDATE infra.idempotency_keys SET status = 'completed', response_code = $3, response_body = $4
		WHERE scope = $1 AND key = $2 AND status = 'in_progress'`, scope, key, status, body)
	if err != nil {
		return fmt.Errorf("complete idempotency key: %w", err)
	}
	if tag.RowsAffected() != 1 {
		return fmt.Errorf("%w: %s %s", ErrNotClaimed, scope, key)
	}
	return nil
}

// Purge deletes the keys of every scope created before before (architecture §9: 7 days for HTTP ones)
// and reports how many it deleted. It deletes in batches, each in its own statement, and skips rows
// another transaction has locked (a retry reading its response): the next purge will delete them.
func (s *Store) Purge(ctx context.Context, before time.Time) (int64, error) {
	var total int64
	for {
		tag, err := s.db.Exec(ctx, `DELETE FROM infra.idempotency_keys WHERE (scope, key) IN (
			SELECT scope, key FROM infra.idempotency_keys WHERE created_at < $1
			ORDER BY created_at LIMIT $2 FOR UPDATE SKIP LOCKED)`, before, purgeBatch)
		if err != nil {
			return total, fmt.Errorf("purge idempotency keys: %w", err)
		}
		total += tag.RowsAffected()
		if tag.RowsAffected() < purgeBatch {
			return total, nil
		}
	}
}
