//go:build integration

package pricing_test

import (
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"pgregory.net/rapid"

	"github.com/DrewGGM/aluna/internal/calendar"
	"github.com/DrewGGM/aluna/internal/ids"
	"github.com/DrewGGM/aluna/internal/money"
	"github.com/DrewGGM/aluna/internal/postgres"
	"github.com/DrewGGM/aluna/internal/postgres/pgtest"
	"github.com/DrewGGM/aluna/internal/pricing"
	"github.com/DrewGGM/aluna/internal/products"
)

// exemptionCap is 350 times the UVT of 2026 that gmfWorld loads (52,374 pesos): 18,330,900 COP, in minor
// units.
const exemptionCap = 1833090000

// gmfWorld is an entity on 2026-03-02 with the savings product published, which allows the GMF exemption,
// priced with modality m; the UVT of 2026 is loaded.
func gmfWorld(t *testing.T, m pricing.Modality) world {
	t.Helper()
	w := worldOf(t, "valid-savings.json", m, "2026-03-02")
	loadUVT(t, calendar.Date{Year: 2026, Month: time.January, Day: 1}, calendar.Date{Year: 2027, Month: time.January, Day: 1}, "52374")
	return w
}

// loadUVT loads a UVT of value pesos from day from to day to, unless another test of the package's database
// already did.
func loadUVT(t *testing.T, from, to calendar.Date, value string) {
	t.Helper()
	uvt, err := money.ParsePrecise(value)
	if err != nil {
		t.Fatal(err)
	}
	err = try(t, func(tx postgres.Tx) error {
		return products.New(discard{}).LoadMarketParameter(t.Context(), tx, products.MarketParameter{
			Kind: products.UVT, ValidFrom: from, ValidTo: to, Value: uvt,
		})
	})
	if err != nil && !errors.Is(err, products.ErrMarketParameterOverlap) {
		t.Fatal(err)
	}
}

// mark exempts account of party from GMF.
func (w world) mark(t *testing.T, party, account ids.ID) error {
	t.Helper()
	c, err := money.CurrencyOf("COP")
	if err != nil {
		t.Fatal(err)
	}
	return w.markIn(t, party, account, c)
}

func (w world) markIn(t *testing.T, party, account ids.ID, c money.Currency) error {
	t.Helper()
	return try(t, func(tx postgres.Tx) error {
		_, err := w.pricing.MarkExempt(t.Context(), tx, pricing.ExemptionInput{
			LegalEntityID: w.entity, AccountID: account, PartyID: party, ProductVersionID: w.version.ID, Currency: c,
		})
		return err
	})
}

// charge prices op of amount on account of party as it is done, without a quote.
func (w world) charge(t *testing.T, party, account ids.ID, op pricing.Operation, amount money.Amount) (pricing.Charges, error) {
	t.Helper()
	var c pricing.Charges
	err := try(t, func(tx postgres.Tx) error {
		var err error
		c, err = w.pricing.Charge(t.Context(), tx, w.input(party, account, op, amount))
		return err
	})
	return c, err
}

// exemptUsed is what party used of its exemptions, in all months.
func (w world) exemptUsed(t *testing.T, party ids.ID) (int64, error) {
	t.Helper()
	var used int64
	err := testPool.QueryRow(t.Context(), `SELECT coalesce(sum(exempt_used_units), 0) FROM pricing.gmf_monthly
		WHERE legal_entity_id = $1 AND party_id = $2`, w.entity, party).Scan(&used)
	return used, err
}

