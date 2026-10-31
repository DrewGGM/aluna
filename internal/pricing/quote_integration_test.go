//go:build integration

package pricing_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/DrewGGM/aluna/internal/ids"
	"github.com/DrewGGM/aluna/internal/money"
	"github.com/DrewGGM/aluna/internal/outbox"
	"github.com/DrewGGM/aluna/internal/postgres"
	"github.com/DrewGGM/aluna/internal/postgres/pgtest"
	"github.com/DrewGGM/aluna/internal/pricing"
	"github.com/DrewGGM/aluna/internal/products"
)

var testPool *pgxpool.Pool

func TestMain(m *testing.M) { os.Exit(pgtest.Run(m, &testPool)) }

type discard struct{}

func (discard) Emit(context.Context, postgres.Tx, outbox.Event) error { return nil }

// world is an entity with the tiered savings product of products' testdata published: a withdrawal fee of
// 4,500 COP that the first three withdrawals of the month do not pay (month_tx_count < 3), and a transfer fee
// of 3,000 COP.
type world struct {
	entity  ids.ID
	version products.Version
	pricing *pricing.Service
}

func newWorld(t *testing.T) world {
	t.Helper()
	definition, err := os.ReadFile(filepath.Join("..", "products", "internal", "domain", "testdata", "valid-tiered-savings.json"))
	if err != nil {
		t.Fatal(err)
	}
	catalog := products.New(discard{})
	w := world{entity: ids.New(), pricing: pricing.New(catalog)}
	inTx(t, func(tx postgres.Tx) error {
		p, err := catalog.CreateProduct(t.Context(), tx, w.entity, "p"+strings.ReplaceAll(ids.New().String(), "-", ""), "Tiered savings")
		if err != nil {
			return err
		}
		v, err := catalog.CreateVersion(t.Context(), tx, w.entity, p.ID, "product-designer", definition)
		if err != nil {
			return err
		}
		if _, err := catalog.RequestPublication(t.Context(), tx, w.entity, v.ID, "product-designer"); err != nil {
			return err
		}
		w.version, err = catalog.Publish(t.Context(), tx, w.entity, v.ID)
		return err
	})
	return w
}

func inTx(t *testing.T, fn func(tx postgres.Tx) error) {
	t.Helper()
	if err := postgres.NewUnitOfWork(testPool).Do(t.Context(), fn); err != nil {
		t.Fatal(err)
	}
}

func try(t *testing.T, fn func(tx postgres.Tx) error) error {
	t.Helper()
	return postgres.NewUnitOfWork(testPool).Do(t.Context(), fn)
}

func cop(t *testing.T, units int64) money.Amount {
	t.Helper()
	c, err := money.CurrencyOf("COP")
	if err != nil {
		t.Fatal(err)
	}
	return money.New(units, c)
}

func (w world) quote(t *testing.T, account ids.ID, op pricing.Operation, amount money.Amount) (pricing.Quote, error) {
	t.Helper()
	var q pricing.Quote
	err := try(t, func(tx postgres.Tx) error {
		var err error
		q, err = w.pricing.Quote(t.Context(), tx, pricing.QuoteInput{
			LegalEntityID: w.entity, AccountID: account, ProductVersionID: w.version.ID, Operation: op, Amount: amount,
			Facts: products.RuleVars{PartyKind: "natural", RiskLevel: "low", BalanceUnits: 100000000},
		})
		return err
	})
	return q, err
}

func (w world) charges(t *testing.T, u pricing.QuoteUse) (pricing.Charges, error) {
	t.Helper()
	var c pricing.Charges
	err := try(t, func(tx postgres.Tx) error {
		var err error
		c, err = w.pricing.ChargesFor(t.Context(), tx, w.entity, u)
		return err
	})
	return c, err
}

// useOf is the use of quote q by the operation it quoted.
func useOf(q pricing.Quote) pricing.QuoteUse {
	return pricing.QuoteUse{QuoteID: q.ID, AccountID: q.AccountID, Operation: q.Operation, Amount: q.Amount}
}

// TestAQuoteShowsTheFeesOfTheOperation (FR-28, US-7): a quote shows the fees of the product for its operation,
// valid for 5 minutes; the exemption of the withdrawal fee (month_tx_count < 3) counts the quotes the account
// used this month, so the fourth withdrawal pays it.
func TestAQuoteShowsTheFeesOfTheOperation(t *testing.T) {
	w := newWorld(t)
	account := ids.New()
	if _, err := w.quote(t, account, pricing.Withdrawal, cop(t, 1000000)); err != nil { // shown, never used: it does not count
		t.Fatal(err)
	}
	w.freeWithdrawals(t, account)

	fourth, err := w.quote(t, account, pricing.Withdrawal, cop(t, 1000000))
	transfer, terr := w.quote(t, account, pricing.Transfer, cop(t, 1000000))

	if err != nil || terr != nil {
		t.Fatal(errors.Join(err, terr))
	}
	if want := []pricing.Fee{{Code: "withdrawal_fee", Amount: cop(t, 450000)}}; !slices.Equal(fourth.Fees, want) {
		t.Fatalf("fourth withdrawal fees = %+v, want %+v", fourth.Fees, want)
	}
	if want := []pricing.Fee{{Code: "transfer_fee", Amount: cop(t, 300000)}}; !slices.Equal(transfer.Fees, want) {
		t.Fatalf("transfer fees = %+v, want %+v", transfer.Fees, want)
	}
	if fourth.ExpiresAt.Sub(fourth.CreatedAt) != 5*time.Minute || fourth.Amount != cop(t, 1000000) || fourth.AccountID != account {
		t.Fatalf("quote = %+v, want it for the account, of 10,000 COP, valid for 5 minutes", fourth)
	}
}

