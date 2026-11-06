//go:build integration

package reconciliation_test

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/DrewGGM/aluna/internal/calendar"
	"github.com/DrewGGM/aluna/internal/ids"
	"github.com/DrewGGM/aluna/internal/ledger"
	"github.com/DrewGGM/aluna/internal/money"
	"github.com/DrewGGM/aluna/internal/outbox"
	"github.com/DrewGGM/aluna/internal/postgres"
	"github.com/DrewGGM/aluna/internal/postgres/pgtest"
	"github.com/DrewGGM/aluna/internal/reconciliation"
	pg "github.com/DrewGGM/aluna/internal/reconciliation/internal/adapters/postgres"
	"github.com/DrewGGM/aluna/internal/reconciliation/internal/app"
)

const (
	trialBalance    = "trial_balance"
	balances        = "balances_match_entries"
	railAccounts    = "rail_accounts_at_zero"
	customers       = "customers_match_control"
	typeBreakOpened = "aluna.reconciliation.break.opened.v1"
)

var testPool *pgxpool.Pool

func TestMain(m *testing.M) { os.Exit(pgtest.Run(m, &testPool)) }

// recorder keeps the emitted events instead of publishing them.
type recorder struct{ events []outbox.Event }

func (r *recorder) Emit(_ context.Context, _ postgres.Tx, e outbox.Event) error {
	r.events = append(r.events, e)
	return nil
}

// world is an entity with a ledger, on business date 2026-03-02, and reconciliation over it.
type world struct {
	entity ids.ID
	cop    money.Currency
	ledger *ledger.Service
	recon  *reconciliation.Service
	events *recorder
}

func newWorld(t *testing.T) world {
	t.Helper()
	w := world{entity: ids.New(), events: &recorder{}}
	pgtest.AsOwner(t, testPool, func(tx pgx.Tx) error {
		_, err := tx.Exec(t.Context(), `INSERT INTO calendar.legal_entities (id, name, business_date, functional_currency)
			VALUES ($1, 'Entity', '2026-03-02', 'COP')`, w.entity)
		return err
	})
	cop, err := money.CurrencyOf("COP")
	if err != nil {
		t.Fatal(err)
	}
	outboxOf, err := outbox.New(testPool)
	if err != nil {
		t.Fatal(err)
	}
	w.cop, w.ledger, w.recon = cop, ledger.New(calendar.New(), outboxOf), reconciliation.New(w.events)
	return w
}

func inTx(t *testing.T, fn func(tx postgres.Tx) error) {
	t.Helper()
	if err := postgres.NewUnitOfWork(testPool).Do(t.Context(), fn); err != nil {
		t.Fatal(err)
	}
}

// account creates an account with code code.
func (w world) account(t *testing.T, code string, kind ledger.Kind, mode ledger.BalanceMode) ids.ID {
	t.Helper()
	var a ledger.Account
	inTx(t, func(tx postgres.Tx) error {
		var err error
		a, err = w.ledger.CreateAccount(t.Context(), tx, ledger.CreateAccountInput{
			LegalEntityID: w.entity, Code: code, Kind: kind, Currency: w.cop, Mode: mode, LowerLimit: -1_000_000_000,
		})
		return err
	})
	return a.ID
}

// post moves units from credit to debit on the entity's business date.
func (w world) post(t *testing.T, debit, credit ids.ID, units int64) {
	t.Helper()
	var date time.Time
	if err := testPool.QueryRow(t.Context(), `SELECT business_date FROM calendar.legal_entities WHERE id = $1`, w.entity).Scan(&date); err != nil {
		t.Fatal(err)
	}
	w.postOn(t, date, debit, credit, units)
}

