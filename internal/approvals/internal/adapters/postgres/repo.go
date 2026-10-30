// Package postgres guarda las solicitudes de maker-checker en approvals.requests.
package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/DrewGGM/aluna/internal/ids"
	"github.com/DrewGGM/aluna/internal/postgres"
)

// Row es una solicitud tal como está guardada. Expired dice que sigue pendiente y ya venció.
type Row struct {
	ID, LegalEntityID    ids.ID
	Kind                 string
	Payload              json.RawMessage
	Maker, Status        string
	Checker, Reason      *string
	Result               json.RawMessage
	CreatedAt, ExpiresAt time.Time
	DecidedAt            *time.Time
	Expired              bool
}

// Decision es cómo se decidió una solicitud: su estado final, quién la decidió y, según el estado, el
// motivo del rechazo, el error de la ejecución o su resultado.
type Decision struct {
	Status, Checker, Reason, Error string
	Result                         json.RawMessage
}

const columns = `id, legal_entity_id, kind, payload, maker, status, checker, reason, result, created_at, expires_at,
	decided_at, status = 'pending' AND expires_at <= now()`

// Insert guarda una solicitud pendiente; la base pone su creación y su vencimiento.
func Insert(ctx context.Context, tx postgres.Tx, id, entity ids.ID, kind string, payload json.RawMessage, maker string) (Row, error) {
	row, err := scan(tx.QueryRow(ctx, `INSERT INTO approvals.requests (id, legal_entity_id, kind, payload, maker)
		VALUES ($1, $2, $3, $4, $5) RETURNING `+columns, id, entity, kind, payload, maker))
	if err != nil {
		return Row{}, fmt.Errorf("guardar la solicitud %s: %w", kind, err)
	}
	return row, nil
}

// Lock bloquea la solicitud id de la entidad para decidirla; found es false si no existe en ella.
func Lock(ctx context.Context, tx postgres.Tx, entity, id ids.ID) (row Row, found bool, err error) {
	row, err = scan(tx.QueryRow(ctx, `SELECT `+columns+` FROM approvals.requests
		WHERE id = $1 AND legal_entity_id = $2 FOR UPDATE`, id, entity))
	if errors.Is(err, pgx.ErrNoRows) {
		return Row{}, false, nil
	}
	if err != nil {
		return Row{}, false, fmt.Errorf("leer la solicitud %s: %w", id, err)
	}
	return row, true, nil
}

// Decide guarda la decisión de la solicitud id, ya bloqueada.
func Decide(ctx context.Context, tx postgres.Tx, id ids.ID, d Decision) (Row, error) {
	row, err := scan(tx.QueryRow(ctx, `UPDATE approvals.requests
		SET status = $2, checker = $3, reason = NULLIF($4, ''), error = NULLIF($5, ''), result = $6, decided_at = now()
		WHERE id = $1 RETURNING `+columns, id, d.Status, d.Checker, d.Reason, d.Error, d.Result))
	if err != nil {
		return Row{}, fmt.Errorf("decidir la solicitud %s: %w", id, err)
	}
	return row, nil
}

// List son hasta limit solicitudes de la entidad, de la más nueva a la más vieja; con status, solo las de
// ese estado, donde "expired" es una pendiente vencida y "pending" una que no venció.
func List(ctx context.Context, q postgres.Querier, entity ids.ID, status string, limit int) ([]Row, error) {
	rows, err := q.Query(ctx, `SELECT `+columns+` FROM approvals.requests
		WHERE legal_entity_id = $1 AND ($2 = '' OR
			CASE WHEN status = 'pending' AND expires_at <= now() THEN 'expired' ELSE status END = $2)
		ORDER BY created_at DESC, id DESC LIMIT $3`, entity, status, limit)
	if err != nil {
		return nil, fmt.Errorf("listar las solicitudes: %w", err)
	}
	list, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (Row, error) { return scan(r) })
	if err != nil {
		return nil, fmt.Errorf("listar las solicitudes: %w", err)
	}
	return list, nil
}

func scan(r pgx.Row) (Row, error) {
	var row Row
	err := r.Scan(&row.ID, &row.LegalEntityID, &row.Kind, &row.Payload, &row.Maker, &row.Status, &row.Checker,
		&row.Reason, &row.Result, &row.CreatedAt, &row.ExpiresAt, &row.DecidedAt, &row.Expired)
	return row, err
}
