//go:build integration

package idempotency

import (
	"context"
	"crypto/sha256"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/DrewGGM/aluna/internal/ids"
	"github.com/DrewGGM/aluna/internal/postgres"
	"github.com/DrewGGM/aluna/internal/postgres/pgtest"
)

var testPool *pgxpool.Pool

func TestMain(m *testing.M) { os.Exit(pgtest.Run(m, &testPool)) }

const txScope = "POST /v1/transactions"

func hashOf(s string) [32]byte { return sha256.Sum256([]byte(s)) }

func inTx(ctx context.Context, fn func(tx postgres.Tx) error) error {
	return postgres.NewUnitOfWork(testPool).Do(ctx, fn)
}

// claim does what the HTTP middleware will do in a transaction: it claims the key and, if it is new,
// stores status and body as its response.
func claim(ctx context.Context, s *Store, scope, key string, hash [32]byte, status int, body []byte) (Claim, error) {
	var c Claim
	err := inTx(ctx, func(tx postgres.Tx) error {
		var err error
		if c, err = s.Claim(ctx, tx, scope, key, hash); err != nil || !c.Fresh {
			return err
		}
		return s.Complete(ctx, tx, scope, key, status, body)
	})
	return c, err
}

// mustClaimFresh claims a key that must be new and stores its response.
func mustClaimFresh(t *testing.T, s *Store, key string, hash [32]byte, status int, body string) {
	t.Helper()
	if c, err := claim(t.Context(), s, txScope, key, hash, status, []byte(body)); err != nil || !c.Fresh {
		t.Fatalf("Claim of a new key = %+v, %v; want Fresh", c, err)
	}
}

func TestReplayReturnsTheStoredResponse(t *testing.T) {
	s := NewStore(testPool)
	for _, status := range []int{200, 201, 422, 499} { // business 2xx and 4xx are stored
		key := ids.New().String()
		mustClaimFresh(t, s, key, hashOf("a"), status, `{"id":"1"}`)

		c, err := claim(t.Context(), s, txScope, key, hashOf("a"), 500, nil)

		if err != nil || c.Fresh || c.Status != status || string(c.Body) != `{"id":"1"}` {
			t.Errorf("replay of a %d = %+v, %v; want the stored response", status, c, err)
		}
	}
}

func TestAResponseWithoutBodyIsStored(t *testing.T) {
	s, key := NewStore(testPool), ids.New().String()
	if c, err := claim(t.Context(), s, txScope, key, hashOf("a"), 204, nil); err != nil || !c.Fresh {
		t.Fatalf("Claim = %+v, %v", c, err)
	}

	c, err := claim(t.Context(), s, txScope, key, hashOf("a"), 500, nil)

	if err != nil || c.Fresh || c.Status != 204 || len(c.Body) != 0 {
		t.Fatalf("replay of a 204 = %+v, %v", c, err)
	}
}

func TestSameKeyWithAnotherPayloadIsRejected(t *testing.T) {
	s, key := NewStore(testPool), ids.New().String()
	mustClaimFresh(t, s, key, hashOf("a"), 201, `{}`)

	if _, err := claim(t.Context(), s, txScope, key, hashOf("b"), 201, nil); !errors.Is(err, ErrKeyReused) {
		t.Fatalf("another payload with the same key = %v, want ErrKeyReused", err)
	}
}

func TestTheSameKeyInAnotherScopeIsIndependent(t *testing.T) {
	s, key := NewStore(testPool), ids.New().String()
	mustClaimFresh(t, s, key, hashOf("a"), 201, `{}`)

	c, err := claim(t.Context(), s, "POST /v1/ledger-accounts", key, hashOf("b"), 201, nil)

	if err != nil || !c.Fresh {
		t.Fatalf("the same key in another scope = %+v, %v; want Fresh", c, err)
	}
}

