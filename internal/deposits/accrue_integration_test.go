//go:build integration

package deposits_test

import (
	"context"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/DrewGGM/aluna/internal/calendar"
	"github.com/DrewGGM/aluna/internal/deposits"
	pg "github.com/DrewGGM/aluna/internal/deposits/internal/adapters/postgres"
	"github.com/DrewGGM/aluna/internal/deposits/internal/app"
	"github.com/DrewGGM/aluna/internal/deposits/internal/domain/interest"
	"github.com/DrewGGM/aluna/internal/ids"
	"github.com/DrewGGM/aluna/internal/ledger"
	"github.com/DrewGGM/aluna/internal/money"
	"github.com/DrewGGM/aluna/internal/party"
	"github.com/DrewGGM/aluna/internal/postgres"
	"github.com/DrewGGM/aluna/internal/postgres/pgtest"
)

// accrualBatch is the batch of the tests' runs: small, so that a run takes several.
const accrualBatch = 3

// accrual is an account's row of a value date.
type accrual struct {
	ValueDate                 calendar.Date
	BalanceUnits, PostedUnits int64
	Interest, AccruedPrecise  string
}

// storedAccruals are account id's rows, by value date.
func storedAccruals(t *testing.T, id ids.ID) []accrual {
	t.Helper()
	rows, err := testPool.Query(t.Context(), `SELECT value_date, balance_units, interest::text, accrued_precise::text, posted_units
		FROM deposits.accruals WHERE account_id = $1 ORDER BY value_date`, id)
	if err != nil {
		t.Fatal(err)
	}
	got, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (accrual, error) {
		var a accrual
		var on time.Time
		err := row.Scan(&on, &a.BalanceUnits, &a.Interest, &a.AccruedPrecise, &a.PostedUnits)
		a.ValueDate = calendar.Date{Year: on.Year(), Month: on.Month(), Day: on.Day()}
		return a, err
	})
	if err != nil {
		t.Fatal(err)
	}
	return got
}

// accrue runs the accrual of every entity in batches of accrualBatch and returns how many account-days it
// accrued.
func (w world) accrue(t *testing.T) int {
	t.Helper()
	return w.accrueIn(t, accrualBatch)
}

// accrueIn is accrue in batches of batch.
func (w world) accrueIn(t *testing.T, batch int) int {
	t.Helper()
	n, err := w.deposits.Accrue(t.Context(), postgres.NewUnitOfWork(testPool), batch)
	if err != nil {
		t.Fatal(err)
	}
	return n
}

// dayBalance is the closing balance of a day.
type dayBalance struct {
	day     calendar.Date
	balance int64
}

// expected accrues each day, in order and from an empty state, with the product of account a: the rows the
// accrual must store.
func (w world) expected(t *testing.T, a deposits.Account, days ...dayBalance) []accrual {
	t.Helper()
	v, err := w.products.Version(t.Context(), testPool, a.ProductVersionID)
	if err != nil {
		t.Fatal(err)
	}
	var state interest.AccrualState
	want := make([]accrual, 0, len(days))
	for _, d := range days {
		var earned money.Precise
		if state, earned, err = interest.AccrueDay(state, d.balance, v.Definition.RateEA, v.Definition.DayCount, d.day); err != nil {
			t.Fatal(err)
		}
		want = append(want, accrual{ValueDate: d.day, BalanceUnits: d.balance, Interest: earned.String(), AccruedPrecise: state.AccruedPrecise.String()})
	}
	return want
}

func march(day int) calendar.Date { return calendar.Date{Year: 2026, Month: time.March, Day: day} }

// uncapitalized opens an active account of the savings product with an unadjusted capitalization date: its
// February ends on Saturday the 28th, so no day of March up to the 30th capitalizes (with the product's
// following convention, Monday March 2 would capitalize February).
func (w world) uncapitalized(t *testing.T) deposits.Account {
	t.Helper()
	return w.openWith(t, map[string]string{"date_convention": `"unadjusted"`})
}

// TestEachClosedDayAccruesOnItsClosingBalance (FR-25): after two rollovers, the account accrues the two closed
// days, each on the posted balance at the close of its value date (a deposit of the second day does not
// count for the first), with the domain's interest and the state carried from the previous day.
func TestEachClosedDayAccruesOnItsClosingBalance(t *testing.T) {
	w := newWorld(t)
	w.vault(t)
	a := w.uncapitalized(t)
	steps(t,
		func() error { return w.deposit(t, a.ID, 10000000) },
		func() error { w.rollover(t, march(2)); return w.deposit(t, a.ID, 5000000) },
	)
	w.rollover(t, march(3))

	w.accrue(t)

	want := w.expected(t, a, dayBalance{march(2), 10000000}, dayBalance{march(3), 15000000})
	if got := storedAccruals(t, a.ID); !slices.Equal(got, want) {
		t.Fatalf("accruals = %+v, want %+v", got, want)
	}
}

