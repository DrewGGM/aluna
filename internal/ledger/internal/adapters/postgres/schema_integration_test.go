//go:build integration

package postgres

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/DrewGGM/aluna/internal/ids"
	"github.com/DrewGGM/aluna/internal/postgres/pgtest"
)

// SQLSTATEs set by migration 0002 (and the PostgreSQL ones that go with them).
const (
	checkViolation        = "23514" // accounting invariant or CHECK
	foreignKeyViolation   = "23503"
	insufficientPrivilege = "42501"
	prohibitedStatement   = "2F003" // change to something already posted
	none                  = ""      // no error
)

var (
	testPool     *pgxpool.Pool
	legalEntityA = ids.New()
	legalEntityB = ids.New()
)

func TestMain(m *testing.M) { os.Exit(pgtest.Run(m, &testPool)) }

type entry struct {
	account  ids.ID
	currency string
	amount   int64
}

func newAccount(t *testing.T, entity ids.ID, kind, currency string) ids.ID {
	t.Helper()
	id := ids.New()
	_, err := testPool.Exec(t.Context(),
		`INSERT INTO ledger.accounts (id, legal_entity_id, code, kind, currency) VALUES ($1, $2, $3, $4, $5)`,
		id, entity, "acct-"+id.String(), kind, currency)
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func insertTransaction(ctx context.Context, tx pgx.Tx, entity ids.ID) (ids.ID, error) {
	id := ids.New()
	_, err := tx.Exec(ctx, `INSERT INTO ledger.transactions
		(id, legal_entity_id, code, value_date, business_date, source, external_ref, actor, channel, rule_version)
		VALUES ($1, $2, 'test', current_date, current_date, 'test', $3, 'tester', 'test', 'v1')`,
		id, entity, id.String())
	return id, err
}

func insertEntries(ctx context.Context, tx pgx.Tx, txID ids.ID, entries []entry) error {
	for _, e := range entries {
		_, err := tx.Exec(ctx, `INSERT INTO ledger.entries (transaction_id, account_id, currency, amount) VALUES ($1, $2, $3, $4)`,
			txID, e.account, e.currency, e.amount)
		if err != nil {
			return err
		}
	}
	return nil
}

// post posts a transaction of entity A as aluna_app (the role of the pgtest pool) and returns its ID and
// the error of the COMMIT or of the INSERT that fails first.
func post(t *testing.T, entries ...entry) (ids.ID, error) {
	t.Helper()
	var txID ids.ID
	err := pgx.BeginFunc(t.Context(), testPool, func(tx pgx.Tx) error {
		var err error
		if txID, err = insertTransaction(t.Context(), tx, legalEntityA); err != nil {
			return err
		}
		return insertEntries(t.Context(), tx, txID, entries)
	})
	return txID, err
}

func sqlState(err error) string {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		return pgErr.Code
	}
	return none
}

func TestPoolConnectsAsAppRole(t *testing.T) {
	var user string
	if err := testPool.QueryRow(t.Context(), `SELECT current_user`).Scan(&user); err != nil || user != "aluna_app" {
		t.Fatalf("current_user = %q (%v): the tests must run with the application role", user, err)
	}
}

func TestDeferredBalanceCheck(t *testing.T) {
	cash, deposit := newAccount(t, legalEntityA, "asset", "COP"), newAccount(t, legalEntityA, "liability", "COP")
	usdCash, usdDeposit := newAccount(t, legalEntityA, "asset", "USD"), newAccount(t, legalEntityA, "liability", "USD")
	otherEntity := newAccount(t, legalEntityB, "liability", "COP")
	cases := map[string]struct {
		entries []entry
		want    string
	}{
		"balanced":                          {[]entry{{cash, "COP", 10_000_000}, {deposit, "COP", -10_000_000}}, none},
		"unbalanced":                        {[]entry{{cash, "COP", 100}, {deposit, "COP", -99}}, checkViolation},
		"a single entry":                    {[]entry{{cash, "COP", 100}}, checkViolation},
		"no entries":                        {nil, checkViolation},
		"two currencies, each at 0":         {[]entry{{cash, "COP", 5}, {deposit, "COP", -5}, {usdCash, "USD", 7}, {usdDeposit, "USD", -7}}, none},
		"two crossed currencies":            {[]entry{{cash, "COP", 5}, {usdDeposit, "USD", -5}}, checkViolation},
		"zero amount":                       {[]entry{{cash, "COP", 0}, {deposit, "COP", 0}}, checkViolation},
		"currency other than the account's": {[]entry{{cash, "USD", 5}, {deposit, "USD", -5}}, foreignKeyViolation},
		"another legal entity's account":    {[]entry{{cash, "COP", 5}, {otherEntity, "COP", -5}}, foreignKeyViolation},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := post(t, tc.entries...)

			if got := sqlState(err); got != tc.want {
				t.Fatalf("SQLSTATE = %q, want %q (error: %v)", got, tc.want, err)
			}
		})
	}
}