// holdClaim claims key in a transaction that stays open until release is closed; then it finishes with
// finish. It returns once the key is claimed; the transaction's result arrives on the channel.
func holdClaim(t *testing.T, s *Store, key string, release <-chan struct{}, finish func(tx postgres.Tx) error) <-chan error {
	t.Helper()
	claimed, done := make(chan struct{}), make(chan error, 1)
	go func() {
		done <- inTx(t.Context(), func(tx postgres.Tx) error {
			if c, err := s.Claim(t.Context(), tx, txScope, key, hashOf("a")); err != nil || !c.Fresh {
				return errors.Join(err, errors.New("the first one should be Fresh"))
			}
			close(claimed)
			<-release
			return finish(tx)
		})
	}()
	select {
	case <-claimed:
	case err := <-done:
		t.Fatalf("the first transaction did not claim the key: %v", err)
	}
	return done
}

func complete201(s *Store, key string) func(tx postgres.Tx) error {
	return func(tx postgres.Tx) error {
		return s.Complete(context.Background(), tx, txScope, key, 201, []byte(`{}`))
	}
}

// TestConcurrentClaimIsInProgress: if the first transaction does not finish within the pool's
// lock_timeout (2 s), the second does not repeat the effect: it gets ErrInProgress.
func TestConcurrentClaimIsInProgress(t *testing.T) {
	s, key, release := NewStore(testPool), ids.New().String(), make(chan struct{})
	first := holdClaim(t, s, key, release, complete201(s, key))

	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second) // Claim must not wait indefinitely
	defer cancel()
	_, err := claim(ctx, s, txScope, key, hashOf("a"), 201, nil)
	close(release)

	if !errors.Is(err, ErrInProgress) {
		t.Fatalf("concurrent Claim = %v, want ErrInProgress", err)
	}
	if err := <-first; err != nil {
		t.Fatal(err)
	}
	if c, err := claim(t.Context(), s, txScope, key, hashOf("a"), 500, nil); err != nil || c.Fresh || c.Status != 201 {
		t.Fatalf("after the first one = %+v, %v; want its response", c, err)
	}
}

// TestAConcurrentClaimWaitsForAShortFirst: if the first one finishes within the lock_timeout, the second
// waits and gets its response; if the first one rolls back, the second is treated as new.
func TestAConcurrentClaimWaitsForAShortFirst(t *testing.T) {
	s := NewStore(testPool)
	rollback := func(postgres.Tx) error { return errors.New("the command failed") }
	for name, tc := range map[string]struct {
		rollback  bool
		wantFresh bool
	}{
		"commit":   {},
		"rollback": {rollback: true, wantFresh: true},
	} {
		t.Run(name, func(t *testing.T) {
			key, release := ids.New().String(), make(chan struct{})
			finish := complete201(s, key)
			if tc.rollback {
				finish = rollback
			}
			first := holdClaim(t, s, key, release, finish)
			time.AfterFunc(300*time.Millisecond, func() { close(release) })

			c, err := claim(t.Context(), s, txScope, key, hashOf("a"), 201, []byte(`{}`))

			if firstErr := <-first; (firstErr != nil) != tc.rollback {
				t.Fatalf("the first transaction = %v", firstErr)
			}
			if err != nil || c.Fresh != tc.wantFresh || (!c.Fresh && c.Status != 201) {
				t.Fatalf("the second one = %+v, %v; want Fresh = %v", c, err, tc.wantFresh)
			}
		})
	}
}

// TestALockedStoredKeyIsInProgress: if another transaction has the row locked, the retry does not read a
// response that may vanish: ErrInProgress. With FOR UPDATE it answers immediately (NOWAIT); with an
// ongoing DELETE (a purge), after the pool's lock_timeout.
func TestALockedStoredKeyIsInProgress(t *testing.T) {
	s := NewStore(testPool)
	for name, lock := range map[string]string{
		"FOR UPDATE": `SELECT 1 FROM infra.idempotency_keys WHERE key = $1 FOR UPDATE`,
		"DELETE":     `DELETE FROM infra.idempotency_keys WHERE key = $1`,
	} {
		t.Run(name, func(t *testing.T) {
			key := ids.New().String()
			mustClaimFresh(t, s, key, hashOf("a"), 201, `{}`)
			unlock := lockRow(t, lock, key)
			defer unlock()

			if _, err := claim(t.Context(), s, txScope, key, hashOf("a"), 201, nil); !errors.Is(err, ErrInProgress) {
				t.Fatalf("Claim on a locked row = %v, want ErrInProgress", err)
			}
		})
	}
}