// postOn is post on date.
func (w world) postOn(t *testing.T, date time.Time, debit, credit ids.ID, units int64) {
	t.Helper()
	inTx(t, func(tx postgres.Tx) error {
		_, err := w.ledger.Post(t.Context(), tx, ledger.PostInput{
			LegalEntityID: w.entity, Code: "test", ValueDate: date, BusinessDate: date, Source: "test", ExternalRef: ids.New().String(),
			Actor: "tester", Channel: "test", RuleVersion: "v1",
			Entries: []ledger.EntryInput{{AccountID: debit, Amount: money.New(units, w.cop)}, {AccountID: credit, Amount: money.New(-units, w.cop)}},
		})
		return err
	})
}

func (w world) run(t *testing.T) {
	t.Helper()
	if err := w.recon.RunControls(t.Context(), postgres.NewUnitOfWork(testPool)); err != nil {
		t.Fatal(err)
	}
}

// rollover moves the entity's business date to 2026-03-03, closing 2026-03-02.
func (w world) rollover(t *testing.T) {
	t.Helper()
	pgtest.AsOwner(t, testPool, func(tx pgx.Tx) error {
		_, err := tx.Exec(t.Context(), `UPDATE calendar.legal_entities SET business_date = '2026-03-03' WHERE id = $1`, w.entity)
		return err
	})
}

// openBreak is a break as the database has it.
type openBreak struct {
	control, subject, currency string
	difference                 int64
}

func (w world) breaks(t *testing.T) []openBreak {
	t.Helper()
	rows, err := testPool.Query(t.Context(), `SELECT control, subject, currency, difference_units FROM recon.breaks
		WHERE legal_entity_id = $1 AND status = 'open' ORDER BY control, subject`, w.entity)
	if err != nil {
		t.Fatal(err)
	}
	found, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (openBreak, error) {
		var b openBreak
		err := row.Scan(&b.control, &b.subject, &b.currency, &b.difference)
		return b, err
	})
	if err != nil {
		t.Fatal(err)
	}
	return found
}

func (w world) expectBreaks(t *testing.T, want ...openBreak) {
	t.Helper()
	slices.SortFunc(want, func(x, y openBreak) int {
		return cmp.Or(strings.Compare(x.control, y.control), strings.Compare(x.subject, y.subject))
	})
	if got := w.breaks(t); !slices.Equal(got, want) {
		t.Fatalf("breaks = %+v, want %+v", got, want)
	}
}

func day(t *testing.T, s string) time.Time {
	t.Helper()
	d, err := time.Parse(time.DateOnly, s)
	if err != nil {
		t.Fatal(err)
	}
	return d
}

// corrupt runs a statement as the owner, as a superuser would by hand.
func corrupt(t *testing.T, statement string, args ...any) {
	t.Helper()
	pgtest.AsOwner(t, testPool, func(tx pgx.Tx) error {
		_, err := tx.Exec(t.Context(), statement, args...)
		return err
	})
}

// TestASquareLedgerPasses (FR-33): each control records a run with no findings, and nothing opens.
func TestASquareLedgerPasses(t *testing.T) {
	w := newWorld(t)
	vault, customer := w.account(t, "A.CASH.VAULT", ledger.KindAsset, ledger.BalanceModeSync), w.account(t, "deposits/"+ids.New().String(), ledger.KindLiability, ledger.BalanceModeSync)
	w.post(t, vault, customer, 100_00)

	w.run(t)

	w.expectBreaks(t)
	rows, err := testPool.Query(t.Context(), `SELECT control FROM recon.control_runs WHERE legal_entity_id = $1 AND findings = 0 ORDER BY control`, w.entity)
	if err != nil {
		t.Fatal(err)
	}
	got, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{balances, customers, railAccounts, trialBalance}; !slices.Equal(got, want) {
		t.Fatalf("clean runs = %v, want %v", got, want)
	}
}

