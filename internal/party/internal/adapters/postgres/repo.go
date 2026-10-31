// Package postgres stores party reference data in party.parties and its versions in party.versions.
package postgres

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/DrewGGM/aluna/internal/ids"
	"github.com/DrewGGM/aluna/internal/party/internal/domain"
	"github.com/DrewGGM/aluna/internal/postgres"
)

// SQLSTATE of unique_violation and the constraint that keeps one external_ref per entity.
const (
	uniqueViolation   = "23505"
	uniqueExternalRef = "parties_external_ref_unique"
)

// Insert stores party p and its first version; the database sets the timestamps. A repeated external_ref
// is domain.ErrAlreadyRegistered, without the reference: it may be a document number.
func Insert(ctx context.Context, tx postgres.Tx, p domain.Party) (domain.Party, error) {
	err := tx.QueryRow(ctx, `INSERT INTO party.parties (id, legal_entity_id, kind, external_ref)
		VALUES ($1, $2, $3, $4) RETURNING created_at`, p.ID, p.LegalEntityID, p.Kind, p.ExternalRef).Scan(&p.CreatedAt)
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == uniqueViolation && pgErr.ConstraintName == uniqueExternalRef {
		return domain.Party{}, domain.ErrAlreadyRegistered
	}
	if err != nil {
		return domain.Party{}, fmt.Errorf("insert party: %w", err)
	}
	if p.Current, err = insertVersion(ctx, tx, p.ID, p.Current); err != nil {
		return domain.Party{}, err
	}
	return p, nil
}

// insertVersion stores version v of party id; the database sets valid_from and recorded_at.
func insertVersion(ctx context.Context, tx postgres.Tx, id ids.ID, v domain.Version) (domain.Version, error) {
	err := tx.QueryRow(ctx, `INSERT INTO party.versions (party_id, version, kyc_status, risk_level, pep, consents)
		VALUES ($1, $2, $3, $4, $5, $6) RETURNING valid_from, recorded_at`,
		id, v.Number, v.KYCStatus, v.RiskLevel, v.PEP, v.Consents).Scan(&v.ValidFrom, &v.RecordedAt)
	if err != nil {
		return domain.Version{}, fmt.Errorf("insert version %d of party %s: %w", v.Number, id, err)
	}
	return v, nil
}

// Get returns party id of the entity with its latest version, or domain.ErrNotFound.
func Get(ctx context.Context, q postgres.Querier, entity, id ids.ID) (domain.Party, error) {
	var p domain.Party
	err := q.QueryRow(ctx, `SELECT p.id, p.legal_entity_id, p.kind, p.external_ref, p.created_at,
			v.version, v.kyc_status, v.risk_level, v.pep, v.consents, v.valid_from, v.recorded_at
		FROM party.parties p
		JOIN LATERAL (SELECT * FROM party.versions WHERE party_id = p.id ORDER BY version DESC LIMIT 1) v ON true
		WHERE p.id = $1 AND p.legal_entity_id = $2`, id, entity).
		Scan(&p.ID, &p.LegalEntityID, &p.Kind, &p.ExternalRef, &p.CreatedAt, &p.Current.Number, &p.Current.KYCStatus,
			&p.Current.RiskLevel, &p.Current.PEP, &p.Current.Consents, &p.Current.ValidFrom, &p.Current.RecordedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.Party{}, fmt.Errorf("%w: %s", domain.ErrNotFound, id)
	}
	if err != nil {
		return domain.Party{}, fmt.Errorf("read party %s: %w", id, err)
	}
	return p, nil
}