// TestAccruingTwiceAddsNothing (FR-25): a second run for the same closed dates creates no rows and changes
// none.
func TestAccruingTwiceAddsNothing(t *testing.T) {
	w := newWorld(t)
	w.vault(t)
	a := w.active(t)
	if err := w.deposit(t, a.ID, 10000000); err != nil {
		t.Fatal(err)
	}
	w.rollover(t, march(2))
	w.accrue(t)
	first := storedAccruals(t, a.ID)

	again := w.accrue(t)

	if again != 0 {
		t.Fatalf("the second run accrued %d account-days, want 0", again)
	}
	if got := storedAccruals(t, a.ID); len(first) != 1 || !slices.Equal(got, first) {
		t.Fatalf("rows after the second run = %+v, want the first run's single row %+v", got, first)
	}
}

// TestAWeekendAccruesEveryCalendarDay (FR-25): the rollover from Friday to Monday closes Friday, Saturday and
// Sunday, and each one accrues, in order; nothing accrues for a day that is not closed yet.
func TestAWeekendAccruesEveryCalendarDay(t *testing.T) {
	w := newWorldOn(t, "2026-03-06") // a Friday
	w.vault(t)
	a := w.active(t)
	if err := w.deposit(t, a.ID, 1000000); err != nil {
		t.Fatal(err)
	}
	w.rollover(t, march(6))

	w.accrue(t)

	want := w.expected(t, a, dayBalance{march(6), 1000000}, dayBalance{march(7), 1000000}, dayBalance{march(8), 1000000})
	if got := storedAccruals(t, a.ID); !slices.Equal(got, want) {
		t.Fatalf("accruals = %+v, want Friday to Sunday %+v", got, want)
	}
}

// TestANegativeClosingBalanceEarnsNothing: a posting on an earlier value date (a reversal does that) can leave
// a day not yet accrued with a negative closing balance; the day accrues with nothing earned instead of
// stopping the accrual.
func TestANegativeClosingBalanceEarnsNothing(t *testing.T) {
	w := newWorld(t)
	vault := w.vault(t)
	a := w.active(t)
	w.rollover(t, march(2))
	if err := w.deposit(t, a.ID, 1000); err != nil {
		t.Fatal(err)
	}
	cop, monday := copOf(t), time.Date(2026, 3, 2, 0, 0, 0, 0, time.UTC)
	inTx(t, func(tx postgres.Tx) error { // 400 out of the account, dated Monday, when it held nothing
		_, err := w.ledger.Post(t.Context(), tx, ledger.PostInput{
			LegalEntityID: w.entity, Code: "back-dated", ValueDate: monday, BusinessDate: monday.AddDate(0, 0, 1),
			Source: "test", ExternalRef: ids.New().String(), Actor: "tester", Channel: "test", RuleVersion: "v1",
			Entries: []ledger.EntryInput{{AccountID: a.LedgerAccountID, Amount: money.New(400, cop)}, {AccountID: vault, Amount: money.New(-400, cop)}},
		})
		return err
	})
	w.rollover(t, march(3))

	w.accrue(t)

	want := w.expected(t, a, dayBalance{march(2), -400}, dayBalance{march(3), 600})
	if got := storedAccruals(t, a.ID); !slices.Equal(got, want) || want[0].Interest != "0" {
		t.Fatalf("accruals = %+v, want Monday at -400 earning nothing %+v", got, want)
	}
}

// TestAClosedAccountStopsAccruing: an account accrues while open and nothing for the days after it closed,
// while another account of its entity goes on.
func TestAClosedAccountStopsAccruing(t *testing.T) {
	w := newWorld(t)
	w.vault(t)
	closing, open := w.active(t), w.active(t)
	w.rollover(t, march(2))
	w.accrue(t)
	if err := w.change(t, closing.ID, deposits.Closed); err != nil {
		t.Fatal(err)
	}
	w.rollover(t, march(3))

	w.accrue(t)

	if closed, other := len(storedAccruals(t, closing.ID)), len(storedAccruals(t, open.ID)); closed != 1 || other != 2 {
		t.Fatalf("the closed account accrued %d days and the open one %d, want 1 (before closing) and 2", closed, other)
	}
}

