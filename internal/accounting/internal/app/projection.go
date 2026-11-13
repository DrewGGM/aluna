package app

import (
	"context"
	"errors"

	"github.com/DrewGGM/aluna/internal/accounting/internal/domain"
	"github.com/DrewGGM/aluna/internal/calendar"
	"github.com/DrewGGM/aluna/internal/ids"
)

// Day is a business date of an entity.
type Day struct {
	LegalEntityID ids.ID
	Date          calendar.Date
}

// Mark says up to where a read saw the journal: the oldest transaction in progress before it. One with an id from
// the mark on may have committed after the read.
type Mark string

// Projection is what projecting the daily GL needs.
type Projection interface {
	// DaysToProject are the closed days with transactions, from the entity's first mapping, that have no projection
	// or a transaction from the mark of their projection on: the newest first.
	DaysToProject(ctx context.Context) ([]Day, error)
	// AccountTotals is what each account moved on d, read after taking its mark.
	AccountTotals(ctx context.Context, d Day) ([]domain.AccountTotal, Mark, error)
	// Mappings are all the entity's mappings.
	Mappings(ctx context.Context, entity ids.ID) ([]domain.Mapping, error)
	// ReplaceDay replaces d's projection: its status and lines, as seen up to mark.
	ReplaceDay(ctx context.Context, d Day, status domain.DayStatus, lines []domain.Line, mark Mark) error
}

// ProjectDay projects the GL of d from the journal, replacing what was there: the projection is never edited, only
// recalculated (ADR-0011). A day with an account without a mapping in force is recorded unmapped and without lines,
// so that its GL is not served incomplete and it is not projected again until a transaction reaches it.
func ProjectDay(ctx context.Context, p Projection, d Day) error {
	totals, mark, err := p.AccountTotals(ctx, d)
	if err != nil {
		return err
	}
	mappings, err := p.Mappings(ctx, d.LegalEntityID)
	if err != nil {
		return err
	}
	status := domain.DayProjected
	lines, err := domain.Project(totals, mappings, d.Date)
	switch {
	case errors.Is(err, domain.ErrNoMapping):
		status, lines = domain.DayUnmapped, nil
	case err != nil:
		return err
	}
	return p.ReplaceDay(ctx, d, status, lines, mark)
}
