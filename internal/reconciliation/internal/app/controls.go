// Package app holds reconciliation's use cases: the continuous controls over the ledger (FR-33), the rail's
// statements (FR-34) and the breaks they open.
package app

import (
	"context"
	"slices"

	"github.com/DrewGGM/aluna/internal/ids"
	"github.com/DrewGGM/aluna/internal/money"
)

// Control is what found a difference: one of the continuous controls, or StatementMatch.
type Control string

// The controls of FR-33.
const (
	// TrialBalance checks that the journal sums zero by entity and currency.
	TrialBalance Control = "trial_balance"
	// BalancesMatchEntries checks that each account's materialized balance is the sum of its entries.
	BalancesMatchEntries Control = "balances_match_entries"
	// RailAccountsAtZero checks that each clearing and suspense account was at zero at the last cut: the close of
	// the entity's previous business date.
	RailAccountsAtZero Control = "rail_accounts_at_zero"
	// CustomersMatchControl checks, by entity and currency, the customers' balances against the journal's total
	// of their accounts, the control figure.
	CustomersMatchControl Control = "customers_match_control"
)

// Controls are all the controls, in the order a run goes through them.
var Controls = []Control{TrialBalance, BalancesMatchEntries, RailAccountsAtZero, CustomersMatchControl}

// Finding is a difference a control or a statement found: what does not square (a currency, a ledger account, or a
// direction and an end-to-end id), by how much and, in a statement, why.
type Finding struct {
	LegalEntityID ids.ID
	Subject       string
	Reason        Reason
	Difference    money.Amount
}

// Run is the result of a control over an entity: how many findings it had and, for a control that waits for a
// second run, their subjects.
type Run struct {
	ID, LegalEntityID ids.ID
	Control           Control
	Findings          int
	Subjects          []string
}

// controlsOwner is who works the breaks of the controls.
const controlsOwner = "finance"

// runKeeper keeps the runs of a control.
type runKeeper interface {
	// LastSubjects are the subjects the entity's last run of c kept; none if it never ran.
	LastSubjects(ctx context.Context, entity ids.ID, c Control) ([]string, error)
	RecordRun(ctx context.Context, r Run) error
}

// Recorder keeps the results of a control.
type Recorder interface {
	runKeeper
	opener
}

// Record stores the run of control c over each of entities, and over any other entity with findings, and opens
// a break for each finding that has none open: a difference that lasts counts in every run but opens and is
// announced once. A finding of RailAccountsAtZero opens only if the entity's previous run found the same subject:
// a posting that read the closed date can still commit after the rollover (QS-2), so a cut can move for a moment.
func Record(ctx context.Context, r Recorder, c Control, entities []ids.ID, findings []Finding) error {
	subjects := make(map[ids.ID][]string, len(entities))
	for _, entity := range entities {
		subjects[entity] = nil
	}
	for _, f := range findings {
		subjects[f.LegalEntityID] = append(subjects[f.LegalEntityID], f.Subject)
	}
	previous, err := recordRuns(ctx, r, c, subjects)
	if err != nil {
		return err
	}
	for _, f := range findings {
		if c == RailAccountsAtZero && !slices.Contains(previous[f.LegalEntityID], f.Subject) {
			continue
		}
		if err := open(ctx, r, Break{ID: ids.New(), Control: c, Finding: f, Owner: controlsOwner}); err != nil {
			return err
		}
	}
	return nil
}

// recordRuns stores a run of c for each entity with the subjects of its findings. For RailAccountsAtZero it keeps
// them, and returns the ones of the previous run of each entity with findings.
func recordRuns(ctx context.Context, r runKeeper, c Control, subjects map[ids.ID][]string) (map[ids.ID][]string, error) {
	previous := make(map[ids.ID][]string)
	for entity, found := range subjects {
		run := Run{ID: ids.New(), LegalEntityID: entity, Control: c, Findings: len(found)}
		if c == RailAccountsAtZero && len(found) > 0 {
			last, err := r.LastSubjects(ctx, entity, c)
			if err != nil {
				return nil, err
			}
			previous[entity], run.Subjects = last, found
		}
		if err := r.RecordRun(ctx, run); err != nil {
			return nil, err
		}
	}
	return previous, nil
}