// TestPostedTransactionTakesNoNewEntries covers the botched retry: an already committed transaction takes
// no new legs even if they are balanced, because they would duplicate the movement.
func TestPostedTransactionTakesNoNewEntries(t *testing.T) {
	cash, deposit := newAccount(t, legalEntityA, "asset", "COP"), newAccount(t, legalEntityA, "liability", "COP")
	txID, err := post(t, entry{cash, "COP", 100}, entry{deposit, "COP", -100})
	if err != nil {
		t.Fatal(err)
	}

	err = pgx.BeginFunc(t.Context(), testPool, func(tx pgx.Tx) error {
		return insertEntries(t.Context(), tx, txID, []entry{{cash, "COP", 100}, {deposit, "COP", -100}})
	})

	if got := sqlState(err); got != prohibitedStatement {
		t.Fatalf("SQLSTATE = %q, want %q (error: %v)", got, prohibitedStatement, err)
	}
}

// TestEntriesInsideASavepointArePosted checks that the "same database transaction" rule does not break a
// posting with savepoints (pgx uses them with a nested Begin).
func TestEntriesInsideASavepointArePosted(t *testing.T) {
	cash, deposit := newAccount(t, legalEntityA, "asset", "COP"), newAccount(t, legalEntityA, "liability", "COP")

	err := pgx.BeginFunc(t.Context(), testPool, func(tx pgx.Tx) error {
		txID, err := insertTransaction(t.Context(), tx, legalEntityA)
		if err != nil {
			return err
		}
		return pgx.BeginFunc(t.Context(), tx, func(savepoint pgx.Tx) error {
			return insertEntries(t.Context(), savepoint, txID, []entry{{cash, "COP", 100}, {deposit, "COP", -100}})
		})
	})
	if err != nil {
		t.Fatal(err)
	}
}

