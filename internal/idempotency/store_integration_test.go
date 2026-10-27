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

// claim hace lo que hará el middleware HTTP en una transacción: reclama la clave y, si es nueva,
// guarda status y body como su respuesta.
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

// mustClaimFresh reclama una clave que debe ser nueva y guarda su respuesta.
func mustClaimFresh(t *testing.T, s *Store, key string, hash [32]byte, status int, body string) {
	t.Helper()
	if c, err := claim(t.Context(), s, txScope, key, hash, status, []byte(body)); err != nil || !c.Fresh {
		t.Fatalf("Claim de una clave nueva = %+v, %v; se esperaba Fresh", c, err)
	}
}

func TestReplayReturnsTheStoredResponse(t *testing.T) {
	s := NewStore(testPool)
	for _, status := range []int{200, 201, 422, 499} { // 2xx y 4xx de negocio se guardan
		key := ids.New().String()
		mustClaimFresh(t, s, key, hashOf("a"), status, `{"id":"1"}`)

		c, err := claim(t.Context(), s, txScope, key, hashOf("a"), 500, nil)

		if err != nil || c.Fresh || c.Status != status || string(c.Body) != `{"id":"1"}` {
			t.Errorf("replay de un %d = %+v, %v; se esperaba la respuesta guardada", status, c, err)
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
		t.Fatalf("replay de un 204 = %+v, %v", c, err)
	}
}

func TestSameKeyWithAnotherPayloadIsRejected(t *testing.T) {
	s, key := NewStore(testPool), ids.New().String()
	mustClaimFresh(t, s, key, hashOf("a"), 201, `{}`)

	if _, err := claim(t.Context(), s, txScope, key, hashOf("b"), 201, nil); !errors.Is(err, ErrKeyReused) {
		t.Fatalf("otro payload con la misma clave = %v, se esperaba ErrKeyReused", err)
	}
}

func TestTheSameKeyInAnotherScopeIsIndependent(t *testing.T) {
	s, key := NewStore(testPool), ids.New().String()
	mustClaimFresh(t, s, key, hashOf("a"), 201, `{}`)

	c, err := claim(t.Context(), s, "POST /v1/ledger-accounts", key, hashOf("b"), 201, nil)

	if err != nil || !c.Fresh {
		t.Fatalf("la misma clave en otro scope = %+v, %v; se esperaba Fresh", c, err)
	}
}

// holdClaim reclama key en una transacción que sigue abierta hasta que se cierra release; entonces
// termina con finish. Vuelve cuando la clave ya está reclamada; el resultado de la transacción llega
// por el canal.
func holdClaim(t *testing.T, s *Store, key string, release <-chan struct{}, finish func(tx postgres.Tx) error) <-chan error {
	t.Helper()
	claimed, done := make(chan struct{}), make(chan error, 1)
	go func() {
		done <- inTx(t.Context(), func(tx postgres.Tx) error {
			if c, err := s.Claim(t.Context(), tx, txScope, key, hashOf("a")); err != nil || !c.Fresh {
				return errors.Join(err, errors.New("la primera debía ser Fresh"))
			}
			close(claimed)
			<-release
			return finish(tx)
		})
	}()
	select {
	case <-claimed:
	case err := <-done:
		t.Fatalf("la primera transacción no reclamó la clave: %v", err)
	}
	return done
}

func complete201(s *Store, key string) func(tx postgres.Tx) error {
	return func(tx postgres.Tx) error {
		return s.Complete(context.Background(), tx, txScope, key, 201, []byte(`{}`))
	}
}

// TestConcurrentClaimIsInProgress: si la primera transacción no termina dentro del lock_timeout del
// pool (2 s), la segunda no repite el efecto: recibe ErrInProgress.
func TestConcurrentClaimIsInProgress(t *testing.T) {
	s, key, release := NewStore(testPool), ids.New().String(), make(chan struct{})
	first := holdClaim(t, s, key, release, complete201(s, key))

	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second) // Claim no debe esperar sin límite
	defer cancel()
	_, err := claim(ctx, s, txScope, key, hashOf("a"), 201, nil)
	close(release)

	if !errors.Is(err, ErrInProgress) {
		t.Fatalf("Claim concurrente = %v, se esperaba ErrInProgress", err)
	}
	if err := <-first; err != nil {
		t.Fatal(err)
	}
	if c, err := claim(t.Context(), s, txScope, key, hashOf("a"), 500, nil); err != nil || c.Fresh || c.Status != 201 {
		t.Fatalf("después de la primera = %+v, %v; se esperaba su respuesta", c, err)
	}
}

