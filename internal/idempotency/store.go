// Package idempotency guarda la respuesta de cada comando con clave de idempotencia (architecture §9):
// un reintento con la misma clave y el mismo payload recibe la respuesta original sin repetir el
// efecto. La clave se escribe en la misma transacción del efecto, así que los dos se confirman o se
// pierden juntos.
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
	// ErrKeyReused indica una clave ya usada con otro payload (422 idempotency_key_reused).
	ErrKeyReused = errors.New("clave de idempotencia reutilizada con otro payload")
	// ErrInProgress indica que otra solicitud con la misma clave no ha terminado (409 request_in_progress).
	ErrInProgress = errors.New("solicitud con la misma clave en curso")
	// ErrNotClaimed indica un Complete sobre una clave que no se reclamó en la transacción, o que ya
	// se completó.
	ErrNotClaimed = errors.New("clave de idempotencia no reclamada")
	// ErrNotStorable indica una respuesta que no se guarda: solo se guardan 2xx y 4xx.
	ErrNotStorable = errors.New("respuesta no guardable")
)

const (
	// lockNotAvailable es el SQLSTATE de lock_timeout y de NOWAIT sobre una fila bloqueada.
	lockNotAvailable = "55P03"
	// purgeBatch acota cada DELETE de Purge para que quepa en el statement_timeout del pool.
	purgeBatch = 10_000
)

// Claim es el resultado de reclamar una clave: Fresh si es nueva y el comando debe ejecutarse; si no,
// Status y Body son la respuesta guardada.
type Claim struct {
	Fresh  bool
	Status int
	Body   []byte
}

// Store es el almacén de claves sobre infra.idempotency_keys.
type Store struct {
	db postgres.Querier // para Purge, que corre fuera de una transacción de negocio
}

// NewStore crea el almacén.
func NewStore(db postgres.Querier) *Store { return &Store{db: db} }

// Claim reclama (scope, key) dentro de tx. Una clave nueva queda reservada hasta el commit, que exige
// haberla completado. Una clave completada con el mismo requestHash devuelve su respuesta; con otro,
// ErrKeyReused. Si otra transacción insertó la misma clave y no ha terminado, el INSERT la espera hasta
// el lock_timeout del pool: si termina antes, este reintento ve su respuesta; si no, ErrInProgress.
func (s *Store) Claim(ctx context.Context, tx postgres.Tx, scope, key string, requestHash [32]byte) (Claim, error) {
	tag, err := tx.Exec(ctx, `INSERT INTO infra.idempotency_keys (scope, key, request_hash) VALUES ($1, $2, $3)
		ON CONFLICT DO NOTHING`, scope, key, requestHash[:])
	if isLockNotAvailable(err) {
		return Claim{}, ErrInProgress
	}
	if err != nil {
		return Claim{}, fmt.Errorf("reclamar la clave de idempotencia: %w", err)
	}
	if tag.RowsAffected() == 1 {
		return Claim{Fresh: true}, nil
	}
	return stored(ctx, tx, scope, key, requestHash)
}

// stored lee la respuesta guardada de una clave que ya existe. FOR KEY SHARE NOWAIT no deja leer una
// fila que otra transacción está borrando (una purga), porque la respuesta podría desaparecer con su
// commit, pero sí deja que varios reintentos la lean a la vez: con FOR UPDATE, uno de ellos recibiría
// 409 aunque la respuesta ya exista. Una fila que ya no está (la purga confirmó entre el INSERT y esta
// lectura) o que sigue in_progress (un segundo Claim de la misma clave en la misma transacción)
// también es ErrInProgress.
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
		return Claim{}, fmt.Errorf("leer la clave de idempotencia: %w", err)
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

// Complete guarda la respuesta de una clave reclamada con Claim en la misma tx. Solo guarda 2xx y 4xx.
func (s *Store) Complete(ctx context.Context, tx postgres.Tx, scope, key string, status int, body []byte) error {
	if status < 200 || status > 499 {
		return fmt.Errorf("%w: status %d", ErrNotStorable, status)
	}
	if body == nil {
		body = []byte{} // pgx manda nil como NULL, y una clave completada guarda un cuerpo (aunque vacío)
	}
	tag, err := tx.Exec(ctx, `UPDATE infra.idempotency_keys SET status = 'completed', response_code = $3, response_body = $4
		WHERE scope = $1 AND key = $2 AND status = 'in_progress'`, scope, key, status, body)
	if err != nil {
		return fmt.Errorf("completar la clave de idempotencia: %w", err)
	}
	if tag.RowsAffected() != 1 {
		return fmt.Errorf("%w: %s %s", ErrNotClaimed, scope, key)
	}
	return nil
}

// Purge borra las claves de todos los scopes creadas antes de before (architecture §9: 7 días para las
// HTTP) y dice cuántas borró. Borra por lotes, cada uno en su propia sentencia, y salta las filas que
// otra transacción tiene bloqueadas (un reintento leyendo su respuesta): las borrará la próxima purga.
func (s *Store) Purge(ctx context.Context, before time.Time) (int64, error) {
	var total int64
	for {
		tag, err := s.db.Exec(ctx, `DELETE FROM infra.idempotency_keys WHERE (scope, key) IN (
			SELECT scope, key FROM infra.idempotency_keys WHERE created_at < $1
			ORDER BY created_at LIMIT $2 FOR UPDATE SKIP LOCKED)`, before, purgeBatch)
		if err != nil {
			return total, fmt.Errorf("purgar claves de idempotencia: %w", err)
		}
		total += tag.RowsAffected()
		if tag.RowsAffected() < purgeBatch {
			return total, nil
		}
	}
}