// expectTwoAnnounced checks that two breaks of the world's entity were announced, the first of the balances' control.
func (w world) expectTwoAnnounced(t *testing.T) {
	t.Helper()
	var opened []outbox.Event
	for _, e := range w.events.events {
		if e.LegalEntityID == w.entity {
			opened = append(opened, e)
		}
	}
	if len(opened) != 2 {
		t.Fatalf("%d events, want 2: %+v", len(opened), opened)
	}
	data, err := json.Marshal(opened[0].Data)
	if err != nil {
		t.Fatal(err)
	}
	if e := opened[0]; e.Type != typeBreakOpened || !strings.HasPrefix(e.Subject, "reconciliation-breaks/") || e.SchemaVersion != 1 ||
		!strings.Contains(string(data), `"control":"balances_match_entries"`) || !strings.Contains(string(data), `"difference":{"amount":-7,"currency":"COP"}`) {
		t.Errorf("event = %+v with %s", e, data)
	}
}

// TestADifferenceInjectedInTheBalancesOpensABreak (FR-33, US-9): a superuser changes a materialized balance;
// the control sees it against the entries, the customers' control against their journal, the break is stored
// and the event goes out, once however many runs find it.
func TestADifferenceInjectedInTheBalancesOpensABreak(t *testing.T) {
	w := newWorld(t)
	vault, customer := w.account(t, "A.CASH.VAULT", ledger.KindAsset, ledger.BalanceModeSync), w.account(t, "deposits/"+ids.New().String(), ledger.KindLiability, ledger.BalanceModeSync)
	w.post(t, vault, customer, 100_00)
	corrupt(t, `UPDATE ledger.balances SET posted = posted - 7 WHERE account_id = $1`, customer)

	w.run(t)
	w.run(t)

	w.expectBreaks(t,
		openBreak{balances, customer.String(), "COP", -7},
		openBreak{customers, "COP", "COP", -7},
	)
	w.expectTwoAnnounced(t)
	var withFindings int
	if err := testPool.QueryRow(t.Context(), `SELECT count(*) FROM recon.control_runs WHERE legal_entity_id = $1 AND control = $2 AND findings = 1`,
		w.entity, balances).Scan(&withFindings); err != nil {
		t.Fatal(err)
	}
	if withFindings != 2 {
		t.Errorf("%d runs with the finding, want 2: every run records it", withFindings)
	}
}

// TestATrialBalanceThatDoesNotSumZeroOpensABreak (FR-33): the journal's own invariant, broken past the database's
// checks by a superuser.
func TestATrialBalanceThatDoesNotSumZeroOpensABreak(t *testing.T) {
	w := newWorld(t)
	vault := w.account(t, "A.CASH.VAULT", ledger.KindAsset, ledger.BalanceModeSync)
	other := w.account(t, "L.OTHER", ledger.KindLiability, ledger.BalanceModeSync)
	w.post(t, vault, other, 100_00)
	pgtest.AsOwner(t, testPool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(t.Context(), `SET LOCAL session_replication_role = replica`); err != nil {
			return err
		}
		_, err := tx.Exec(t.Context(), `UPDATE ledger.entries SET amount = amount + 5 WHERE account_id = $1`, vault)
		return err
	})

	w.run(t)

	w.expectBreaks(t,
		openBreak{balances, vault.String(), "COP", -5},
		openBreak{trialBalance, "COP", "COP", 5},
	)
}

// TestAnAsyncAccountIsComparedUpToItsCursor: entries the aggregator has not reached are no difference, and after
// it does the balance still matches. A difference in what it did reach is one.
func TestAnAsyncAccountIsComparedUpToItsCursor(t *testing.T) {
	w := newWorld(t)
	vault := w.account(t, "A.CASH.VAULT", ledger.KindAsset, ledger.BalanceModeSync)
	fees := w.account(t, "I.FEES", ledger.KindIncome, ledger.BalanceModeAsync)
	w.post(t, vault, fees, 100_00)

	w.run(t)
	w.expectBreaks(t)

	uow := postgres.NewUnitOfWork(testPool)
	waitAggregated(t, w, uow, fees, -100_00)
	w.run(t)
	w.expectBreaks(t)

	corrupt(t, `UPDATE ledger.balances SET posted = posted + 3 WHERE account_id = $1`, fees)
	w.run(t)
	w.expectBreaks(t, openBreak{balances, fees.String(), "COP", 3})
}

