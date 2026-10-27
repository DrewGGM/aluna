//go:build integration

package ledger_test

import (
	"testing"
	"time"

	"github.com/DrewGGM/aluna/internal/ids"
	"github.com/DrewGGM/aluna/internal/ledger"
)

func postedAsOf(t *testing.T, account ids.ID, at ledger.BalanceAsOf) int64 {
	t.Helper()
	posted, err := svc().PostedAsOf(t.Context(), testPool, account, at)
	if err != nil {
		t.Fatal(err)
	}
	return posted
}

// TestABackdatedEntryIsKnownFromItsPosting (FR-8): un entry con fecha valor de hace diez días asentado
// hoy cuenta para esa fecha valor, pero solo desde que se asentó: consultado como se sabía ayer, no
// está.
func TestABackdatedEntryIsKnownFromItsPosting(t *testing.T) {
	cash, deposit := createAccount(t, ledger.KindAsset), createAccount(t, ledger.KindLiability)
	in := transfer(t, cash.ID, deposit.ID, ids.New().String())
	tenDaysAgo := in.ValueDate.AddDate(0, 0, -10)
	in.ValueDate = tenDaysAgo
	posted, _ := mustPostRef(t, in)
	yesterday, elevenDaysAgo := posted.PostedAt.Add(-24*time.Hour), tenDaysAgo.AddDate(0, 0, -1)
	// las 22:00 en Bogotá del día anterior a la fecha valor: en UTC ya es el día de la fecha valor
	lateInBogota := time.Date(elevenDaysAgo.Year(), elevenDaysAgo.Month(), elevenDaysAgo.Day(), 22, 0, 0, 0, time.FixedZone("COT", -5*3600))

	cases := map[string]struct {
		at   ledger.BalanceAsOf
		want int64
	}{
		"sin filtros":                         {ledger.BalanceAsOf{}, -100},
		"como se sabe al asentarlo":           {ledger.BalanceAsOf{ValueDate: &tenDaysAgo, KnownAt: &posted.PostedAt}, -100},
		"como se sabía ayer":                  {ledger.BalanceAsOf{ValueDate: &tenDaysAgo, KnownAt: &yesterday}, 0},
		"antes de su fecha valor":             {ledger.BalanceAsOf{ValueDate: &elevenDaysAgo}, 0},
		"a su fecha valor, sin corte de alta": {ledger.BalanceAsOf{ValueDate: &tenDaysAgo}, -100},
		"solo el corte de alta, de ayer":      {ledger.BalanceAsOf{KnownAt: &yesterday}, 0},
		"el día anterior, en hora local":      {ledger.BalanceAsOf{ValueDate: &lateInBogota}, 0},
	}
	for name, tc := range cases {
		if got := postedAsOf(t, deposit.ID, tc.at); got != tc.want {
			t.Errorf("%s = %d, se esperaba %d", name, got, tc.want)
		}
	}
}

// TestTheBitemporalBalanceWithoutFiltersIsTheMaterialized: sin cortes, la suma de los entries es el
// saldo materializado de una cuenta sync.
func TestTheBitemporalBalanceWithoutFiltersIsTheMaterialized(t *testing.T) {
	deposit, _ := fundedDeposit(t)

	if got, want := postedAsOf(t, deposit.ID, ledger.BalanceAsOf{}), balanceOf(t, deposit.ID).Posted; got != want {
		t.Fatalf("saldo bitemporal sin cortes = %d, materializado %d", got, want)
	}
}
