package app_test

import (
	"context"
	"errors"
	"maps"
	"slices"
	"testing"

	"github.com/DrewGGM/aluna/internal/ids"
	"github.com/DrewGGM/aluna/internal/money"
	"github.com/DrewGGM/aluna/internal/reconciliation/internal/app"
)

var errFailed = errors.New("failed")

// memory is a Recorder that keeps what it is told; subjects in open already have an open break, and last holds
// the subjects of each entity's previous run. Each failing field makes its method fail.
type memory struct {
	runs                                 map[ids.ID]int
	subjects                             map[ids.ID][]string
	breaks                               []app.Break
	published                            []string
	open                                 map[string]bool
	last                                 map[ids.ID][]string
	failLast, failRun, failOpen, failPub bool
}

func newMemory() *memory {
	return &memory{runs: map[ids.ID]int{}, subjects: map[ids.ID][]string{}, open: map[string]bool{}, last: map[ids.ID][]string{}}
}

func (m *memory) LastSubjects(_ context.Context, entity ids.ID, _ app.Control) ([]string, error) {
	if m.failLast {
		return nil, errFailed
	}
	return m.last[entity], nil
}

func (m *memory) RecordRun(_ context.Context, r app.Run) error {
	if m.failRun {
		return errFailed
	}
	m.runs[r.LegalEntityID], m.subjects[r.LegalEntityID] = r.Findings, r.Subjects
	return nil
}

func (m *memory) OpenBreak(_ context.Context, b app.Break) (bool, error) {
	if m.failOpen {
		return false, errFailed
	}
	if m.open[b.Subject] {
		return false, nil
	}
	m.breaks = append(m.breaks, b)
	return true, nil
}

func (m *memory) Publish(_ context.Context, b app.Break) error {
	if m.failPub {
		return errFailed
	}
	m.published = append(m.published, b.Subject)
	return nil
}

func finding(t *testing.T, entity ids.ID, subject string, units int64) app.Finding {
	t.Helper()
	cop, err := money.CurrencyOf("COP")
	if err != nil {
		t.Fatal(err)
	}
	return app.Finding{LegalEntityID: entity, Subject: subject, Difference: money.New(units, cop)}
}

// TestAResultIsRecordedForEveryEntity: an entity with nothing wrong gets its run too, with no findings; a run is
// the evidence that the control looked at it.
func TestAResultIsRecordedForEveryEntity(t *testing.T) {
	clean, broken := ids.New(), ids.New()
	m := newMemory()

	err := app.Record(t.Context(), m, app.TrialBalance, []ids.ID{clean, broken}, []app.Finding{finding(t, broken, "a", 1), finding(t, broken, "b", 2)})
	if err != nil {
		t.Fatal(err)
	}

	if want := (map[ids.ID]int{clean: 0, broken: 2}); !maps.Equal(m.runs, want) {
		t.Errorf("runs = %v, want %v", m.runs, want)
	}
	if want := []string{"a", "b"}; !slices.Equal(m.published, want) {
		t.Errorf("published = %v, want %v", m.published, want)
	}
}

// TestAFindingOfAnEntityNotYetKnownIsRecorded: an entity created between reading the entities and running the
// control still gets its run.
func TestAFindingOfAnEntityNotYetKnownIsRecorded(t *testing.T) {
	late := ids.New()
	m := newMemory()
	if err := app.Record(t.Context(), m, app.TrialBalance, nil, []app.Finding{finding(t, late, "a", 1)}); err != nil {
		t.Fatal(err)
	}
	if m.runs[late] != 1 {
		t.Errorf("runs = %v, want one finding for %s", m.runs, late)
	}
}

// TestABreakAlreadyOpenIsNotPublishedAgain: the difference still counts in the run, but the same break is neither
// opened nor announced twice.
func TestABreakAlreadyOpenIsNotPublishedAgain(t *testing.T) {
	entity := ids.New()
	m := newMemory()
	m.open["known"] = true

	err := app.Record(t.Context(), m, app.TrialBalance, []ids.ID{entity}, []app.Finding{finding(t, entity, "known", 1), finding(t, entity, "new", 2)})
	if err != nil {
		t.Fatal(err)
	}

	if m.runs[entity] != 2 {
		t.Errorf("%d findings, want 2", m.runs[entity])
	}
	if want := []string{"new"}; !slices.Equal(m.published, want) {
		t.Errorf("published = %v, want %v", m.published, want)
	}
}

