package postgres

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/DrewGGM/aluna/internal/accounting/internal/app"
	"github.com/DrewGGM/aluna/internal/accounting/internal/domain"
	"github.com/DrewGGM/aluna/internal/calendar"
	"github.com/DrewGGM/aluna/internal/ids"
	"github.com/DrewGGM/aluna/internal/money"
)

func amountOf(units int64, currency string) (money.Amount, error) {
	c, err := money.CurrencyOf(currency)
	if err != nil {
		return money.Amount{}, err
	}
	return money.New(units, c), nil
}

// DaysToProject are the closed days of the entities that map, from their first mapping, with transactions that have
// no projection or one with an id from the mark of their projection on: the newest first, so that a catch-up does
// the recent days before the old ones. It reads the ledger and the calendar, read-only.
func (s Store) DaysToProject(ctx context.Context) ([]app.Day, error) {
	rows, err := s.q.Query(ctx, `SELECT t.legal_entity_id, t.business_date
		FROM ledger.transactions t
		JOIN calendar.legal_entities l ON l.id = t.legal_entity_id
		JOIN (SELECT legal_entity_id, min(valid_from) AS adopted FROM accounting.cuif_mappings GROUP BY legal_entity_id) m
			ON m.legal_entity_id = t.legal_entity_id
		LEFT JOIN accounting.gl_days d ON d.legal_entity_id = t.legal_entity_id AND d.business_date = t.business_date
		WHERE t.business_date >= m.adopted AND t.business_date < l.business_date
			AND (d.seen_below IS NULL OR t.posted_xact >= d.seen_below)
		GROUP BY t.legal_entity_id, t.business_date
		ORDER BY t.business_date DESC, t.legal_entity_id`)
	if err != nil {
		return nil, fmt.Errorf("read the days to project: %w", err)
	}
	days, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (app.Day, error) {
		var d app.Day
		var day time.Time
		err := row.Scan(&d.LegalEntityID, &day)
		d.Date = calendar.DateOf(day)
		return d, err
	})
	if err != nil {
		return nil, fmt.Errorf("read the days to project: %w", err)
	}
	return days, nil
}

// AccountTotals is what each account of the entity moved on d, by currency, and the mark of what the read sees: the
// oldest transaction in progress before it, which commits later or not at all. The mark is taken first: whatever
// commits between the two statements has an id from it on.
func (s Store) AccountTotals(ctx context.Context, d app.Day) ([]domain.AccountTotal, app.Mark, error) {
	var mark app.Mark
	if err := s.q.QueryRow(ctx, `SELECT pg_snapshot_xmin(pg_current_snapshot())::text`).Scan(&mark); err != nil {
		return nil, "", fmt.Errorf("mark the projection of %v: %w", d, err)
	}
	rows, err := s.q.Query(ctx, `SELECT a.code, e.currency::text,
			coalesce(sum(e.amount) FILTER (WHERE e.amount > 0), 0)::bigint, coalesce(-sum(e.amount) FILTER (WHERE e.amount < 0), 0)::bigint
		FROM ledger.transactions t
		JOIN ledger.entries e ON e.transaction_id = t.id
		JOIN ledger.accounts a ON a.id = e.account_id
		WHERE t.legal_entity_id = $1 AND t.business_date = $2
		GROUP BY a.code, e.currency`, d.LegalEntityID, d.Date.Time())
	if err != nil {
		return nil, "", fmt.Errorf("read the accounts of %v: %w", d, err)
	}
	totals, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (domain.AccountTotal, error) {
		var t domain.AccountTotal
		var currency string
		var debits, credits int64
		if err := row.Scan(&t.Account, &currency, &debits, &credits); err != nil {
			return domain.AccountTotal{}, err
		}
		var err error
		if t.Debits, err = amountOf(debits, currency); err != nil {
			return domain.AccountTotal{}, err
		}
		t.Credits, err = amountOf(credits, currency)
		return t, err
	})
	if err != nil {
		return nil, "", fmt.Errorf("read the accounts of %v: %w", d, err)
	}
	return totals, mark, nil
}

