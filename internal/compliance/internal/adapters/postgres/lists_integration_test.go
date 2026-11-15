//go:build integration

package postgres

import (
	"errors"
	"os"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/DrewGGM/aluna/internal/compliance/internal/app"
	"github.com/DrewGGM/aluna/internal/compliance/internal/domain/matching"
	"github.com/DrewGGM/aluna/internal/postgres"
	"github.com/DrewGGM/aluna/internal/postgres/pgtest"
)

var testPool *pgxpool.Pool

func TestMain(m *testing.M) { os.Exit(pgtest.Run(m, &testPool)) }

// loaded loads w and is what it loaded.
func loaded(t *testing.T, w *Watchlist) app.LoadedLists {
	t.Helper()
	if err := w.Load(t.Context()); err != nil {
		t.Fatal(err)
	}
	current, err := w.Current()
	if err != nil {
		t.Fatal(err)
	}
	return current
}

// TestAWatchlistReadsTheEntriesAgainOnlyWhenAListChanged (ADR-0018): a watchlist never loaded fails closed; a
// refresh of the same list keeps the index it has and takes the new refresh time, so that screening stays fresh
// without reading every entry again; a list that changed is indexed again.
func TestAWatchlistReadsTheEntriesAgainOnlyWhenAListChanged(t *testing.T) {
	lists, w := NewLists(postgres.NewUnitOfWork(testPool)), NewWatchlist(testPool)
	if _, err := w.Current(); !errors.Is(err, app.ErrStaleLists) {
		t.Fatalf("a watchlist never loaded = %v, want ErrStaleLists", err)
	}
	sanctioned := matching.Entry{List: app.ListOFAC, ID: "4108", Names: []string{"RODRIGUEZ OREJUELA, Miguel Angel"}}
	if err := lists.Replace(t.Context(), app.ListOFAC, "v1", []matching.Entry{sanctioned}); err != nil {
		t.Fatal(err)
	}
	first := loaded(t, w)

	if err := lists.Touch(t.Context(), app.ListOFAC); err != nil {
		t.Fatal(err)
	}
	touched := loaded(t, w)
	if touched.Index != first.Index || !touched.Refreshed[app.ListOFAC].After(first.Refreshed[app.ListOFAC]) {
		t.Fatalf("after a refresh of the same list: the same index %t, refreshed %s after %s",
			touched.Index == first.Index, touched.Refreshed[app.ListOFAC], first.Refreshed[app.ListOFAC])
	}

	another := matching.Entry{List: app.ListOFAC, ID: "6717", Names: []string{"HENAO MONTOYA, Arcangel de Jesus"}}
	if err := lists.Replace(t.Context(), app.ListOFAC, "v2", []matching.Entry{sanctioned, another}); err != nil {
		t.Fatal(err)
	}
	replaced := loaded(t, w)
	all := func(string) bool { return true }
	if hits := replaced.Index.Match([]string{"Arcangel de Jesus Henao Montoya"}, nil, 0.92, all); replaced.Index == first.Index || len(hits) != 1 {
		t.Fatalf("after a list changed: a new index %t, hits %v", replaced.Index != first.Index, hits)
	}
}