// TestAFailureStopsTheRecord: each step's error comes back, for the caller's transaction to undo, and a break
// that could not open is not announced.
func TestAFailureStopsTheRecord(t *testing.T) {
	entity := ids.New()
	for name, fail := range map[string]func(*memory){
		"last run": func(m *memory) { m.failLast = true },
		"run":      func(m *memory) { m.failRun = true },
		"break":    func(m *memory) { m.failOpen = true },
		"announce": func(m *memory) { m.failPub = true },
	} {
		m := newMemory()
		m.last[entity] = []string{"a"}
		fail(m)

		err := app.Record(t.Context(), m, app.RailAccountsAtZero, []ids.ID{entity}, []app.Finding{finding(t, entity, "a", 1)})

		if !errors.Is(err, errFailed) {
			t.Errorf("%s: Record = %v, want %v", name, err, errFailed)
		}
		if len(m.published) != 0 {
			t.Errorf("%s: published %v, want nothing", name, m.published)
		}
	}
}

// TestAnOpenedBreakCarriesTheFinding: the break says which control found what, and how much.
func TestAnOpenedBreakCarriesTheFinding(t *testing.T) {
	entity := ids.New()
	m := newMemory()
	if err := app.Record(t.Context(), m, app.TrialBalance, []ids.ID{entity}, []app.Finding{finding(t, entity, "acct", -7)}); err != nil {
		t.Fatal(err)
	}
	if len(m.breaks) != 1 {
		t.Fatalf("breaks = %+v, want one", m.breaks)
	}
	b := m.breaks[0]
	if b.ID == (ids.ID{}) || b.LegalEntityID != entity || b.Control != app.TrialBalance || b.Subject != "acct" || b.Difference.Units() != -7 {
		t.Errorf("break = %+v", b)
	}
}

// TestARailFindingWaitsForASecondRun (QS-2): a cut can still move just after the rollover, so the first run that
// finds a subject records it and opens nothing; the next one that finds it opens it.
func TestARailFindingWaitsForASecondRun(t *testing.T) {
	entity := ids.New()
	found := []app.Finding{finding(t, entity, "clearing", 1)}

	first := newMemory()
	if err := app.Record(t.Context(), first, app.RailAccountsAtZero, []ids.ID{entity}, found); err != nil {
		t.Fatal(err)
	}
	if first.runs[entity] != 1 || len(first.published) != 0 {
		t.Errorf("first run: %d findings, published %v; want 1 and nothing", first.runs[entity], first.published)
	}

	second := newMemory()
	second.last[entity] = []string{"clearing"}
	if err := app.Record(t.Context(), second, app.RailAccountsAtZero, []ids.ID{entity}, found); err != nil {
		t.Fatal(err)
	}
	if want := []string{"clearing"}; !slices.Equal(second.published, want) {
		t.Errorf("second run published %v, want %v", second.published, want)
	}
}

// TestOtherControlsDoNotWait: their statements see one snapshot of what never moves on its own.
func TestOtherControlsDoNotWait(t *testing.T) {
	entity := ids.New()
	for _, c := range []app.Control{app.TrialBalance, app.BalancesMatchEntries, app.CustomersMatchControl} {
		m := newMemory()
		if err := app.Record(t.Context(), m, c, []ids.ID{entity}, []app.Finding{finding(t, entity, "a", 1)}); err != nil {
			t.Fatal(err)
		}
		if len(m.published) != 1 {
			t.Errorf("%s published %v, want the break", c, m.published)
		}
	}
}

// TestAnotherSubjectDoesNotConfirmARailFinding: a clearing account that stays with a balance does not take away
// the wait of a new subject of the same entity.
func TestAnotherSubjectDoesNotConfirmARailFinding(t *testing.T) {
	entity := ids.New()
	m := newMemory()
	m.last[entity] = []string{"clearing"}

	err := app.Record(t.Context(), m, app.RailAccountsAtZero, []ids.ID{entity}, []app.Finding{finding(t, entity, "clearing", 1), finding(t, entity, "suspense", 2)})
	if err != nil {
		t.Fatal(err)
	}

	if want := []string{"clearing"}; !slices.Equal(m.published, want) {
		t.Errorf("published %v, want %v", m.published, want)
	}
}

// TestOnlyARailRunKeepsItsSubjects: the others need them for nothing, and a very broken ledger would make their
// rows huge.
func TestOnlyARailRunKeepsItsSubjects(t *testing.T) {
	entity := ids.New()
	m := newMemory()
	found := []app.Finding{finding(t, entity, "a", 1)}
	if err := app.Record(t.Context(), m, app.TrialBalance, []ids.ID{entity}, found); err != nil {
		t.Fatal(err)
	}
	if len(m.subjects[entity]) != 0 {
		t.Errorf("trial balance kept %v", m.subjects[entity])
	}
	if err := app.Record(t.Context(), m, app.RailAccountsAtZero, []ids.ID{entity}, found); err != nil {
		t.Fatal(err)
	}
	if want := []string{"a"}; !slices.Equal(m.subjects[entity], want) {
		t.Errorf("the rail control kept %v, want %v", m.subjects[entity], want)
	}
}