// TestAnExemptAccountPaysGMFOnlyOverItsCap (FR-29): the debits of an exempt account pay no GMF up to 350 UVT
// in the month, 18,330,900 COP in 2026, and 4 per thousand of what passes it: of a transfer of 5,000,000 after
// 15,000,000, 3,330,900 is exempt and 1,669,100 pays 6,676 COP. An account that is not exempt pays on all, as
// does the same account in another entity.
func TestAnExemptAccountPaysGMFOnlyOverItsCap(t *testing.T) {
	w := gmfWorld(t, pricing.SingleAccount)
	party, exempt, other := ids.New(), ids.New(), ids.New()
	if err := w.mark(t, party, exempt); err != nil {
		t.Fatal(err)
	}

	first, err := w.charge(t, party, exempt, pricing.Withdrawal, cop(t, 1500000000))
	second, serr := w.charge(t, party, exempt, pricing.Transfer, cop(t, 500000000))
	third, terr := w.charge(t, party, other, pricing.Withdrawal, cop(t, 100000000))
	elsewhere, eerr := gmfWorld(t, pricing.SingleAccount).charge(t, party, exempt, pricing.Withdrawal, cop(t, 100000000))
	used, uerr := w.exemptUsed(t, party)

	if err := errors.Join(err, serr, terr, eerr, uerr); err != nil {
		t.Fatal(err)
	}
	if elsewhere.GMF != cop(t, 400000) {
		t.Fatalf("the account in another entity pays %v, want 4,000 COP", elsewhere.GMF)
	}
	if first.GMF != cop(t, 0) || second.GMF != cop(t, 667600) || third.GMF != cop(t, 400000) || used != exemptionCap {
		t.Fatalf("GMF %v, %v and %v with %d exempt; want 0, 6,676 COP and 4,000 COP with the whole cap exempt",
			first.GMF, second.GMF, third.GMF, used)
	}
}

// TestAQuoteKeepsTheExemptionItShows (FR-28, FR-29): a quote shows the GMF with the exemption that remains,
// and the operation that uses it is charged exactly that. If another operation used that exemption
// meanwhile, the quote is ErrQuoteUnavailable, and a new one shows what remains.
func TestAQuoteKeepsTheExemptionItShows(t *testing.T) {
	w := gmfWorld(t, pricing.SingleAccount)
	party, account := ids.New(), ids.New()
	if err := w.mark(t, party, account); err != nil {
		t.Fatal(err)
	}
	a, err := w.quoteFor(t, party, account, pricing.Withdrawal, cop(t, 1000000000))
	b, berr := w.quoteFor(t, party, account, pricing.Withdrawal, cop(t, 1000000000))
	if err := errors.Join(err, berr); err != nil {
		t.Fatal(err)
	}

	charged, err := w.charges(t, useOf(a))
	_, late := w.charges(t, useOf(b))
	again, aerr := w.quoteFor(t, party, account, pricing.Withdrawal, cop(t, 1000000000))
	recharged, rerr := w.charges(t, useOf(again))

	if err := errors.Join(err, aerr, rerr); err != nil {
		t.Fatal(err)
	}
	if a.GMF != (pricing.GMF{Exempt: cop(t, 1000000000), Tax: cop(t, 0)}) || charged.GMF != cop(t, 0) {
		t.Fatalf("first quote %+v charged %v, want it exempt and charged no GMF", a.GMF, charged.GMF)
	}
	if !errors.Is(late, pricing.ErrQuoteUnavailable) {
		t.Fatalf("a quote whose exemption was used meanwhile = %v, want ErrQuoteUnavailable", late)
	}
	// 8,330,900 COP remain exempt; the other 1,669,100 pay 6,676.4, rounded to 6,676
	if want := (pricing.GMF{Exempt: cop(t, 833090000), Tax: cop(t, 667600)}); again.GMF != want || recharged.GMF != want.Tax {
		t.Fatalf("new quote %+v charged %v, want %+v", again.GMF, recharged.GMF, want)
	}
}

// TestTheExemptionIsOnOneAccountOrSplit (FR-29): with the exemption on a single account a party has only one
// exempt account; split, it has several, which share one cap.
func TestTheExemptionIsOnOneAccountOrSplit(t *testing.T) {
	single, split := gmfWorld(t, pricing.SingleAccount), gmfWorld(t, pricing.SplitAccounts)
	party, a, b, c, d := ids.New(), ids.New(), ids.New(), ids.New(), ids.New()

	err := errors.Join(single.mark(t, party, a), single.mark(t, ids.New(), ids.New()), split.mark(t, party, c), split.mark(t, party, d))
	if err != nil {
		t.Fatal(err)
	}
	for name, err := range map[string]error{
		"another account of the party, on a single account": single.mark(t, party, b),
		"the same account again":                            split.mark(t, party, c),
	} {
		if !errors.Is(err, pricing.ErrGMFExemptionTaken) {
			t.Errorf("%s = %v, want ErrGMFExemptionTaken", name, err)
		}
	}

	first, err := split.charge(t, party, c, pricing.Withdrawal, cop(t, 1500000000))
	second, serr := split.charge(t, party, d, pricing.Withdrawal, cop(t, 500000000))
	if err := errors.Join(err, serr); err != nil {
		t.Fatal(err)
	}
	if first.GMF != cop(t, 0) || second.GMF != cop(t, 667600) {
		t.Fatalf("split GMF %v and %v, want 0 and 6,676 COP: the second account uses what the first left", first.GMF, second.GMF)
	}
}