// TestWritersCannotChooseTimeOrEntity sends forged posted_at, posted_xact, value date and entity; the
// database replaces them with its own (transactions_stamp, entries_stamp).
func TestWritersCannotChooseTimeOrEntity(t *testing.T) {
	cash, deposit := newAccount(t, legalEntityA, "asset", "COP"), newAccount(t, legalEntityA, "liability", "COP")
	forged := time.Date(2001, 1, 1, 0, 0, 0, 0, time.UTC)
	txID := ids.New()
	err := pgx.BeginFunc(t.Context(), testPool, func(tx pgx.Tx) error {
		_, err := tx.Exec(t.Context(), `INSERT INTO ledger.transactions
			(id, legal_entity_id, code, value_date, business_date, source, external_ref, actor, channel, rule_version, posted_at, posted_xact)
			VALUES ($1, $2, 'test', current_date, current_date, 'test', $3, 'tester', 'test', 'v1', $4, '1')`,
			txID, legalEntityA, txID.String(), forged)
		if err != nil {
			return err
		}
		_, err = tx.Exec(t.Context(), `INSERT INTO ledger.entries
			(transaction_id, account_id, currency, amount, legal_entity_id, value_date, posted_at)
			VALUES ($1, $2, 'COP', 100, $3, $4, $5), ($1, $6, 'COP', -100, $3, $4, $5)`,
			txID, cash, legalEntityB, forged.Format(time.DateOnly), forged, deposit)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}

	var forgedTx, mismatched int
	err = testPool.QueryRow(t.Context(), `SELECT
			(SELECT count(*) FROM ledger.transactions WHERE id = $1 AND (posted_at < now() - interval '1 hour' OR posted_xact = '1')),
			(SELECT count(*) FROM ledger.entries e JOIN ledger.transactions t ON t.id = e.transaction_id
			 WHERE e.transaction_id = $1 AND (e.posted_at <> t.posted_at OR e.value_date <> t.value_date
			   OR e.legal_entity_id <> t.legal_entity_id))`,
		txID).Scan(&forgedTx, &mismatched)
	if err != nil || forgedTx != 0 || mismatched != 0 {
		t.Fatalf("forged values remained: transaction %d, entries %d (err = %v)", forgedTx, mismatched, err)
	}
}

// TestSetConstraintsImmediateCannotSneakALeg covers the attempt to bring the deferred check forward with
// SET CONSTRAINTS ALL IMMEDIATE (it needs no privileges) and then add a leg that creates money.
func TestSetConstraintsImmediateCannotSneakALeg(t *testing.T) {
	cash, deposit := newAccount(t, legalEntityA, "asset", "COP"), newAccount(t, legalEntityA, "liability", "COP")

	err := pgx.BeginFunc(t.Context(), testPool, func(tx pgx.Tx) error {
		txID, err := insertTransaction(t.Context(), tx, legalEntityA)
		if err != nil {
			return err
		}
		if err := insertEntries(t.Context(), tx, txID, []entry{{cash, "COP", 100}, {deposit, "COP", -100}}); err != nil {
			return err
		}
		if _, err := tx.Exec(t.Context(), `SET CONSTRAINTS ALL IMMEDIATE`); err != nil {
			return err
		}
		return insertEntries(t.Context(), tx, txID, []entry{{cash, "COP", 999_999_999}})
	})

	if got := sqlState(err); got != checkViolation {
		t.Fatalf("SQLSTATE = %q, want %q (error: %v)", got, checkViolation, err)
	}
}

// reversalOf posts, as aluna_app, a balanced transaction of entity that reverses reversed.
func reversalOf(t *testing.T, entity, reversed ids.ID, self bool) error {
	t.Helper()
	cash, deposit := newAccount(t, entity, "asset", "COP"), newAccount(t, entity, "liability", "COP")
	return pgx.BeginFunc(t.Context(), testPool, func(tx pgx.Tx) error {
		id := ids.New()
		target := reversed
		if self {
			target = id
		}
		_, err := tx.Exec(t.Context(), `INSERT INTO ledger.transactions
			(id, legal_entity_id, code, value_date, business_date, source, external_ref, actor, channel, rule_version, reverses_id)
			VALUES ($1, $2, 'reversal', current_date, current_date, 'test', $3, 'tester', 'test', 'v1', $4)`,
			id, entity, id.String(), target)
		if err != nil {
			return err
		}
		return insertEntries(t.Context(), tx, id, []entry{{cash, "COP", -100}, {deposit, "COP", 100}})
	})
}

func TestReversalStaysInsideTheLegalEntity(t *testing.T) {
	cash, deposit := newAccount(t, legalEntityA, "asset", "COP"), newAccount(t, legalEntityA, "liability", "COP")
	original, err := post(t, entry{cash, "COP", 100}, entry{deposit, "COP", -100})
	if err != nil {
		t.Fatal(err)
	}

	err = reversalOf(t, legalEntityB, original, false)

	if got := sqlState(err); got != foreignKeyViolation {
		t.Fatalf("an entity does not reverse another's transactions: SQLSTATE = %q (error: %v)", got, err)
	}
	if err := reversalOf(t, legalEntityA, original, false); err != nil {
		t.Fatalf("the reversal within the same entity must go through: %v", err)
	}
}

func TestTransactionIsReversedOnlyOnce(t *testing.T) {
	cash, deposit := newAccount(t, legalEntityA, "asset", "COP"), newAccount(t, legalEntityA, "liability", "COP")
	original, err := post(t, entry{cash, "COP", 100}, entry{deposit, "COP", -100})
	if err != nil {
		t.Fatal(err)
	}
	if err := reversalOf(t, legalEntityA, original, false); err != nil {
		t.Fatal(err)
	}

	err = reversalOf(t, legalEntityA, original, false)

	if got := constraintOf(err); got != "transactions_single_reversal" {
		t.Fatalf("the second reversal must conflict with transactions_single_reversal, got %v", err)
	}
}

func TestExternalRefIsIdempotentPerSource(t *testing.T) {
	cash, deposit := newAccount(t, legalEntityA, "asset", "COP"), newAccount(t, legalEntityA, "liability", "COP")
	ref := "batch-" + ids.New().String()
	postWithRef := func() error {
		return pgx.BeginFunc(t.Context(), testPool, func(tx pgx.Tx) error {
			id := ids.New()
			_, err := tx.Exec(t.Context(), `INSERT INTO ledger.transactions
				(id, legal_entity_id, code, value_date, business_date, source, external_ref, actor, channel, rule_version)
				VALUES ($1, $2, 'test', current_date, current_date, 'payroll', $3, 'tester', 'test', 'v1')`,
				id, legalEntityA, ref)
			if err != nil {
				return err
			}
			return insertEntries(t.Context(), tx, id, []entry{{cash, "COP", 100}, {deposit, "COP", -100}})
		})
	}
	if err := postWithRef(); err != nil {
		t.Fatal(err)
	}

	err := postWithRef()

	if got := constraintOf(err); got != "transactions_idempotent_ref" {
		t.Fatalf("repeating (source, external_ref) must conflict with transactions_idempotent_ref, got %v", err)
	}
}

func constraintOf(err error) string {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		return pgErr.ConstraintName
	}
	return none
}