// waitAggregated aggregates until the balance of account reaches want: the aggregator skips database
// transactions newer than the oldest one in progress, which may be another test's.
func waitAggregated(t *testing.T, w world, uow *postgres.UnitOfWork, account ids.ID, want int64) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		if _, err := w.ledger.AggregateAsyncBalances(t.Context(), uow, 1000); err != nil {
			t.Fatal(err)
		}
		b, err := w.ledger.Balance(t.Context(), testPool, account)
		if err != nil {
			t.Fatal(err)
		}
		if b.Posted == want {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("balance %d, want %d", b.Posted, want)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// TestARailAccountHasToBeAtZeroAtTheCut (FR-33): what is in a clearing or a suspense account during the day is
// work in progress; what stays from a closed business date opens a break on the second run that finds it. An
// account that came back to zero, or a movement of the open day, is not a difference.
func TestARailAccountHasToBeAtZeroAtTheCut(t *testing.T) {
	w := newWorld(t)
	vault := w.account(t, "A.CASH.VAULT", ledger.KindAsset, ledger.BalanceModeSync)
	clearing := w.account(t, "L.CLEARING.SIMBREB", ledger.KindLiability, ledger.BalanceModeAsync)
	suspense := w.account(t, "L.SUSPENSE.SIMBREB", ledger.KindLiability, ledger.BalanceModeAsync)
	settled := w.account(t, "L.CLEARING.OTHER", ledger.KindLiability, ledger.BalanceModeAsync)
	w.post(t, vault, clearing, 100_00)
	w.post(t, vault, suspense, 40_00)
	w.post(t, vault, settled, 10_00)
	w.post(t, settled, vault, 10_00)

	w.run(t)
	w.run(t)
	w.expectBreaks(t)

	w.rollover(t)
	w.post(t, clearing, vault, 100_00) // the next day's: it does not move yesterday's cut
	w.run(t)
	w.expectBreaks(t)

	w.run(t)
	w.expectBreaks(t, openBreak{railAccounts, clearing.String(), "COP", -100_00}, openBreak{railAccounts, suspense.String(), "COP", -40_00})
}

// TestACutThatSquaresBeforeTheSecondRunOpensNothing (QS-2): a posting that read the closed date commits after the
// rollover; the run in between sees a difference that is gone by the next.
func TestACutThatSquaresBeforeTheSecondRunOpensNothing(t *testing.T) {
	w := newWorld(t)
	vault := w.account(t, "A.CASH.VAULT", ledger.KindAsset, ledger.BalanceModeSync)
	clearing := w.account(t, "L.CLEARING.SIMBREB", ledger.KindLiability, ledger.BalanceModeAsync)
	w.post(t, vault, clearing, 100_00)

	w.rollover(t)
	w.run(t)
	w.postOn(t, day(t, "2026-03-02"), clearing, vault, 100_00)
	w.run(t)
	w.run(t)

	w.expectBreaks(t)
}

// TestAMissingBalanceRowIsADifference: the balance of an account whose row was deleted is not skipped, for a sync
// account of a customer or for an async one.
func TestAMissingBalanceRowIsADifference(t *testing.T) {
	w := newWorld(t)
	vault := w.account(t, "A.CASH.VAULT", ledger.KindAsset, ledger.BalanceModeSync)
	customer := w.account(t, "deposits/"+ids.New().String(), ledger.KindLiability, ledger.BalanceModeSync)
	fees := w.account(t, "I.FEES", ledger.KindIncome, ledger.BalanceModeAsync)
	w.post(t, vault, customer, 100_00)
	w.post(t, vault, fees, 30_00)
	waitAggregated(t, w, postgres.NewUnitOfWork(testPool), fees, -30_00)
	corrupt(t, `DELETE FROM ledger.balances WHERE account_id = ANY($1)`, []ids.ID{customer, fees})

	w.run(t)

	w.expectBreaks(t,
		openBreak{balances, customer.String(), "COP", 100_00},
		openBreak{balances, fees.String(), "COP", 30_00},
		openBreak{customers, "COP", "COP", 100_00},
	)
}

// TestABreakGoesOutThroughTheOutbox: the event is one the real outbox stores.
func TestABreakGoesOutThroughTheOutbox(t *testing.T) {
	w := newWorld(t)
	vault, other := w.account(t, "A.CASH.VAULT", ledger.KindAsset, ledger.BalanceModeSync), w.account(t, "L.OTHER", ledger.KindLiability, ledger.BalanceModeSync)
	w.post(t, vault, other, 1_00)
	corrupt(t, `UPDATE ledger.balances SET posted = posted + 1 WHERE account_id = $1`, other)
	events, err := outbox.New(testPool)
	if err != nil {
		t.Fatal(err)
	}

	if err := reconciliation.New(events).RunControls(t.Context(), postgres.NewUnitOfWork(testPool)); err != nil {
		t.Fatal(err)
	}

	var stored int
	if err := testPool.QueryRow(t.Context(), `SELECT count(*) FROM infra.outbox WHERE legal_entity_id = $1 AND type = $2`,
		w.entity, typeBreakOpened).Scan(&stored); err != nil {
		t.Fatal(err)
	}
	if stored != 1 {
		t.Fatalf("%d events stored, want 1", stored)
	}
}

// TestOnlyOwnAccountsCount: another entity's difference opens a break of that entity.
func TestOnlyOwnAccountsCount(t *testing.T) {
	w, other := newWorld(t), newWorld(t)
	vault := other.account(t, "A.CASH.VAULT", ledger.KindAsset, ledger.BalanceModeSync)
	liability := other.account(t, "L.OTHER", ledger.KindLiability, ledger.BalanceModeSync)
	other.post(t, vault, liability, 1_00)
	corrupt(t, `UPDATE ledger.balances SET posted = posted + 1 WHERE account_id = $1`, liability)

	w.run(t)

	w.expectBreaks(t)
	other.expectBreaks(t, openBreak{balances, liability.String(), "COP", 1})
}

// TestARunAndABreakStay: as the application's role, and even as the owner, a run does not change or disappear, and
// neither does a break.
func TestARunAndABreakStay(t *testing.T) {
	w := newWorld(t)
	vault, other := w.account(t, "A.CASH.VAULT", ledger.KindAsset, ledger.BalanceModeSync), w.account(t, "L.OTHER", ledger.KindLiability, ledger.BalanceModeSync)
	w.post(t, vault, other, 1_00)
	corrupt(t, `UPDATE ledger.balances SET posted = posted + 1 WHERE account_id = $1`, other)
	w.run(t)

	for _, statement := range []string{
		`UPDATE recon.control_runs SET findings = 9 WHERE legal_entity_id = $1`,
		`DELETE FROM recon.control_runs WHERE legal_entity_id = $1`,
		`DELETE FROM recon.breaks WHERE legal_entity_id = $1`,
	} {
		if _, err := testPool.Exec(t.Context(), statement, w.entity); err == nil || !strings.Contains(err.Error(), "permission denied") {
			t.Errorf("%s: %v, want permission denied", statement, err)
		}
		expectForbidden(t, statement, w.entity)
	}
	expectForbidden(t, `TRUNCATE recon.control_runs`)
	expectForbidden(t, `TRUNCATE recon.breaks`)
}

// expectForbidden runs statement as the owner, which the database refuses with 2F003.
func expectForbidden(t *testing.T, statement string, args ...any) {
	t.Helper()
	pgtest.AsOwner(t, testPool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(t.Context(), `SAVEPOINT guarded`); err != nil {
			return err
		}
		_, err := tx.Exec(t.Context(), statement, args...)
		var pgErr *pgconn.PgError
		if !errors.As(err, &pgErr) || pgErr.Code != "2F003" {
			t.Errorf("%s as the owner: %v, want 2F003", statement, err)
		}
		_, err = tx.Exec(t.Context(), `ROLLBACK TO SAVEPOINT guarded`)
		return err
	})
}

