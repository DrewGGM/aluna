//go:build integration

package products_test

import (
	"cmp"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/DrewGGM/aluna/internal/calendar"
	"github.com/DrewGGM/aluna/internal/money"
	"github.com/DrewGGM/aluna/internal/postgres"
	"github.com/DrewGGM/aluna/internal/postgres/pgtest"
	"github.com/DrewGGM/aluna/internal/products"
)

// usedYears hands out the years of the tests: the parameters are the tenant's, shared by every test of the
// database (also when -count repeats them), so each test loads its own years, from 3000 on.
var usedYears atomic.Int64

// years returns the first of n consecutive years that no other test uses, nor the year before or after them.
func years(n int) int { return 3000 + int(usedYears.Add(int64(n+2))) - n }

func date(year int, month time.Month, day int) calendar.Date {
	return calendar.Date{Year: year, Month: month, Day: day}
}

func value(t *testing.T, s string) money.Precise {
	t.Helper()
	p, err := money.ParsePrecise(s)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

// yearOf is parameter kind with value v for the whole year.
func yearOf(t *testing.T, kind products.MarketKind, modality string, year int, v string) products.MarketParameter {
	t.Helper()
	return products.MarketParameter{Kind: kind, Modality: modality, ValidFrom: date(year, 1, 1), ValidTo: date(year+1, 1, 1), Value: value(t, v)}
}

func load(t *testing.T, svc *products.Service, p products.MarketParameter) error {
	t.Helper()
	return try(t, func(tx postgres.Tx) error { return svc.LoadMarketParameter(t.Context(), tx, p) })
}

// TestAParameterHoldsInItsPeriod (FR-21): a value holds from valid_from to the day before valid_to; outside
// its period there is no value (fail closed), and the value comes back without losing a decimal.
func TestAParameterHoldsInItsPeriod(t *testing.T) {
	y := years(3)
	svc := products.New(&recorder{})
	if err := cmp.Or(load(t, svc, yearOf(t, products.UVT, "", y, "49799")),
		load(t, svc, yearOf(t, products.UsuryRate, "consumer", y, "0.25316789"))); err != nil {
		t.Fatal(err)
	}

	first, firstErr := svc.MarketParameter(t.Context(), testPool, products.UVT, "", date(y, 1, 1))
	last, lastErr := svc.MarketParameter(t.Context(), testPool, products.UVT, "", date(y, 12, 31))
	usury, usuryErr := svc.MarketParameter(t.Context(), testPool, products.UsuryRate, "consumer", date(y, 6, 15))
	_, after := svc.MarketParameter(t.Context(), testPool, products.UVT, "", date(y+1, 1, 1))
	_, before := svc.MarketParameter(t.Context(), testPool, products.UVT, "", date(y-1, 12, 31))
	_, otherModality := svc.MarketParameter(t.Context(), testPool, products.UsuryRate, "microcredit", date(y, 6, 15))

	if firstErr != nil || lastErr != nil || usuryErr != nil || first.String() != "49799" || last.String() != "49799" || usury.String() != "0.25316789" {
		t.Fatalf("UVT on its first and last day = %s, %s (%v, %v); usury = %s (%v)", first, last, firstErr, lastErr, usury, usuryErr)
	}
	for name, err := range map[string]error{"the day after": after, "the day before": before, "another modality": otherModality} {
		if !errors.Is(err, products.ErrNoMarketParameter) {
			t.Errorf("%s = %v, want ErrNoMarketParameter", name, err)
		}
	}
}

// TestPeriodsDoNotOverlap: a value whose period overlaps another of the same kind and modality is rejected
// when checking and when loading, also when it starts on the same day; the next period and another
// modality are loaded.
func TestPeriodsDoNotOverlap(t *testing.T) {
	y := years(3)
	svc := products.New(&recorder{})
	if err := load(t, svc, yearOf(t, products.SMMLV, "", y, "1423500")); err != nil {
		t.Fatal(err)
	}
	overlapping := products.MarketParameter{Kind: products.SMMLV, ValidFrom: date(y, 12, 31), ValidTo: date(y+1, 2, 1), Value: value(t, "1500000")}

	checked := svc.CheckMarketParameter(t.Context(), testPool, overlapping)
	loaded := load(t, svc, overlapping)
	sameStart := load(t, svc, yearOf(t, products.SMMLV, "", y, "1500000"))
	next := load(t, svc, yearOf(t, products.SMMLV, "", y+1, "1500000"))
	tenor := load(t, svc, yearOf(t, products.IBR, "overnight", y, "0.0925"))
	otherTenor := load(t, svc, yearOf(t, products.IBR, "three_months", y, "0.0931"))

	if !errors.Is(checked, products.ErrMarketParameterOverlap) || !errors.Is(loaded, products.ErrMarketParameterOverlap) ||
		!errors.Is(sameStart, products.ErrMarketParameterOverlap) {
		t.Fatalf("overlapping period: check = %v, load = %v, same start = %v; want ErrMarketParameterOverlap", checked, loaded, sameStart)
	}
	if next != nil || tenor != nil || otherTenor != nil {
		t.Fatalf("next period = %v, IBR overnight = %v, IBR three months = %v; want them loaded", next, tenor, otherTenor)
	}
}

// TestConcurrentOverlappingLoadsCannotBothHold: while a load is uncommitted, an overlapping one, loaded
// without checking first, waits for it on the exclusion constraint and then is rejected.
func TestConcurrentOverlappingLoadsCannotBothHold(t *testing.T) {
	y := years(3)
	svc := products.New(&recorder{})
	firstValue := yearOf(t, products.UVB, "", y, "3.8742")
	// it starts on another day, so the exclusion constraint, and not the primary key, rejects it
	secondValue := products.MarketParameter{Kind: products.UVB, ValidFrom: date(y, 6, 1), ValidTo: date(y+1, 6, 1), Value: value(t, "3.9")}
	loaded, release, first := make(chan struct{}), make(chan struct{}), make(chan error, 1)
	releaseOnce := sync.OnceFunc(func() { close(release) })
	defer releaseOnce() // if the test fails early, the first transaction is not left open
	go func() {
		first <- postgres.NewUnitOfWork(testPool).Do(t.Context(), func(tx postgres.Tx) error {
			if err := svc.LoadMarketParameter(t.Context(), tx, firstValue); err != nil {
				return err
			}
			close(loaded)
			<-release
			return nil
		})
	}()
	select {
	case <-loaded:
	case err := <-first:
		t.Fatalf("the first load did not happen: %v", err)
	}
	second := make(chan error, 1)
	go func() {
		second <- postgres.NewUnitOfWork(testPool).Do(t.Context(), func(tx postgres.Tx) error {
			return svc.LoadMarketParameter(t.Context(), tx, secondValue)
		})
	}()
	waitForLockWait(t, "transactionid")
	releaseOnce()

	if err := <-first; err != nil {
		t.Fatal(err)
	}
	if err := <-second; !errors.Is(err, products.ErrMarketParameterOverlap) {
		t.Fatalf("second load = %v, want ErrMarketParameterOverlap", err)
	}
	if got, err := svc.MarketParameter(t.Context(), testPool, products.UVB, "", date(y, 7, 1)); err != nil || got.String() != "3.8742" {
		t.Fatalf("UVB = %s (%v), want the first value", got, err)
	}
}

// TestInvalidParametersAreRejected: an unknown kind, a missing or extra modality, an empty period, a value
// that is not positive, a rate above 1 and a value with more than 8 decimals.
func TestInvalidParametersAreRejected(t *testing.T) {
	y := years(3)
	svc := products.New(&recorder{})
	cases := map[string]products.MarketParameter{
		"unknown kind":           yearOf(t, "inflation", "", y, "0.05"),
		"usury without modality": yearOf(t, products.UsuryRate, "", y, "0.25"),
		"UVT with modality":      yearOf(t, products.UVT, "annual", y, "49799"),
		"modality in capitals":   yearOf(t, products.IBR, "Overnight", y, "0.09"),
		"empty period":           {Kind: products.UVT, ValidFrom: date(y, 1, 1), ValidTo: date(y, 1, 1), Value: value(t, "49799")},
		"zero":                   yearOf(t, products.SMMLV, "", y, "0"),
		"negative":               yearOf(t, products.SMMLV, "", y, "-1"),
		"rate above 1":           yearOf(t, products.UsuryRate, "consumer", y, "1.0001"),
		"9 decimals":             yearOf(t, products.IBR, "overnight", y, "0.092500001"),
	}
	for name, p := range cases {
		t.Run(name, func(t *testing.T) {
			if err := load(t, svc, p); !errors.Is(err, products.ErrInvalidMarketParameter) {
				t.Fatalf("got %v, want ErrInvalidMarketParameter", err)
			}
		})
	}
	if err := load(t, svc, yearOf(t, products.UsuryRate, "consumer", y, "1.000")); err != nil {
		t.Fatalf("a rate of exactly 1 = %v, want it loaded", err)
	}
}

// TestALoadedValueDoesNotChange: even as the tables' owner, a value is not updated, deleted or truncated.
func TestALoadedValueDoesNotChange(t *testing.T) {
	svc := products.New(&recorder{})
	p := yearOf(t, products.UVT, "", years(1), "49799")
	if err := load(t, svc, p); err != nil {
		t.Fatal(err)
	}
	from := time.Date(p.ValidFrom.Year, p.ValidFrom.Month, p.ValidFrom.Day, 0, 0, 0, 0, time.UTC)
	statements := []struct {
		sql  string
		args []any
	}{
		{`UPDATE products.market_parameters SET value = 1 WHERE kind = 'uvt' AND valid_from = $1`, []any{from}},
		{`DELETE FROM products.market_parameters WHERE kind = 'uvt' AND valid_from = $1`, []any{from}},
		{`TRUNCATE products.market_parameters`, nil},
	}
	for _, st := range statements {
		var err error
		pgtest.AsOwner(t, testPool, func(tx pgx.Tx) error {
			sp, beginErr := tx.Begin(t.Context())
			if beginErr != nil {
				return beginErr
			}
			_, err = sp.Exec(t.Context(), st.sql, st.args...)
			return sp.Rollback(t.Context())
		})
		var pgErr *pgconn.PgError
		if !errors.As(err, &pgErr) || pgErr.Code != "2F003" {
			t.Errorf("%s = %v, want prohibited_sql_statement_attempted", st.sql, err)
		}
	}
}