func TestAccountConstraints(t *testing.T) {
	taken := "cash-" + ids.New().String()
	if _, err := testPool.Exec(t.Context(), `INSERT INTO ledger.accounts (id, legal_entity_id, code, kind, currency)
		VALUES ($1, $2, $3, 'asset', 'COP')`, ids.New(), legalEntityA, taken); err != nil {
		t.Fatal(err)
	}
	cases := map[string]struct {
		code, kind, mode string
		owner            any
		want             string
	}{
		"code repeated in the entity": {taken, "asset", "sync", nil, "23505"},
		"unknown kind":                {"", "revenue", "sync", nil, checkViolation},
		"unknown mode":                {"", "asset", "lazy", nil, checkViolation},
		"async account with an owner": {"", "liability", "async", "customer-7", checkViolation},
		"internal async account":      {"", "asset", "async", nil, none},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			id, code := ids.New(), tc.code
			if code == "" { // a unique code per run, to isolate the constraint under test
				code = "acct-" + id.String()
			}
			_, err := testPool.Exec(t.Context(), `INSERT INTO ledger.accounts
				(id, legal_entity_id, code, kind, currency, balance_mode, owner_ref) VALUES ($1, $2, $3, $4, 'COP', $5, $6)`,
				id, legalEntityA, code, tc.kind, tc.mode, tc.owner)

			if got := sqlState(err); got != tc.want {
				t.Fatalf("SQLSTATE = %q, want %q (error: %v)", got, tc.want, err)
			}
		})
	}
}

func TestTransactionCannotReverseItself(t *testing.T) {
	err := reversalOf(t, legalEntityA, ids.ID{}, true)

	if got := constraintOf(err); got != "transactions_not_self_reversal" {
		t.Fatalf("want transactions_not_self_reversal, got %v", err)
	}
}

func TestCurrencyMustBeAnISOCode(t *testing.T) {
	_, err := testPool.Exec(t.Context(),
		`INSERT INTO ledger.accounts (id, legal_entity_id, code, kind, currency) VALUES ($1, $2, 'bad', 'asset', 'cop')`,
		ids.New(), legalEntityA)

	if got := sqlState(err); got != checkViolation {
		t.Fatalf("SQLSTATE = %q, want %q", got, checkViolation)
	}
}

func TestPostedRowsAreImmutable(t *testing.T) {
	cash, deposit := newAccount(t, legalEntityA, "asset", "COP"), newAccount(t, legalEntityA, "liability", "COP")
	txID, err := post(t, entry{cash, "COP", 100}, entry{deposit, "COP", -100})
	if err != nil {
		t.Fatal(err)
	}
	byID := []any{txID}
	attempts := map[string]struct {
		sql  string
		args []any
	}{
		"UPDATE entry":          {`UPDATE ledger.entries SET amount = amount * 2 WHERE transaction_id = $1`, byID},
		"DELETE entry":          {`DELETE FROM ledger.entries WHERE transaction_id = $1`, byID},
		"UPDATE transaction":    {`UPDATE ledger.transactions SET code = 'other' WHERE id = $1`, byID},
		"DELETE transaction":    {`DELETE FROM ledger.transactions WHERE id = $1`, byID},
		"TRUNCATE entries":      {`TRUNCATE ledger.entries`, nil},
		"TRUNCATE transactions": {`TRUNCATE ledger.transactions, ledger.entries, ledger.holds`, nil}, // the tables that reference it
		"TRUNCATE holds":        {`TRUNCATE ledger.holds`, nil},
	}
	for name, attempt := range attempts {
		t.Run(name+" as aluna_app", func(t *testing.T) {
			if got := execInTx(t, false, attempt.sql, attempt.args...); got != insufficientPrivilege { // never granted
				t.Fatalf("SQLSTATE = %q, want %q", got, insufficientPrivilege)
			}
		})
		t.Run(name+" as owner", func(t *testing.T) {
			if got := execInTx(t, true, attempt.sql, attempt.args...); got != prohibitedStatement { // the trigger
				t.Fatalf("SQLSTATE = %q, want %q", got, prohibitedStatement)
			}
		})
	}
	var n int
	if err := testPool.QueryRow(t.Context(), `SELECT count(*) FROM ledger.entries WHERE transaction_id = $1`, txID).Scan(&n); err != nil || n != 2 {
		t.Fatalf("the original transaction must stay intact: %d entries, err = %v", n, err)
	}
}

// execInTx runs stmt in its own transaction, as aluna_app or, with asOwner, switching back to the
// connection's user (the tables' owner), and returns the SQLSTATE.
func execInTx(t *testing.T, asOwner bool, stmt string, args ...any) string {
	t.Helper()
	var state string
	err := pgx.BeginFunc(t.Context(), testPool, func(tx pgx.Tx) error {
		if asOwner {
			if _, err := tx.Exec(t.Context(), "SET LOCAL ROLE NONE"); err != nil {
				return err
			}
		}
		_, err := tx.Exec(t.Context(), stmt, args...)
		state = sqlState(err)
		return err
	})
	if err != nil && state == none {
		t.Fatal(err)
	}
	return state
}