// TestADayWaitsForTheDayBefore: an account is due to accrue a day only on its opening day or once it accrued
// the day before, because each day starts from the previous one's state: a day it missed (locked, or a run
// that stopped) is accrued before the next. It reads the store directly: from the facade, a run cannot be made
// to skip a day and then find the account free for the next one.
func TestADayWaitsForTheDayBefore(t *testing.T) {
	w := newWorld(t)
	w.vault(t)
	a := w.active(t)
	due := func(day calendar.Date) []app.DueAccount {
		var got []app.DueAccount
		inTx(t, func(tx postgres.Tx) error {
			var err error
			got, err = pg.NewStore(tx).Due(t.Context(), w.entity, day, ids.ID{}, accrualBatch)
			return err
		})
		return got
	}

	second, first := due(march(3)), due(march(2))

	if len(second) != 0 || len(first) != 1 || first[0].ID != a.ID {
		t.Fatalf("due on March 3 = %+v, on March 2 = %+v; want none, then the account opened on March 2", second, first)
	}
}

// TestALockedAccountIsAccruedLater: an account locked by a movement in progress is skipped (SKIP LOCKED),
// without waiting for it, and so are its later days; the next run accrues them.
func TestALockedAccountIsAccruedLater(t *testing.T) {
	w := newWorld(t)
	w.vault(t)
	busy, free := w.active(t), w.active(t)
	w.rollover(t, march(2))
	w.rollover(t, march(3))
	_, finish := holdOpen(t, func(tx postgres.Tx) error {
		_, err := tx.Exec(t.Context(), `SELECT 1 FROM deposits.accounts WHERE id = $1 FOR UPDATE`, busy.ID)
		return err
	})

	w.accrue(t)
	whileBusy, freeRows := len(storedAccruals(t, busy.ID)), len(storedAccruals(t, free.ID))
	if err := finish(); err != nil {
		t.Fatal(err)
	}
	w.accrue(t)

	if whileBusy != 0 || freeRows != 2 || len(storedAccruals(t, busy.ID)) != 2 {
		t.Fatalf("while locked: %d rows (free account %d); after: %d; want 0 (2), then 2", whileBusy, freeRows, len(storedAccruals(t, busy.ID)))
	}
}

// insertAccounts inserts n active accounts of the world's entity directly, for party partyID with product
// version versionID, opened on 2026-03-02, and returns their ledger accounts: opening them one by one is too
// slow for what the tests need.
func (w world) insertAccounts(t *testing.T, partyID, versionID ids.ID, n int) []ids.ID {
	t.Helper()
	var ledgerAccounts []ids.ID
	pgtest.AsOwner(t, testPool, func(tx pgx.Tx) error {
		rows, err := tx.Query(t.Context(), `
			WITH accounts AS (
				SELECT gen_random_uuid() AS id, gen_random_uuid() AS ledger_id FROM generate_series(1, $4::int)
			), ledger AS (
				INSERT INTO ledger.accounts (id, legal_entity_id, code, kind, currency, balance_mode, lower_limit)
				SELECT ledger_id, $1, 'deposits/' || id, 'liability', 'COP', 'sync', 0 FROM accounts RETURNING id
			), balances AS (
				INSERT INTO ledger.balances (account_id) SELECT id FROM ledger
			), deposit AS (
				INSERT INTO deposits.accounts (id, legal_entity_id, party_id, product_version_id, ledger_account_id, currency, status,
					opened_on, last_customer_activity_on)
				SELECT accounts.id, $1, $2, $3, accounts.ledger_id, 'COP', 'active', '2026-03-02', '2026-03-02' FROM accounts
			)
			SELECT ledger_id FROM accounts`, w.entity, partyID, versionID, n)
		if err != nil {
			return err
		}
		ledgerAccounts, err = pgx.CollectRows(rows, pgx.RowTo[ids.ID])
		return err
	})
	return ledgerAccounts
}

// fund credits units to each of the ledger accounts from vault, in postings of 500 accounts: short
// transactions, because in CI the server is shared with other packages and the ledger's aggregation waits for
// the oldest transaction in progress on it.
func (w world) fund(t *testing.T, vault ids.ID, accounts []ids.ID, units int64) {
	t.Helper()
	cop, today := copOf(t), time.Date(2026, 3, 2, 0, 0, 0, 0, time.UTC)
	for part := range slices.Chunk(accounts, 500) {
		entries := []ledger.EntryInput{{AccountID: vault, Amount: money.New(units*int64(len(part)), cop)}}
		for _, id := range part {
			entries = append(entries, ledger.EntryInput{AccountID: id, Amount: money.New(-units, cop)})
		}
		inTx(t, func(tx postgres.Tx) error {
			_, err := w.ledger.Post(t.Context(), tx, ledger.PostInput{
				LegalEntityID: w.entity, Code: "funding", ValueDate: today, BusinessDate: today, Source: "test", ExternalRef: ids.New().String(),
				Actor: "tester", Channel: "test", RuleVersion: "v1", Entries: entries,
			})
			return err
		})
	}
}