// TestAConcurrentClaimWaitsForAShortFirst: si la primera termina dentro del lock_timeout, la segunda
// espera y recibe su respuesta; si la primera se revierte, la segunda queda como nueva.
func TestAConcurrentClaimWaitsForAShortFirst(t *testing.T) {
	s := NewStore(testPool)
	rollback := func(postgres.Tx) error { return errors.New("el comando falló") }
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
				t.Fatalf("la primera transacción = %v", firstErr)
			}
			if err != nil || c.Fresh != tc.wantFresh || (!c.Fresh && c.Status != 201) {
				t.Fatalf("la segunda = %+v, %v; se esperaba Fresh = %v", c, err, tc.wantFresh)
			}
		})
	}
}

// TestALockedStoredKeyIsInProgress: si otra transacción tiene la fila bloqueada, el reintento no lee
// una respuesta que puede desaparecer: ErrInProgress. Con FOR UPDATE responde de inmediato (NOWAIT);
// con un DELETE en curso (una purga), tras el lock_timeout del pool.
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
				t.Fatalf("Claim sobre una fila bloqueada = %v, se esperaba ErrInProgress", err)
			}
		})
	}
}

// lockRow corre sql sobre key en una transacción que queda abierta hasta llamar la función que devuelve.
func lockRow(t *testing.T, sql, key string) func() {
	t.Helper()
	locker, err := testPool.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := locker.Exec(t.Context(), sql, key); err != nil {
		t.Fatal(err)
	}
	return func() { locker.Rollback(context.Background()) } //nolint:errcheck,gosec // solo suelta el lock
}

