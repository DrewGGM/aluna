//go:build integration

package deposits_test

import (
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/DrewGGM/aluna/internal/calendar"
	"github.com/DrewGGM/aluna/internal/deposits"
	pg "github.com/DrewGGM/aluna/internal/deposits/internal/adapters/postgres"
	"github.com/DrewGGM/aluna/internal/deposits/internal/app"
	"github.com/DrewGGM/aluna/internal/ids"
	"github.com/DrewGGM/aluna/internal/ledger"
	"github.com/DrewGGM/aluna/internal/money"
	"github.com/DrewGGM/aluna/internal/postgres"
	"github.com/DrewGGM/aluna/internal/postgres/pgtest"
)

// statementOf is a statement's document as these tests read it.
type statementOf struct {
	AccountID      ids.ID `json:"account_id"`
	Period         string `json:"period"`
	Currency       string `json:"currency"`
	OpeningBalance int64  `json:"opening_balance"`
	Interest       int64  `json:"interest"`
	Fees           int64  `json:"fees"`
	GMF            int64  `json:"gmf"`
	ClosingBalance int64  `json:"closing_balance"`
	Movements      []struct {
		Kind   string `json:"kind"`
		Amount int64  `json:"amount"`
	} `json:"movements"`
}

// february is a world whose February moved: a pays a withdrawal and a transfer to b with their fees and GMF; b also
// got 700 on January 31, 100 on February 1, 9 of interest on February 27 and 50 on March 1; moved, of a product whose fee and GMF accounts do not
// exist, got and gave back 500 and was closed; idle was closed without moving. Then the business date went to March
// and February was closed.
type february struct {
	world
	a, b, moved, idle deposits.Account
}

func newFebruary(t *testing.T) february {
	t.Helper()
	f := february{world: newWorldOn(t, "2026-02-02")}
	cash := f.vault(t)
	f.a, f.b = f.openWith(t, withFees), f.openWith(t, withFees)
	f.deposited(t, f.a.ID, 100000000)
	f.withdrawn(t, f.cashIn(50000000, f.a.ID, copOf(t)))
	if err := f.transfer(t, f.a.ID, f.b.ID, 3000000); err != nil {
		t.Fatal(err)
	}
	f.post(t, "2026-01-31", cash, f.b.LedgerAccountID, 700)
	f.post(t, "2026-02-01", cash, f.b.LedgerAccountID, 100)
	f.post(t, "2026-02-27", f.accountOf(t, "E.INTEREST.SAVINGS").ID, f.b.LedgerAccountID, 9) // its interest, as a capitalization posts it
	f.moved, f.idle = f.openWith(t, withoutChargeAccounts), f.active(t)
	f.post(t, "2026-02-02", cash, f.moved.LedgerAccountID, 500)
	f.post(t, "2026-02-02", f.moved.LedgerAccountID, cash, 500)
	for _, id := range []ids.ID{f.moved.ID, f.idle.ID} {
		if err := f.change(t, id, deposits.Closed); err != nil {
			t.Fatal(err)
		}
	}
	pgtest.AsOwner(t, testPool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(t.Context(), `UPDATE calendar.legal_entities SET business_date = '2026-03-02' WHERE id = $1`, f.entity); err != nil {
			return err
		}
		_, err := tx.Exec(t.Context(), `INSERT INTO calendar.periods (legal_entity_id, period, status, closed_at, closed_by)
			VALUES ($1, '2026-02', 'closed', clock_timestamp(), 'finance-b')`, f.entity)
		return err
	})
	f.post(t, "2026-03-01", cash, f.b.LedgerAccountID, 50)
	return f
}

// withoutChargeAccounts are the fields of a product whose fee income and GMF payable accounts do not exist.
var withoutChargeAccounts = map[string]string{
	"accounts": `{"cash": "A.CASH.VAULT", "interest_expense": "E.INTEREST.SAVINGS", "fee_income": "I.FEES.NONE", "gmf_payable": "L.TAX.NONE"}`,
}

// post moves units from credit to debit with business date day, outside the deposit movements.
func (f february) post(t *testing.T, day string, debit, credit ids.ID, units int64) {
	t.Helper()
	on, err := time.Parse(time.DateOnly, day)
	if err != nil {
		t.Fatal(err)
	}
	inTx(t, func(tx postgres.Tx) error {
		_, err := f.ledger.Post(t.Context(), tx, ledger.PostInput{
			LegalEntityID: f.entity, Code: "test", ValueDate: on, BusinessDate: on, Source: "test", ExternalRef: ids.New().String(),
			Actor: "tester", Channel: "test", RuleVersion: "v1",
			Entries: []ledger.EntryInput{{AccountID: debit, Amount: money.New(units, copOf(t))}, {AccountID: credit, Amount: money.New(-units, copOf(t))}},
		})
		return err
	})
}