// TestOnlyAnEligibleAccountIsExempt (FR-29): an account of a product that does not allow the exemption, or
// not in COP, is ErrGMFNotEligible.
func TestOnlyAnEligibleAccountIsExempt(t *testing.T) {
	tiered, savings := newWorld(t), gmfWorld(t, pricing.SingleAccount)
	usd, err := money.CurrencyOf("USD")
	if err != nil {
		t.Fatal(err)
	}
	for name, err := range map[string]error{
		"a product that does not allow it": tiered.mark(t, ids.New(), ids.New()),
		"an account in dollars":            savings.markIn(t, ids.New(), ids.New(), usd),
	} {
		if !errors.Is(err, pricing.ErrGMFNotEligible) {
			t.Errorf("%s = %v, want ErrGMFNotEligible", name, err)
		}
	}
}

// TestAnExemptAccountNeedsAUVT: without a UVT for the year of the business date, the GMF of an exempt account is
// ErrUVTMissing; an account that is not exempt needs none.
func TestAnExemptAccountNeedsAUVT(t *testing.T) {
	w := worldOf(t, "valid-savings.json", pricing.SingleAccount, "2031-03-03")
	party, exempt := ids.New(), ids.New()
	if err := w.mark(t, party, exempt); err != nil {
		t.Fatal(err)
	}

	_, err := w.quoteFor(t, party, exempt, pricing.Withdrawal, cop(t, 100000000))
	other, oerr := w.quoteFor(t, party, ids.New(), pricing.Withdrawal, cop(t, 100000000))

	if !errors.Is(err, pricing.ErrUVTMissing) || oerr != nil || other.GMF.Tax != cop(t, 400000) {
		t.Fatalf("exempt quote = %v; another account's = %+v (%v), want ErrUVTMissing and 4,000 COP", err, other.GMF, oerr)
	}
}

