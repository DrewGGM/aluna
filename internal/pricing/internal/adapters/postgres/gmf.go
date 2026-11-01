package postgres

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/DrewGGM/aluna/internal/calendar"
	"github.com/DrewGGM/aluna/internal/ids"
	"github.com/DrewGGM/aluna/internal/pricing/internal/app"
)

// LockParty serializes, until the transaction ends, the GMF exemptions of party of the entity: an advisory
// lock on its ID, as the party may have no row yet to lock.
func (s Store) LockParty(ctx context.Context, entity, party ids.ID) error {
	_, err := s.q.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended('pricing.gmf_exempt_accounts ' || $1::text || ' ' || $2::text, 0))`,
		entity, party)
	if err != nil {
		return fmt.Errorf("lock the GMF exemptions of party %s: %w", party, err)
	}
	return nil
}

// ExemptAccounts are the accounts of party of the entity exempt from GMF.
func (s Store) ExemptAccounts(ctx context.Context, entity, party ids.ID) ([]ids.ID, error) {
	rows, err := s.q.Query(ctx, `SELECT account_id FROM pricing.gmf_exempt_accounts WHERE legal_entity_id = $1 AND party_id = $2`, entity, party)
	if err != nil {
		return nil, fmt.Errorf("exempt accounts of party %s: %w", party, err)
	}
	accounts, err := pgx.CollectRows(rows, pgx.RowTo[ids.ID])
	if err != nil {
		return nil, fmt.Errorf("exempt accounts of party %s: %w", party, err)
	}
	return accounts, nil
}

// InsertExemption stores in and returns it with its time.
func (s Store) InsertExemption(ctx context.Context, in app.ExemptionInput) (app.Exemption, error) {
	e := app.Exemption{AccountID: in.AccountID, PartyID: in.PartyID}
	err := s.q.QueryRow(ctx, `INSERT INTO pricing.gmf_exempt_accounts (account_id, legal_entity_id, party_id) VALUES ($1, $2, $3)
		RETURNING marked_at`, in.AccountID, in.LegalEntityID, in.PartyID).Scan(&e.MarkedAt)
	if err != nil {
		return app.Exemption{}, fmt.Errorf("exempt account %s: %w", in.AccountID, err)
	}
	return e, nil
}

// IsExempt reports whether account accountID of the entity is exempt from GMF.
func (s Store) IsExempt(ctx context.Context, entity, accountID ids.ID) (bool, error) {
	var exempt bool
	err := s.q.QueryRow(ctx, `SELECT EXISTS (SELECT FROM pricing.gmf_exempt_accounts WHERE account_id = $1 AND legal_entity_id = $2)`,
		accountID, entity).Scan(&exempt)
	if err != nil {
		return false, fmt.Errorf("GMF exemption of account %s: %w", accountID, err)
	}
	return exempt, nil
}

// ExemptUsed is what party's exempt debits used of the exemption of month, locked (FOR UPDATE) until the
// transaction ends; the month's row is created if missing, so that there is one to lock.
func (s Store) ExemptUsed(ctx context.Context, entity, party ids.ID, month calendar.Date) (int64, error) {
	if _, err := s.q.Exec(ctx, `INSERT INTO pricing.gmf_monthly (legal_entity_id, party_id, month, exempt_used_units)
		VALUES ($1, $2, $3, 0) ON CONFLICT DO NOTHING`, entity, party, dateOf(month)); err != nil {
		return 0, fmt.Errorf("GMF exemption of party %s: %w", party, err)
	}
	var used int64
	err := s.q.QueryRow(ctx, `SELECT exempt_used_units FROM pricing.gmf_monthly WHERE legal_entity_id = $1 AND party_id = $2 AND month = $3
		FOR UPDATE`, entity, party, dateOf(month)).Scan(&used)
	if err != nil {
		return 0, fmt.Errorf("GMF exemption of party %s: %w", party, err)
	}
	return used, nil
}

// UseExemption adds units to what party used of the exemption of month, in one statement that locks its row,
// unless that would pass limit: then it is app.ErrQuoteUnavailable.
func (s Store) UseExemption(ctx context.Context, entity, party ids.ID, month calendar.Date, units, limit int64) error {
	tag, err := s.q.Exec(ctx, `INSERT INTO pricing.gmf_monthly AS m (legal_entity_id, party_id, month, exempt_used_units)
		SELECT $1, $2, $3, $4::bigint WHERE $4::bigint <= $5::bigint
		ON CONFLICT (legal_entity_id, party_id, month) DO UPDATE SET exempt_used_units = m.exempt_used_units + EXCLUDED.exempt_used_units
			WHERE m.exempt_used_units + EXCLUDED.exempt_used_units <= $5::bigint`,
		entity, party, dateOf(month), units, limit)
	if err != nil {
		return fmt.Errorf("use the GMF exemption of party %s: %w", party, err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("%w: party %s has less than %d units of its GMF exemption left", app.ErrQuoteUnavailable, party, units)
	}
	return nil
}

// dateOf is day at midnight UTC, as pgx writes a date.
func dateOf(day calendar.Date) time.Time {
	return time.Date(day.Year, day.Month, day.Day, 0, 0, 0, 0, time.UTC)
}