func (f february) generate(t *testing.T) {
	t.Helper()
	if _, err := f.deposits.GenerateStatements(t.Context(), postgres.NewUnitOfWork(testPool), 1); err != nil {
		t.Fatal(err)
	}
}

// statement is a's statement of February.
func (f february) statement(t *testing.T, a deposits.Account) (deposits.Statement, statementOf) {
	t.Helper()
	st, err := f.deposits.Statement(t.Context(), testPool, f.entity, a.ID, "2026-02")
	if err != nil {
		t.Fatalf("statement of %s: %v", a.ID, err)
	}
	var doc statementOf
	if err := json.Unmarshal(st.Document, &doc); err != nil {
		t.Fatal(err)
	}
	return st, doc
}

// summary is what these tests check of a statement.
type summary struct {
	opening, interest, fees, gmf, closing int64
	movements                             int
}

func (f february) statementDoc(t *testing.T, a deposits.Account) statementOf {
	t.Helper()
	_, doc := f.statement(t, a)
	return doc
}

func summaryOf(doc statementOf) summary {
	return summary{doc.OpeningBalance, doc.Interest, doc.Fees, doc.GMF, doc.ClosingBalance, len(doc.Movements)}
}

// TestAClosedMonthHasItsStatement (FR-45, US-16): the statement matches the history: its closing balance is the
// account's, it has every entry, and the fees and the GMF are those its account paid, not those of who sent it money.
func TestAClosedMonthHasItsStatement(t *testing.T) {
	f := newFebruary(t)

	f.generate(t)

	st, doc := f.statement(t, f.a)
	history, err := f.ledger.History(t.Context(), testPool, f.entity, f.a.LedgerAccountID, "", ledger.MaxPageSize)
	if err != nil {
		t.Fatal(err)
	}
	if doc.AccountID != f.a.ID || doc.Period != "2026-02" || doc.Currency != "COP" || st.GeneratedAt.IsZero() {
		t.Fatalf("statement of a = %s", st.Document)
	}
	for name, c := range map[string]struct {
		got, want summary
	}{
		"a":                              {summaryOf(doc), summary{0, 0, 250000 + 100000, 200000 + 12000, f.balance(t, f.a), len(history.Entries)}},
		"b, who received from a":         {summaryOf(f.statementDoc(t, f.b)), summary{700, 9, 0, 0, 3000809, 3}},
		"an account closed after moving": {summaryOf(f.statementDoc(t, f.moved)), summary{0, 0, 0, 0, 0, 2}},
	} {
		if c.got != c.want {
			t.Errorf("statement of %s = %+v, want %+v", name, c.got, c.want)
		}
	}
	if _, err := f.deposits.Statement(t.Context(), testPool, f.entity, f.idle.ID, "2026-02"); !errors.Is(err, deposits.ErrStatementNotFound) {
		t.Fatalf("statement of the account closed without moving = %v, want ErrStatementNotFound", err)
	}
}

// TestAStatementIsGeneratedAgainTheSame: built again from the ledger it has the same hash, which is the SHA-256 of
// the stored bytes (the database checks it), and the job does not generate it twice.
func TestAStatementIsGeneratedAgainTheSame(t *testing.T) {
	f := newFebruary(t)
	f.generate(t)
	stored, _ := f.statement(t, f.a)

	counters := app.CounterAccounts{
		InterestExpense: f.accountOf(t, "E.INTEREST.SAVINGS").ID, FeeIncome: f.accountOf(t, "I.FEES.SAVINGS").ID, GMFPayable: f.accountOf(t, "L.TAX.GMF").ID,
	}
	from, to := calendar.Date{Year: 2026, Month: time.February, Day: 1}, calendar.Date{Year: 2026, Month: time.March, Day: 1}
	month, err := pg.NewStore(testPool).Month(t.Context(), f.a, from, to)
	if err != nil {
		t.Fatal(err)
	}
	again, err := app.BuildStatement(f.a, "2026-02", month, counters)
	if err != nil {
		t.Fatal(err)
	}
	if again.ContentHash != stored.ContentHash || string(again.Document) != string(stored.Document) {
		t.Fatalf("generated again %s (%s), stored %s (%s)", again.Document, again.ContentHash, stored.Document, stored.ContentHash)
	}

	f.generate(t)
	if n := count(t, `SELECT count(*) FROM deposits.statements WHERE account_id = $1`, f.a.ID); n != 1 {
		t.Fatalf("%d statements of a", n)
	}
	if n := count(t, `SELECT count(*) FROM deposits.statement_periods WHERE legal_entity_id = $1 AND period = '2026-02'`, f.entity); n != 1 {
		t.Fatalf("February recorded complete %d times", n)
	}
}