// TestAControlThatFailsStopsNoOther: the run says it failed, with no result of that control, and the controls
// after it still run.
func TestAControlThatFailsStopsNoOther(t *testing.T) {
	w := newWorld(t)
	corrupt(t, `REVOKE SELECT ON ledger.async_cursor FROM aluna_app`) // only balances_match_entries reads it
	defer corrupt(t, `GRANT SELECT ON ledger.async_cursor TO aluna_app`)

	err := w.recon.RunControls(t.Context(), postgres.NewUnitOfWork(testPool))

	if err == nil || !strings.Contains(err.Error(), "permission denied") {
		t.Fatalf("RunControls = %v, want the failure of the control", err)
	}
	rows, err := testPool.Query(t.Context(), `SELECT control FROM recon.control_runs WHERE legal_entity_id = $1 ORDER BY control`, w.entity)
	if err != nil {
		t.Fatal(err)
	}
	got, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{customers, railAccounts, trialBalance}; !slices.Equal(got, want) {
		t.Fatalf("runs = %v, want %v", got, want)
	}
}

// TestARunWithoutItsEntitiesFails: without knowing the entities no control runs, so none leaves a run that would
// say a clean entity was looked at.
func TestARunWithoutItsEntitiesFails(t *testing.T) {
	w := newWorld(t)
	corrupt(t, `REVOKE SELECT ON calendar.legal_entities FROM aluna_app`)
	defer corrupt(t, `GRANT SELECT ON calendar.legal_entities TO aluna_app`)

	err := w.recon.RunControls(t.Context(), postgres.NewUnitOfWork(testPool))

	if err == nil || !strings.Contains(err.Error(), "read the legal entities") {
		t.Fatalf("RunControls = %v, want the failure to read the entities", err)
	}
	var runs int
	if err := testPool.QueryRow(t.Context(), `SELECT count(*) FROM recon.control_runs WHERE legal_entity_id = $1`, w.entity).Scan(&runs); err != nil {
		t.Fatal(err)
	}
	if runs != 0 {
		t.Fatalf("%d runs, want none", runs)
	}
}