// ReplaceDay records d's projection, its status and mark, and replaces its lines with lines.
func (s Store) ReplaceDay(ctx context.Context, d app.Day, status domain.DayStatus, lines []domain.Line, mark app.Mark) error {
	day := d.Date.Time()
	_, err := s.q.Exec(ctx, `INSERT INTO accounting.gl_days (legal_entity_id, business_date, status, seen_below) VALUES ($1, $2, $3, $4::xid8)
		ON CONFLICT (legal_entity_id, business_date)
		DO UPDATE SET status = EXCLUDED.status, seen_below = EXCLUDED.seen_below, projected_at = clock_timestamp()`,
		d.LegalEntityID, day, status, string(mark))
	if err != nil {
		return fmt.Errorf("record the GL of %v: %w", d, err)
	}
	if _, err := s.q.Exec(ctx, `DELETE FROM accounting.gl_lines WHERE legal_entity_id = $1 AND business_date = $2`, d.LegalEntityID, day); err != nil {
		return fmt.Errorf("clear the GL of %v: %w", d, err)
	}
	for _, l := range lines {
		_, err := s.q.Exec(ctx, `INSERT INTO accounting.gl_lines (legal_entity_id, business_date, cuif_code, currency, debit_units, credit_units)
			VALUES ($1, $2, $3, $4, $5, $6)`, d.LegalEntityID, day, l.CUIFCode, l.Debits.Currency().Code(), l.Debits.Units(), l.Credits.Units())
		if err != nil {
			return fmt.Errorf("store the GL of %v: %w", d, err)
		}
	}
	return nil
}

// DailyGL is the projection of the entity's day, read in one statement so that its status, time and lines are of
// the same projection: domain.ErrNotProjected if there is none, domain.ErrDayUnmapped if it has no lines for want of
// a mapping.
func (s Store) DailyGL(ctx context.Context, entity ids.ID, day calendar.Date) (domain.DailyGL, error) {
	rows, err := s.q.Query(ctx, `SELECT d.status, d.projected_at, l.cuif_code, l.currency::text, l.debit_units, l.credit_units
		FROM accounting.gl_days d
		LEFT JOIN accounting.gl_lines l ON l.legal_entity_id = d.legal_entity_id AND l.business_date = d.business_date
		WHERE d.legal_entity_id = $1 AND d.business_date = $2
		ORDER BY l.cuif_code, l.currency`, entity, day.Time())
	if err != nil {
		return domain.DailyGL{}, fmt.Errorf("read the GL of %v: %w", day, err)
	}
	defer rows.Close()
	var gl domain.DailyGL
	found := false
	for rows.Next() {
		var code, currency *string
		var debits, credits *int64
		if err := rows.Scan(&gl.Status, &gl.ProjectedAt, &code, &currency, &debits, &credits); err != nil {
			return domain.DailyGL{}, fmt.Errorf("read the GL of %v: %w", day, err)
		}
		found = true
		if code == nil {
			continue
		}
		l, err := lineOf(*code, *currency, *debits, *credits)
		if err != nil {
			return domain.DailyGL{}, fmt.Errorf("read the GL of %v: %w", day, err)
		}
		gl.Lines = append(gl.Lines, l)
	}
	if err := rows.Err(); err != nil {
		return domain.DailyGL{}, fmt.Errorf("read the GL of %v: %w", day, err)
	}
	switch {
	case !found:
		return domain.DailyGL{}, domain.ErrNotProjected
	case gl.Status == domain.DayUnmapped:
		return domain.DailyGL{}, domain.ErrDayUnmapped
	}
	return gl, nil
}

func lineOf(code, currency string, debits, credits int64) (domain.Line, error) {
	d, err := amountOf(debits, currency)
	if err != nil {
		return domain.Line{}, err
	}
	c, err := amountOf(credits, currency)
	return domain.Line{CUIFCode: code, Debits: d, Credits: c}, err
}