// TestTheDatabaseGuardsTheGMF: even as the tables' owner, an exemption is never changed, deleted or
// truncated, and what a party used of its exemption only grows.
func TestTheDatabaseGuardsTheGMF(t *testing.T) {
	w := gmfWorld(t, pricing.SingleAccount)
	party, account := ids.New(), ids.New()
	if err := w.mark(t, party, account); err != nil {
		t.Fatal(err)
	}
	if _, err := w.charge(t, party, account, pricing.Withdrawal, cop(t, 100000000)); err != nil {
		t.Fatal(err)
	}
	exemption, monthly := ` WHERE account_id = '`+account.String()+`'`, ` WHERE party_id = '`+party.String()+`'`
	for name, sql := range map[string]string{
		"move the exemption":      `UPDATE pricing.gmf_exempt_accounts SET party_id = gen_random_uuid()` + exemption,
		"delete the exemption":    `DELETE FROM pricing.gmf_exempt_accounts` + exemption,
		"truncate the exemption":  `TRUNCATE pricing.gmf_exempt_accounts`,
		"give the exemption back": `UPDATE pricing.gmf_monthly SET exempt_used_units = exempt_used_units - 1` + monthly,
		"move what was used":      `UPDATE pricing.gmf_monthly SET month = month + interval '1 month'` + monthly,
		"delete what was used":    `DELETE FROM pricing.gmf_monthly` + monthly,
		"truncate what was used":  `TRUNCATE pricing.gmf_monthly`,
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

// TestAMonthOfQuotesNeverExemptsMoreThanItsCap (FR-28, FR-29): whatever two exempt accounts of a party quote
// and do, in any order, with the exemption split between them, what the month exempts never passes its cap,
// and each operation that uses a quote is charged exactly what it showed or, if the exemption it showed was
// used meanwhile, finds it unavailable.
func TestAMonthOfQuotesNeverExemptsMoreThanItsCap(t *testing.T) {
	w := gmfWorld(t, pricing.SplitAccounts)
	rapid.Check(t, func(rt *rapid.T) {
		party, accounts := ids.New(), []ids.ID{ids.New(), ids.New()}
		if err := errors.Join(w.mark(t, party, accounts[0]), w.mark(t, party, accounts[1])); err != nil {
			rt.Fatal(err)
		}
		var pending []pricing.Quote
		for range rapid.IntRange(1, 8).Draw(rt, "steps") {
			pending = w.step(t, rt, party, rapid.SampledFrom(accounts).Draw(rt, "account"), pending)
			if used, err := w.exemptUsed(t, party); err != nil || used > exemptionCap {
				rt.Fatalf("exempt %d (%v), more than the cap of %d", used, err, exemptionCap)
			}
		}
	})
}

// step is a random step of a month of party's account: it quotes a withdrawal, uses one of the pending quotes
// or withdraws without a quote. It returns the quotes still pending.
func (w world) step(t *testing.T, rt *rapid.T, party, account ids.ID, pending []pricing.Quote) []pricing.Quote {
	amount := cop(t, rapid.Int64Range(1, 1000000000).Draw(rt, "amount"))
	switch rapid.SampledFrom([]string{"quote", "use a quote", "pay as it is done"}).Draw(rt, "step") {
	case "quote":
		q, err := w.quoteFor(t, party, account, pricing.Withdrawal, amount)
		if err != nil {
			rt.Fatal(err)
		}
		return append(pending, q)
	case "use a quote":
		return w.useOne(t, rt, pending)
	default:
		if _, err := w.charge(t, party, account, pricing.Withdrawal, amount); err != nil {
			rt.Fatal(err)
		}
		return pending
	}
}

// useOne uses one of the pending quotes, if any, which charges exactly what it quoted or, if the exemption it
// quoted was used meanwhile, is unavailable. It returns the others.
func (w world) useOne(t *testing.T, rt *rapid.T, pending []pricing.Quote) []pricing.Quote {
	if len(pending) == 0 {
		return pending
	}
	i := rapid.IntRange(0, len(pending)-1).Draw(rt, "quote")
	q := pending[i]
	c, err := w.charges(t, useOf(q))
	if err != nil && !errors.Is(err, pricing.ErrQuoteUnavailable) || err == nil && (c.GMF != q.GMF.Tax || !slices.Equal(c.Fees, q.Fees)) {
		rt.Fatalf("quote %+v charged %+v (%v), want exactly what it quoted or ErrQuoteUnavailable", q, c, err)
	}
	return slices.Delete(pending, i, i+1)
}

// TestThePartyExemptionIsDecidedOneOperationAtATime (FR-29): while an operation on a party's exemption is in
// flight, another one waits for it and then sees what it did: a second exemption on a single account is
// taken, a debit waits even for a quote that only read the exemption, and a debit of 5,000,000 COP after
// 15,002,000 exempt pays on the 1,671,100 over the cap (6,684 COP) instead of failing.
func TestThePartyExemptionIsDecidedOneOperationAtATime(t *testing.T) {
	w := gmfWorld(t, pricing.SingleAccount)
	party, a, b := ids.New(), ids.New(), ids.New()
	second := w.whileHeld(t, func(tx postgres.Tx) error {
		_, err := w.pricing.MarkExempt(t.Context(), tx, pricing.ExemptionInput{
			LegalEntityID: w.entity, AccountID: a, PartyID: party, ProductVersionID: w.version.ID, Currency: cop(t, 0).Currency(),
		})
		return err
	}, func() error { return w.mark(t, party, b) })
	if !errors.Is(second, pricing.ErrGMFExemptionTaken) {
		t.Fatalf("a second exemption while the first is in flight = %v, want ErrGMFExemptionTaken", second)
	}
	if _, err := w.charge(t, party, a, pricing.Withdrawal, cop(t, 100000)); err != nil { // the month's row exists from now on
		t.Fatal(err)
	}
	second = w.whileHeld(t, func(tx postgres.Tx) error {
		_, err := w.pricing.Quote(t.Context(), tx, w.input(party, a, pricing.Withdrawal, cop(t, 100000)))
		return err
	}, func() error {
		_, err := w.charge(t, party, a, pricing.Withdrawal, cop(t, 100000))
		return err
	})
	if second != nil {
		t.Fatalf("a debit while a quote is in flight = %v", second)
	}

	var charged pricing.Charges
	second = w.whileHeld(t, func(tx postgres.Tx) error {
		_, err := w.pricing.Charge(t.Context(), tx, w.input(party, a, pricing.Withdrawal, cop(t, 1500000000)))
		return err
	}, func() error {
		var err error
		charged, err = w.charge(t, party, a, pricing.Withdrawal, cop(t, 500000000))
		return err
	})

	if second != nil || charged.GMF != cop(t, 668400) {
		t.Fatalf("a debit while another is in flight = %+v (%v), want 6,684 COP of GMF", charged, second)
	}
}

// TestAQuoteOfDecemberUsedInJanuary (FR-29): the cap of a month is 350 times the UVT of its year; a quote of
// December whose exemption passes January's cap is ErrQuoteUnavailable when it is used in January.
func TestAQuoteOfDecemberUsedInJanuary(t *testing.T) {
	w := gmfWorld(t, pricing.SingleAccount)
	pgtest.AsOwner(t, testPool, func(tx pgx.Tx) error {
		_, err := tx.Exec(t.Context(), `UPDATE calendar.legal_entities SET business_date = '2026-12-31' WHERE id = $1`, w.entity)
		return err
	})
	loadUVT(t, calendar.Date{Year: 2027, Month: time.January, Day: 1}, calendar.Date{Year: 2028, Month: time.January, Day: 1}, "1000") // a cap of 350,000 COP
	party, account := ids.New(), ids.New()
	if err := w.mark(t, party, account); err != nil {
		t.Fatal(err)
	}
	q, err := w.quoteFor(t, party, account, pricing.Withdrawal, cop(t, 1000000000))
	if err != nil {
		t.Fatal(err)
	}
	pgtest.AsOwner(t, testPool, func(tx pgx.Tx) error {
		_, err := tx.Exec(t.Context(), `UPDATE calendar.legal_entities SET business_date = '2027-01-04' WHERE id = $1`, w.entity)
		return err
	})

	_, used := w.charges(t, useOf(q))

	if q.GMF.Exempt != cop(t, 1000000000) || !errors.Is(used, pricing.ErrQuoteUnavailable) {
		t.Fatalf("December's quote %+v used in January = %v, want it exempt and then ErrQuoteUnavailable", q.GMF, used)
	}
}

// whileHeld runs first in a transaction and, before it commits, starts second, which must wait for it; it
// returns second's error.
func (w world) whileHeld(t *testing.T, first func(tx postgres.Tx) error, second func() error) error {
	t.Helper()
	done := make(chan error, 1)
	inTx(t, func(tx postgres.Tx) error {
		if err := first(tx); err != nil {
			return err
		}
		go func() { done <- second() }()
		select {
		case err := <-done:
			return errors.Join(errors.New("the second operation did not wait for the first"), err)
		case <-time.After(300 * time.Millisecond):
			return nil
		}
	})
	return <-done
}

// TestTheCapIsOfTheYearsUVT (FR-29): the cap is 350 times the UVT of the year, the one in force on January 1,
// even if another one holds later in the year: in August of a year whose UVT of 52,374 pesos changes to 1,000
// in July, 10,000,000 COP are still exempt.
func TestTheCapIsOfTheYearsUVT(t *testing.T) {
	w := worldOf(t, "valid-savings.json", pricing.SingleAccount, "2029-08-01")
	for _, p := range []struct {
		from, to time.Month
		value    string
	}{{time.January, time.July, "52374"}, {time.July, time.December, "1000"}} {
		loadUVT(t, calendar.Date{Year: 2029, Month: p.from, Day: 1}, calendar.Date{Year: 2029, Month: p.to, Day: 1}, p.value)
	}
	party, account := ids.New(), ids.New()
	if err := w.mark(t, party, account); err != nil {
		t.Fatal(err)
	}

	c, err := w.charge(t, party, account, pricing.Withdrawal, cop(t, 1000000000))

	if err != nil || c.GMF != cop(t, 0) {
		t.Fatalf("GMF in August = %v (%v), want none: the cap is of January's UVT", c.GMF, err)
	}
}
