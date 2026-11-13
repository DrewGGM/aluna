package app_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/DrewGGM/aluna/internal/calendar/internal/app"
	"github.com/DrewGGM/aluna/internal/calendar/internal/domain"
	"github.com/DrewGGM/aluna/internal/ids"
)

// periods is a PeriodCloser in memory, on business date today.
type periods struct {
	today    domain.Date
	statuses map[string]app.PeriodStatus
	closedBy string
}

func (p *periods) BusinessDate(context.Context, ids.ID) (domain.Date, error) { return p.today, nil }

func (p *periods) PeriodStatus(_ context.Context, _ ids.ID, period string) (app.PeriodStatus, bool, error) {
	s, ok := p.statuses[period]
	return s, ok, nil
}

func (p *periods) MarkClosing(_ context.Context, _ ids.ID, period string) (bool, error) {
	if s, ok := p.statuses[period]; ok && s != app.PeriodOpen {
		return false, nil
	}
	p.statuses[period] = app.PeriodClosing
	return true, nil
}

func (p *periods) MarkClosed(_ context.Context, _ ids.ID, period, by string) error {
	p.statuses[period], p.closedBy = app.PeriodClosed, by
	return nil
}

func newPeriods() *periods {
	return &periods{today: domain.Date{Year: 2026, Month: time.March, Day: 2}, statuses: map[string]app.PeriodStatus{}}
}

// verified records what a close asked to verify and answers err.
type verified struct {
	from, to domain.Date
	status   app.PeriodStatus
	calls    int
	err      error
}

func (v *verified) of(p *periods, period string) func(context.Context, domain.Date, domain.Date) error {
	return func(_ context.Context, from, to domain.Date) error {
		v.from, v.to, v.status, v.calls = from, to, p.statuses[period], v.calls+1
		return v.err
	}
}

// TestAPeriodClosesWhenItHasEnded (FR-37): it is verified while closing, from its first day to the first of the next
// month, and then closed by who approved it.
func TestAPeriodClosesWhenItHasEnded(t *testing.T) {
	p, v := newPeriods(), &verified{}
	if err := app.ClosePeriod(t.Context(), p, ids.New(), "2026-02", "finance-b", v.of(p, "2026-02")); err != nil {
		t.Fatal(err)
	}
	if v.from != (domain.Date{Year: 2026, Month: time.February, Day: 1}) || v.to != (domain.Date{Year: 2026, Month: time.March, Day: 1}) {
		t.Errorf("verified %v to %v", v.from, v.to)
	}
	if v.status != app.PeriodClosing || p.statuses["2026-02"] != app.PeriodClosed || p.closedBy != "finance-b" {
		t.Errorf("verified while %s; then %s by %s", v.status, p.statuses["2026-02"], p.closedBy)
	}
}

// TestAPeriodThatDoesNotVerifyIsNotClosed: it is left closing for the caller's transaction to undo.
func TestAPeriodThatDoesNotVerifyIsNotClosed(t *testing.T) {
	p, v := newPeriods(), &verified{err: errors.New("not square")}
	if err := app.ClosePeriod(t.Context(), p, ids.New(), "2026-02", "finance-b", v.of(p, "2026-02")); !errors.Is(err, v.err) {
		t.Fatalf("ClosePeriod = %v, want %v", err, v.err)
	}
	if p.statuses["2026-02"] != app.PeriodClosing || p.closedBy != "" {
		t.Errorf("status %s by %q", p.statuses["2026-02"], p.closedBy)
	}
}

func TestAPeriodThatCannotCloseIsRejected(t *testing.T) {
	closed := newPeriods()
	closed.statuses["2026-01"] = app.PeriodClosed
	for name, c := range map[string]struct {
		p      *periods
		period string
		want   error
	}{
		"not a month":        {newPeriods(), "2026-13", app.ErrInvalidPeriod},
		"not a period":       {newPeriods(), "february", app.ErrInvalidPeriod},
		"the business month": {newPeriods(), "2026-03", app.ErrPeriodNotEnded},
		"a later month":      {newPeriods(), "2026-04", app.ErrPeriodNotEnded},
		"already closed":     {closed, "2026-01", app.ErrPeriodClosed},
	} {
		if err := app.CheckClose(t.Context(), c.p, ids.New(), c.period); !errors.Is(err, c.want) {
			t.Errorf("%s: CheckClose = %v, want %v", name, err, c.want)
		}
		v := &verified{}
		if err := app.ClosePeriod(t.Context(), c.p, ids.New(), c.period, "finance-b", v.of(c.p, c.period)); !errors.Is(err, c.want) || v.calls != 0 {
			t.Errorf("%s: ClosePeriod = %v after %d verifications, want %v", name, err, v.calls, c.want)
		}
	}
}

// TestAPeriodClosesOnce: one that another close took meanwhile is closed, not verified and closed again.
func TestAPeriodClosesOnce(t *testing.T) {
	p, v := newPeriods(), &verified{}
	p.statuses["2026-02"] = app.PeriodClosing
	if err := app.ClosePeriod(t.Context(), p, ids.New(), "2026-02", "finance-b", v.of(p, "2026-02")); !errors.Is(err, app.ErrPeriodClosed) || v.calls != 0 {
		t.Fatalf("ClosePeriod of a closing period = %v after %d verifications, want ErrPeriodClosed", err, v.calls)
	}
}