// freeWithdrawals quotes and uses the account's first three withdrawals of the month, which must be free.
func (w world) freeWithdrawals(t *testing.T, account ids.ID) {
	t.Helper()
	for i := range 3 {
		q, err := w.quote(t, account, pricing.Withdrawal, cop(t, 1000000))
		if err != nil {
			t.Fatal(err)
		}
		if len(q.Fees) != 0 {
			t.Fatalf("withdrawal %d pays %+v, want it free (the first three of the month)", i+1, q.Fees)
		}
		if _, err := w.charges(t, useOf(q)); err != nil {
			t.Fatal(err)
		}
	}
}

// TestOnlyTheQuotesOfThisMonthAndEntityCount: month_tx_count counts the quotes of the account used since the
// first instant of the month in America/Bogota, and only those of its entity.
func TestOnlyTheQuotesOfThisMonthAndEntityCount(t *testing.T) {
	w := newWorld(t)
	account := ids.New()
	another := w
	another.entity = ids.New()
	another.freeWithdrawals(t, account)
	w.usedAt(t, account, "-1 microsecond") // the last instant of the previous month
	w.usedAt(t, account, "0")
	w.usedAt(t, account, "0")

	third, err := w.quote(t, account, pricing.Withdrawal, cop(t, 1000000))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.charges(t, useOf(third)); err != nil {
		t.Fatal(err)
	}
	fourth, err := w.quote(t, account, pricing.Withdrawal, cop(t, 1000000))

	if err != nil || len(third.Fees) != 0 || len(fourth.Fees) != 1 {
		t.Fatalf("third withdrawal pays %+v, fourth %+v (%v); want the third free and the fourth paid", third.Fees, fourth.Fees, err)
	}
}

// usedAt stores, as the tables' owner, a withdrawal quote of the account used at the first instant of the
// month in America/Bogota plus offset, an SQL interval.
func (w world) usedAt(t *testing.T, account ids.ID, offset string) {
	t.Helper()
	pgtest.AsOwner(t, testPool, func(tx pgx.Tx) error {
		_, err := tx.Exec(t.Context(), `WITH month AS (
				SELECT date_trunc('month', clock_timestamp() AT TIME ZONE 'America/Bogota') AT TIME ZONE 'America/Bogota' AS start)
			INSERT INTO pricing.quotes (id, legal_entity_id, account_id, operation, currency, amount_units, fees, created_at, expires_at, used_at)
			SELECT $1, $2, $3, 'withdrawal', 'COP', 1000000, '[]', start - interval '1 minute', start + interval '4 minutes',
				start + $4::interval FROM month`,
			ids.New(), w.entity, account, offset)
		return err
	})
}

// TestAQuoteIsChargedOnceAndExactly (FR-28, US-7): the operation that uses a quote is charged exactly what it
// showed; used again or expired, it is ErrQuoteUnavailable.
func TestAQuoteIsChargedOnceAndExactly(t *testing.T) {
	w := newWorld(t)
	account := ids.New()
	q, err := w.quote(t, account, pricing.Transfer, cop(t, 500000))
	if err != nil {
		t.Fatal(err)
	}
	expired := useOf(q)
	expired.QuoteID = w.expiredQuote(t, account)

	charged, err := w.charges(t, useOf(q))
	_, again := w.charges(t, useOf(q))
	_, late := w.charges(t, expired)

	if want := []pricing.Fee{{Code: "transfer_fee", Amount: cop(t, 300000)}}; err != nil || !slices.Equal(charged.Fees, want) ||
		charged.GMF != cop(t, 0) {
		t.Fatalf("charges = %+v (%v), want the quoted 3,000 COP and no GMF", charged, err)
	}
	for name, err := range map[string]error{"used again": again, "expired": late} {
		if !errors.Is(err, pricing.ErrQuoteUnavailable) {
			t.Errorf("a quote %s = %v, want ErrQuoteUnavailable", name, err)
		}
	}
}