// TestAFailingEntityDoesNotStopTheOthers: an entity whose accrual fails (here, a product version whose stored
// definition no longer reads) comes back as an error that names it, and the other entities still accrue.
func TestAFailingEntityDoesNotStopTheOthers(t *testing.T) {
	broken, healthy := newWorld(t), newWorld(t)
	_, draft := broken.product(t, "valid-savings.json", false)
	pgtest.AsOwner(t, testPool, func(tx pgx.Tx) error {
		_, err := tx.Exec(t.Context(), `UPDATE products.versions SET definition = '{}' WHERE id = $1`, draft.ID)
		return err
	})
	t.Cleanup(func() { // the later tests of the package's database accrue every entity: close the broken one's accounts
		ctx := context.WithoutCancel(t.Context())
		err := pgx.BeginFunc(ctx, testPool, func(tx pgx.Tx) error {
			if _, err := tx.Exec(ctx, `SET LOCAL ROLE NONE`); err != nil {
				return err
			}
			_, err := tx.Exec(ctx, `UPDATE deposits.accounts SET status = 'closed' WHERE legal_entity_id = $1`, broken.entity)
			return err
		})
		if err != nil {
			t.Error(err)
		}
	})
	broken.insertAccounts(t, broken.party(t, party.KYCApproved), draft.ID, 1)
	healthy.vault(t)
	a := healthy.active(t)
	broken.rollover(t, march(2))
	healthy.rollover(t, march(2))

	_, err := healthy.deposits.Accrue(t.Context(), postgres.NewUnitOfWork(testPool), accrualBatch)

	if err == nil || !strings.Contains(err.Error(), broken.entity.String()) || len(storedAccruals(t, a.ID)) != 1 {
		t.Fatalf("accrue = %v, healthy rows %d; want an error naming entity %s and the healthy account accrued",
			err, len(storedAccruals(t, a.ID)), broken.entity)
	}
}

// TestTenThousandAccountsAccrueInAMinute (FR-25, TH-13): a closed day of 10,000 funded accounts accrues in less
// than 60 s, one row each with its interest, in the batches of the worker's job (50, accrualBatch in
// internal/jobs/jobs.go).
func TestTenThousandAccountsAccrueInAMinute(t *testing.T) {
	w := newWorld(t)
	vault := w.vault(t)
	sample := w.uncapitalized(t) // what is measured is the accrual, not a capitalization of every account
	const n = 10000
	accounts := w.insertAccounts(t, sample.PartyID, sample.ProductVersionID, n)
	if err := w.deposit(t, sample.ID, 1000000); err != nil {
		t.Fatal(err)
	}
	w.fund(t, vault, accounts, 1000000) // every account funded with 10,000 COP, so its interest is computed
	w.rollover(t, march(2))

	start := time.Now()
	w.accrueIn(t, 50) // the job's batch; it also accrues what other tests of the package's database left: count this entity's rows
	took := time.Since(start)

	var rows int
	if err := testPool.QueryRow(t.Context(), `SELECT count(*) FROM deposits.accruals r JOIN deposits.accounts a ON a.id = r.account_id
		WHERE a.legal_entity_id = $1 AND r.interest > 0`, w.entity).Scan(&rows); err != nil {
		t.Fatal(err)
	}
	if rows != n+1 || took >= time.Minute {
		t.Fatalf("%d rows with interest in %s, want %d in less than a minute", rows, took, n+1)
	}
	t.Logf("%d accounts accrued in %s", rows, took)
}

// TestTheDatabaseGuardsTheAccruals: even as the tables' owner, an accrual is never changed, deleted or
// truncated: a correction is another posting, not a rewritten day.
func TestTheDatabaseGuardsTheAccruals(t *testing.T) {
	w := newWorld(t)
	w.vault(t)
	a := w.active(t)
	w.rollover(t, march(2))
	w.accrue(t)
	where := ` WHERE account_id = '` + a.ID.String() + `'`
	expectProhibited(t, []prohibited{
		{"rewrite the interest", `UPDATE deposits.accruals SET interest = 1` + where, "never updated, deleted or truncated"},
		{"delete", `DELETE FROM deposits.accruals` + where, "never updated, deleted or truncated"},
		{"truncate", `TRUNCATE deposits.accruals`, "never updated, deleted or truncated"},
	})
}