// TestARunCutShortIsResumed: a statement already stored is kept, and the rest are generated.
func TestARunCutShortIsResumed(t *testing.T) {
	f := newFebruary(t)
	inTx(t, func(tx postgres.Tx) error {
		_, err := tx.Exec(t.Context(), `INSERT INTO deposits.statements (account_id, period, document, content_hash)
			VALUES ($1, '2026-02', '{}', encode(sha256('{}'), 'hex'))`, f.a.ID)
		return err
	})

	f.generate(t)

	if stored, _ := f.statement(t, f.a); string(stored.Document) != "{}" {
		t.Fatalf("statement of a = %s, want the one stored before", stored.Document)
	}
	if b := f.statementDoc(t, f.b); b.ClosingBalance != 3000809 {
		t.Fatalf("statement of b = %+v", b)
	}
}

// TestAMonthNotClosedHasNoStatement: neither the business month, nor one of an account opened after it, nor an account
// of another entity.
func TestAMonthNotClosedHasNoStatement(t *testing.T) {
	f := newFebruary(t)
	later := f.openWith(t, withFees)
	pgtest.AsOwner(t, testPool, func(tx pgx.Tx) error { // a period with a row is not closed for that
		_, err := tx.Exec(t.Context(), `INSERT INTO calendar.periods (legal_entity_id, period, status) VALUES ($1, '2026-03', 'open')`, f.entity)
		return err
	})
	f.generate(t)

	for what, err := range map[string]error{
		"March":                      errOf(f.deposits.Statement(t.Context(), testPool, f.entity, f.a.ID, "2026-03")),
		"an account opened in March": errOf(f.deposits.Statement(t.Context(), testPool, f.entity, later.ID, "2026-02")),
	} {
		if !errors.Is(err, deposits.ErrStatementNotFound) {
			t.Errorf("%s: %v, want ErrStatementNotFound", what, err)
		}
	}
	if err := errOf(f.deposits.Statement(t.Context(), testPool, ids.New(), f.a.ID, "2026-02")); !errors.Is(err, deposits.ErrAccountNotFound) {
		t.Errorf("another entity's account: %v, want ErrAccountNotFound", err)
	}
}

func errOf(_ deposits.Statement, err error) error { return err }

// TestTheDatabaseKeepsTheStatements: the application cannot change nor remove a statement or a completed period, the
// owner neither, and a statement's hash is its document's.
func TestTheDatabaseKeepsTheStatements(t *testing.T) {
	f := newFebruary(t)
	f.generate(t)
	a, entity := "'"+f.a.ID.String()+"'", "'"+f.entity.String()+"'"
	for name, c := range map[string]struct{ sql, code string }{
		"update a statement":    {`UPDATE deposits.statements SET content_hash = content_hash WHERE account_id = ` + a, "42501"},
		"delete a statement":    {`DELETE FROM deposits.statements WHERE account_id = ` + a, "42501"},
		"delete a period":       {`DELETE FROM deposits.statement_periods WHERE legal_entity_id = ` + entity, "42501"},
		"a hash of other bytes": {`INSERT INTO deposits.statements (account_id, period, document, content_hash) VALUES (` + a + `, '2026-01', '{}', repeat('0', 64))`, "23514"},
	} {
		err := postgres.NewUnitOfWork(testPool).Do(t.Context(), func(tx postgres.Tx) error {
			_, err := tx.Exec(t.Context(), c.sql)
			return err
		})
		var pgErr *pgconn.PgError
		if !errors.As(err, &pgErr) || pgErr.Code != c.code {
			t.Errorf("%s: %v, want %s", name, err, c.code)
		}
	}
	expectProhibited(t, []prohibited{
		{"rewrite a statement", `UPDATE deposits.statements SET content_hash = content_hash WHERE account_id = ` + a, "never updated, deleted or truncated"},
		{"erase a statement", `DELETE FROM deposits.statements WHERE account_id = ` + a, "never updated, deleted or truncated"},
		{"truncate the statements", `TRUNCATE deposits.statements`, "never updated, deleted or truncated"},
		{"reopen a period", `DELETE FROM deposits.statement_periods WHERE legal_entity_id = ` + entity, "never updated, deleted or truncated"},
		{"rewrite a period", `UPDATE deposits.statement_periods SET period = period WHERE legal_entity_id = ` + entity, "never updated, deleted or truncated"},
		{"truncate the periods", `TRUNCATE deposits.statement_periods`, "never updated, deleted or truncated"},
	})
}