// TestAQuoteOnlyPricesWhatItQuoted (FR-28): a quote is only for its entity, account, operation and amount: the
// fees of a free withdrawal are not those of a transfer, nor of another amount. Any other use is
// ErrQuoteUnavailable and leaves it for its own.
func TestAQuoteOnlyPricesWhatItQuoted(t *testing.T) {
	w := newWorld(t)
	q, err := w.quote(t, ids.New(), pricing.Withdrawal, cop(t, 500000))
	if err != nil {
		t.Fatal(err)
	}
	usd, err := money.CurrencyOf("USD")
	if err != nil {
		t.Fatal(err)
	}
	another := w
	another.entity = ids.New()
	uses := map[string]func(u *pricing.QuoteUse){
		"another account":   func(u *pricing.QuoteUse) { u.AccountID = ids.New() },
		"another operation": func(u *pricing.QuoteUse) { u.Operation = pricing.Transfer },
		"another amount":    func(u *pricing.QuoteUse) { u.Amount = cop(t, 50000000) },
		"another currency":  func(u *pricing.QuoteUse) { u.Amount = money.New(500000, usd) },
	}
	for name, change := range uses {
		u := useOf(q)
		change(&u)
		if _, err := w.charges(t, u); !errors.Is(err, pricing.ErrQuoteUnavailable) {
			t.Errorf("used for %s = %v, want ErrQuoteUnavailable", name, err)
		}
	}
	if _, err := another.charges(t, useOf(q)); !errors.Is(err, pricing.ErrQuoteUnavailable) {
		t.Errorf("used by another entity = %v, want ErrQuoteUnavailable", err)
	}

	if _, err := w.charges(t, useOf(q)); err != nil {
		t.Fatalf("used for what it quoted = %v, want it still available", err)
	}
}

// expiredQuote stores, as the tables' owner, a quote of the account that expired a minute ago.
func (w world) expiredQuote(t *testing.T, account ids.ID) ids.ID {
	t.Helper()
	id := ids.New()
	pgtest.AsOwner(t, testPool, func(tx pgx.Tx) error {
		_, err := tx.Exec(t.Context(), `INSERT INTO pricing.quotes (id, legal_entity_id, account_id, operation, currency, amount_units, fees,
			created_at, expires_at) VALUES ($1, $2, $3, 'transfer', 'COP', 500000, '[]', clock_timestamp() - interval '6 minutes',
			clock_timestamp() - interval '1 minute')`, id, w.entity, account)
		return err
	})
	return id
}

// TestAQuoteMustBeValid: an operation that is not priced, an amount that is not positive or in another currency than
// the product's is ErrInvalidQuote.
func TestAQuoteMustBeValid(t *testing.T) {
	w := newWorld(t)
	usd, err := money.CurrencyOf("USD")
	if err != nil {
		t.Fatal(err)
	}
	for name, c := range map[string]struct {
		op     pricing.Operation
		amount money.Amount
	}{
		"a deposit":      {"deposit", cop(t, 100)},
		"nothing":        {pricing.Withdrawal, cop(t, 0)},
		"in dollars":     {pricing.Withdrawal, money.New(100, usd)},
		"a negative one": {pricing.Transfer, cop(t, -100)},
	} {
		if _, err := w.quote(t, ids.New(), c.op, c.amount); !errors.Is(err, pricing.ErrInvalidQuote) {
			t.Errorf("%s = %v, want ErrInvalidQuote", name, err)
		}
	}
}

// TestTheDatabaseGuardsTheQuotes: even as the tables' owner, a quote is never deleted or truncated, what it
// shows never changes and it is used only once.
func TestTheDatabaseGuardsTheQuotes(t *testing.T) {
	w := newWorld(t)
	used, err := w.quote(t, ids.New(), pricing.Transfer, cop(t, 500000))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.charges(t, useOf(used)); err != nil {
		t.Fatal(err)
	}
	unused, err := w.quote(t, ids.New(), pricing.Transfer, cop(t, 500000))
	if err != nil {
		t.Fatal(err)
	}
	where := func(q pricing.Quote) string { return ` WHERE id = '` + q.ID.String() + `'` }
	for name, sql := range map[string]string{
		"change the fees":               `UPDATE pricing.quotes SET fees = '[]'` + where(unused),
		"change the fees as it is used": `UPDATE pricing.quotes SET fees = '[]', used_at = clock_timestamp()` + where(unused),
		"use it without a time":         `UPDATE pricing.quotes SET used_at = NULL` + where(unused),
		"use it again":                  `UPDATE pricing.quotes SET used_at = clock_timestamp()` + where(used),
		"delete":                        `DELETE FROM pricing.quotes` + where(unused),
		"truncate":                      `TRUNCATE pricing.quotes`,
	} {
		var err error
		pgtest.AsOwner(t, testPool, func(tx pgx.Tx) error {
			sp, beginErr := tx.Begin(t.Context())
			if beginErr != nil {
				return beginErr
			}
			_, err = sp.Exec(t.Context(), sql)
			return sp.Rollback(t.Context())
		})
		var pgErr *pgconn.PgError
		if !errors.As(err, &pgErr) || pgErr.Code != "2F003" {
			t.Errorf("%s = %v, want prohibited_sql_statement_attempted", name, err)
		}
	}
}