// lockRow runs sql on key in a transaction that stays open until the returned function is called.
func lockRow(t *testing.T, sql, key string) func() {
	t.Helper()
	locker, err := testPool.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := locker.Exec(t.Context(), sql, key); err != nil {
		t.Fatal(err)
	}
	return func() { locker.Rollback(context.Background()) } //nolint:errcheck,gosec // only releases the lock
}

// TestAClaimMustBeCompletedToCommit: a claimed key that was not completed cannot stay stored, or every
// retry would be ErrInProgress until the purge.
func TestAClaimMustBeCompletedToCommit(t *testing.T) {
	s, key := NewStore(testPool), ids.New().String()
	err := inTx(t.Context(), func(tx postgres.Tx) error {
		_, err := s.Claim(t.Context(), tx, txScope, key, hashOf("a"))
		return err
	})
	if err == nil {
		t.Fatal("committing a Claim without Complete must fail")
	}
	mustClaimFresh(t, s, key, hashOf("a"), 201, `{}`)
}

func TestClaimTwiceInTheSameTransaction(t *testing.T) {
	s, key := NewStore(testPool), ids.New().String()
	err := inTx(t.Context(), func(tx postgres.Tx) error {
		if _, err := s.Claim(t.Context(), tx, txScope, key, hashOf("a")); err != nil {
			return err
		}
		if _, err := s.Claim(t.Context(), tx, txScope, key, hashOf("a")); !errors.Is(err, ErrInProgress) {
			t.Errorf("second Claim in the same transaction = %v, want ErrInProgress", err)
		}
		return s.Complete(t.Context(), tx, txScope, key, 201, nil)
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestCompleteErrors(t *testing.T) {
	s := NewStore(testPool)
	t.Run("without Claim", func(t *testing.T) {
		err := inTx(t.Context(), func(tx postgres.Tx) error {
			return s.Complete(t.Context(), tx, txScope, ids.New().String(), 201, nil)
		})
		if !errors.Is(err, ErrNotClaimed) {
			t.Errorf("= %v, want ErrNotClaimed", err)
		}
	})
	t.Run("twice", func(t *testing.T) {
		key := ids.New().String()
		mustClaimFresh(t, s, key, hashOf("a"), 201, `{}`)
		err := inTx(t.Context(), func(tx postgres.Tx) error { return s.Complete(t.Context(), tx, txScope, key, 409, nil) })
		if !errors.Is(err, ErrNotClaimed) {
			t.Errorf("= %v, want ErrNotClaimed", err)
		}
	})
	t.Run("outside 2xx and 4xx", func(t *testing.T) {
		for _, status := range []int{500, 199} { // a 5xx is not stored: the client may retry
			if _, err := claim(t.Context(), s, txScope, ids.New().String(), hashOf("a"), status, nil); !errors.Is(err, ErrNotStorable) {
				t.Errorf("store a %d = %v, want ErrNotStorable", status, err)
			}
		}
	})
}

// TestTheTableEnforcesItsInvariants: even with direct SQL and the application role, a stored response
// cannot be forged, the response does not change, the fingerprint is not touched and a 5xx is not
// stored.
func TestTheTableEnforcesItsInvariants(t *testing.T) {
	s, key := NewStore(testPool), ids.New().String()
	mustClaimFresh(t, s, key, hashOf("a"), 201, `{}`)
	cases := map[string]struct {
		sql      string
		sqlState string
	}{
		"insert a response": {`INSERT INTO infra.idempotency_keys (scope, key, request_hash, status, response_code, response_body)
			VALUES ('s', $1, decode(repeat('00', 32), 'hex'), 'completed', 201, '')`, "42501"},
		"back to in_progress": {`UPDATE infra.idempotency_keys SET status = 'in_progress', response_code = NULL, response_body = NULL
			WHERE key = $1`, "2F003"},
		"change the response":    {`UPDATE infra.idempotency_keys SET response_code = 409 WHERE key = $1`, "2F003"},
		"change the fingerprint": {`UPDATE infra.idempotency_keys SET request_hash = request_hash WHERE key = $1`, "42501"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := testPool.Exec(t.Context(), tc.sql, key)
			if state := sqlState(err); state != tc.sqlState {
				t.Fatalf("= %v, want SQLSTATE %s", err, tc.sqlState)
			}
		})
	}
	t.Run("store a 5xx", func(t *testing.T) {
		k := ids.New().String()
		err := inTx(t.Context(), func(tx postgres.Tx) error {
			if _, err := s.Claim(t.Context(), tx, txScope, k, hashOf("a")); err != nil {
				return err
			}
			_, err := tx.Exec(t.Context(), `UPDATE infra.idempotency_keys SET status = 'completed', response_code = 500,
				response_body = '' WHERE key = $1`, k)
			return err
		})
		if state := sqlState(err); state != "23514" {
			t.Fatalf("= %v, want SQLSTATE 23514", err)
		}
	})
}

func sqlState(err error) string {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		return pgErr.Code
	}
	return ""
}

func TestPurgeDeletesOnlyOlderKeys(t *testing.T) {
	s, key := NewStore(testPool), ids.New().String()
	mustClaimFresh(t, s, key, hashOf("a"), 201, `{}`)

	if _, err := s.Purge(t.Context(), time.Now().Add(-time.Hour)); err != nil {
		t.Fatal(err)
	}
	if c, err := claim(t.Context(), s, txScope, key, hashOf("a"), 201, nil); err != nil || c.Fresh {
		t.Fatalf("Purge deleted a key newer than the cutoff: %+v, %v", c, err)
	}
	n, err := s.Purge(t.Context(), time.Now().Add(time.Hour))
	if err != nil || n < 1 {
		t.Fatalf("Purge = %d, %v; want at least one key deleted", n, err)
	}
	mustClaimFresh(t, s, key, hashOf("b"), 201, `{}`)
}

// TestPurgeSkipsLockedKeys: a retry holding a key locked does not make the purge of the rest fail. With
// FOR KEY SHARE, the lock of a retry reading its response (stored), the purge does not wait either.
func TestPurgeSkipsLockedKeys(t *testing.T) {
	s := NewStore(testPool)
	for _, lock := range []string{"FOR UPDATE", "FOR KEY SHARE"} {
		t.Run(lock, func(t *testing.T) {
			locked, free := ids.New().String(), ids.New().String()
			mustClaimFresh(t, s, locked, hashOf("a"), 201, `{}`)
			mustClaimFresh(t, s, free, hashOf("a"), 201, `{}`)
			unlock := lockRow(t, `SELECT 1 FROM infra.idempotency_keys WHERE key = $1 `+lock, locked)

			_, err := s.Purge(t.Context(), time.Now().Add(time.Hour))
			unlock()

			if err != nil {
				t.Fatalf("Purge with a locked row = %v", err)
			}
			if c, err := claim(t.Context(), s, txScope, locked, hashOf("a"), 201, nil); err != nil || c.Fresh {
				t.Errorf("the locked key should remain for the next purge: %+v, %v", c, err)
			}
			mustClaimFresh(t, s, free, hashOf("b"), 201, `{}`)
		})
	}
}

// TestPurgeDeletesMoreThanOneBatch: with more keys than one batch, Purge keeps going until it deletes
// them all.
func TestPurgeDeletesMoreThanOneBatch(t *testing.T) {
	s, bulk := NewStore(testPool), "purge-"+ids.New().String()
	err := inTx(t.Context(), func(tx postgres.Tx) error {
		if _, err := tx.Exec(t.Context(), `INSERT INTO infra.idempotency_keys (scope, key, request_hash)
			SELECT $1, n::text, decode(repeat('00', 32), 'hex') FROM generate_series(1, $2::int) AS n`, bulk, purgeBatch+1); err != nil {
			return err
		}
		_, err := tx.Exec(t.Context(), `UPDATE infra.idempotency_keys SET status = 'completed', response_code = 201,
			response_body = '' WHERE scope = $1`, bulk)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}

	n, err := s.Purge(t.Context(), time.Now().Add(time.Hour))

	var left int
	if scanErr := testPool.QueryRow(t.Context(), `SELECT count(*) FROM infra.idempotency_keys WHERE scope = $1`, bulk).Scan(&left); scanErr != nil {
		t.Fatal(scanErr)
	}
	if err != nil || n <= purgeBatch || left != 0 {
		t.Fatalf("Purge = %d, %v; %d keys of the batch left", n, err, left)
	}
}