// TestAControlHasItsOwnTimeLimit: the pool's 5 s are for requests, and a control reads the whole journal.
func TestAControlHasItsOwnTimeLimit(t *testing.T) {
	inTx(t, func(tx postgres.Tx) error {
		if _, err := pg.NewStore(tx).Findings(t.Context(), app.TrialBalance); err != nil {
			return err
		}
		var limit string
		if err := tx.QueryRow(t.Context(), `SHOW statement_timeout`).Scan(&limit); err != nil {
			return err
		}
		if limit != "5min" {
			t.Errorf("statement_timeout = %s, want 5min", limit)
		}
		return nil
	})
}

// TestAControlWithoutQueryFails: it does not pass as a clean run of a control that looked at nothing.
func TestAControlWithoutQueryFails(t *testing.T) {
	inTx(t, func(tx postgres.Tx) error {
		if _, err := pg.NewStore(tx).Findings(t.Context(), "unknown"); err == nil {
			t.Error("no error for a control without a query")
		}
		return nil
	})
}

// TestEveryControlHasAQuery: Findings accepts each control of a run.
func TestEveryControlHasAQuery(t *testing.T) {
	inTx(t, func(tx postgres.Tx) error {
		for _, c := range app.Controls {
			if _, err := pg.NewStore(tx).Findings(t.Context(), c); err != nil {
				return err
			}
		}
		return nil
	})
}