// TestAClaimMustBeCompletedToCommit: una clave reclamada y no completada no puede quedar guardada, o
// cada reintento sería ErrInProgress hasta la purga.
func TestAClaimMustBeCompletedToCommit(t *testing.T) {
	s, key := NewStore(testPool), ids.New().String()
	err := inTx(t.Context(), func(tx postgres.Tx) error {
		_, err := s.Claim(t.Context(), tx, txScope, key, hashOf("a"))
		return err
	})
	if err == nil {
		t.Fatal("el commit de un Claim sin Complete debe fallar")
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
			t.Errorf("segundo Claim en la misma transacción = %v, se esperaba ErrInProgress", err)
		}
		return s.Complete(t.Context(), tx, txScope, key, 201, nil)
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestCompleteErrors(t *testing.T) {
	s := NewStore(testPool)
	t.Run("sin Claim", func(t *testing.T) {
		err := inTx(t.Context(), func(tx postgres.Tx) error {
			return s.Complete(t.Context(), tx, txScope, ids.New().String(), 201, nil)
		})
		if !errors.Is(err, ErrNotClaimed) {
			t.Errorf("= %v, se esperaba ErrNotClaimed", err)
		}
	})
	t.Run("dos veces", func(t *testing.T) {
		key := ids.New().String()
		mustClaimFresh(t, s, key, hashOf("a"), 201, `{}`)
		err := inTx(t.Context(), func(tx postgres.Tx) error { return s.Complete(t.Context(), tx, txScope, key, 409, nil) })
		if !errors.Is(err, ErrNotClaimed) {
			t.Errorf("= %v, se esperaba ErrNotClaimed", err)
		}
	})
	t.Run("fuera de 2xx y 4xx", func(t *testing.T) {
		for _, status := range []int{500, 199} { // un 5xx no se guarda: el cliente puede reintentar
			if _, err := claim(t.Context(), s, txScope, ids.New().String(), hashOf("a"), status, nil); !errors.Is(err, ErrNotStorable) {
				t.Errorf("guardar un %d = %v, se esperaba ErrNotStorable", status, err)
			}
		}
	})
}

// TestTheTableEnforcesItsInvariants: aun con SQL directo y el rol de la aplicación, no se fabrica una
// respuesta guardada, la respuesta no cambia, la huella no se toca y un 5xx no se guarda.
func TestTheTableEnforcesItsInvariants(t *testing.T) {
	s, key := NewStore(testPool), ids.New().String()
	mustClaimFresh(t, s, key, hashOf("a"), 201, `{}`)
	cases := map[string]struct {
		sql      string
		sqlState string
	}{
		"insertar una respuesta": {`INSERT INTO infra.idempotency_keys (scope, key, request_hash, status, response_code, response_body)
			VALUES ('s', $1, decode(repeat('00', 32), 'hex'), 'completed', 201, '')`, "42501"},
		"volver a in_progress": {`UPDATE infra.idempotency_keys SET status = 'in_progress', response_code = NULL, response_body = NULL
			WHERE key = $1`, "2F003"},
		"cambiar la respuesta": {`UPDATE infra.idempotency_keys SET response_code = 409 WHERE key = $1`, "2F003"},
		"cambiar la huella":    {`UPDATE infra.idempotency_keys SET request_hash = request_hash WHERE key = $1`, "42501"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := testPool.Exec(t.Context(), tc.sql, key)
			if state := sqlState(err); state != tc.sqlState {
				t.Fatalf("= %v, se esperaba SQLSTATE %s", err, tc.sqlState)
			}
		})
	}
	t.Run("guardar un 5xx", func(t *testing.T) {
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
			t.Fatalf("= %v, se esperaba SQLSTATE 23514", err)
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
		t.Fatalf("Purge borró una clave más nueva que el corte: %+v, %v", c, err)
	}
	n, err := s.Purge(t.Context(), time.Now().Add(time.Hour))
	if err != nil || n < 1 {
		t.Fatalf("Purge = %d, %v; se esperaba al menos una clave borrada", n, err)
	}
	mustClaimFresh(t, s, key, hashOf("b"), 201, `{}`)
}

// TestPurgeSkipsLockedKeys: un reintento que tiene una clave bloqueada no hace fallar la purga del resto.
func TestPurgeSkipsLockedKeys(t *testing.T) {
	s, locked, free := NewStore(testPool), ids.New().String(), ids.New().String()
	mustClaimFresh(t, s, locked, hashOf("a"), 201, `{}`)
	mustClaimFresh(t, s, free, hashOf("a"), 201, `{}`)
	unlock := lockRow(t, `SELECT 1 FROM infra.idempotency_keys WHERE key = $1 FOR UPDATE`, locked)

	_, err := s.Purge(t.Context(), time.Now().Add(time.Hour))
	unlock()

	if err != nil {
		t.Fatalf("Purge con una fila bloqueada = %v", err)
	}
	if c, err := claim(t.Context(), s, txScope, locked, hashOf("a"), 201, nil); err != nil || c.Fresh {
		t.Errorf("la clave bloqueada debía quedar para la próxima purga: %+v, %v", c, err)
	}
	mustClaimFresh(t, s, free, hashOf("b"), 201, `{}`)
}

// TestPurgeDeletesMoreThanOneBatch: con más claves que un lote, Purge sigue hasta borrarlas todas.
func TestPurgeDeletesMoreThanOneBatch(t *testing.T) {
	s, bulk := NewStore(testPool), "purga-"+ids.New().String()
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
		t.Fatalf("Purge = %d, %v; quedaron %d claves del lote", n, err, left)
	}
}
