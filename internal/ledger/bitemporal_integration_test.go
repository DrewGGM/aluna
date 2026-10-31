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

// TestABackdatedEntryIsKnownFromItsPosting (FR-8): an entry with a value date ten days ago posted today
// counts for that value date, but only from when it was posted: queried as known yesterday, it is not
// there.
func TestABackdatedEntryIsKnownFromItsPosting(t *testing.T) {
	cash, deposit := createAccount(t, ledger.KindAsset), createAccount(t, ledger.KindLiability)
	in := transfer(t, cash.ID, deposit.ID, ids.New().String())
	tenDaysAgo := in.ValueDate.AddDate(0, 0, -10)
	in.ValueDate = tenDaysAgo
	posted, _ := mustPostRef(t, in)
	yesterday, elevenDaysAgo := posted.PostedAt.Add(-24*time.Hour), tenDaysAgo.AddDate(0, 0, -1)
	// 22:00 in Bogota on the day before the value date: in UTC it is already the value date's day
	lateInBogota := time.Date(elevenDaysAgo.Year(), elevenDaysAgo.Month(), elevenDaysAgo.Day(), 22, 0, 0, 0, time.FixedZone("COT", -5*3600))

	cases := map[string]struct {
		at   ledger.BalanceAsOf
		want int64
	}{
		"no filters":                          {ledger.BalanceAsOf{}, -100},
		"as known when posted":                {ledger.BalanceAsOf{ValueDate: &tenDaysAgo, KnownAt: &posted.PostedAt}, -100},
		"as known yesterday":                  {ledger.BalanceAsOf{ValueDate: &tenDaysAgo, KnownAt: &yesterday}, 0},
		"before its value date":               {ledger.BalanceAsOf{ValueDate: &elevenDaysAgo}, 0},
		"at its value date, no knowledge cut": {ledger.BalanceAsOf{ValueDate: &tenDaysAgo}, -100},
		"only the knowledge cut, yesterday's": {ledger.BalanceAsOf{KnownAt: &yesterday}, 0},
		"the day before, in local time":       {ledger.BalanceAsOf{ValueDate: &lateInBogota}, 0},
	}
	for name, tc := range cases {
		if got := postedAsOf(t, deposit.ID, tc.at); got != tc.want {
			t.Errorf("%s = %d, want %d", name, got, tc.want)
		}
	}
}

// TestTheBitemporalBalanceWithoutFiltersIsTheMaterialized: without cuts, the sum of the entries is a sync
// account's materialized balance.
func TestTheBitemporalBalanceWithoutFiltersIsTheMaterialized(t *testing.T) {
	deposit, _ := fundedDeposit(t)

	if got, want := postedAsOf(t, deposit.ID, ledger.BalanceAsOf{}), balanceOf(t, deposit.ID).Posted; got != want {
		t.Fatalf("bitemporal balance without cuts = %d, materialized %d", got, want)
	}
}
