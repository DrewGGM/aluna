//go:build integration

package jobs_test

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/DrewGGM/aluna/internal/compliance"
)

// listOwners serves the excerpts of the published lists in compliance's testdata and a made-up PEP.
func listOwners(t *testing.T) *compliance.ListSources {
	t.Helper()
	testdata := http.FileServer(http.Dir("../compliance/internal/adapters/sources/testdata"))
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/pep.json" {
			_, _ = w.Write([]byte(`[{":id":"row-pep-1","numero_documento":"1000000001","nombre_pep":"ANA PEREZ RUIZ"}]`))
			return
		}
		testdata.ServeHTTP(w, r)
	}))
	t.Cleanup(ts.Close)
	return &compliance.ListSources{
		OFACSDN: ts.URL + "/sdn.csv", OFACAlt: ts.URL + "/alt.csv", UN: ts.URL + "/un.xml", PEP: ts.URL + "/pep.json", Client: ts.Client(),
	}
}

// TestTheWorkerRefreshesTheLists (ADR-0018): when screening reads the lists, the worker refreshes them on startup,
// as aluna_app, and stores the three.
func TestTheWorkerRefreshesTheLists(t *testing.T) {
	deps := newDeps(t)
	deps.ListSources = listOwners(t)

	waitFor(t, startWorker(t, deps), "compliance.lists.refresh")

	if n := count(t, `SELECT count(*) FROM compliance.lists WHERE entries > $1`, 0); n != 3 {
		t.Fatalf("lists stored = %d, want OFAC, the UN and the PEPs", n)
	}
}
