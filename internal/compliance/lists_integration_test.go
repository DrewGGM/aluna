//go:build integration

package compliance_test

import (
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/DrewGGM/aluna/internal/compliance"
	"github.com/DrewGGM/aluna/internal/ids"
	"github.com/DrewGGM/aluna/internal/party"
	"github.com/DrewGGM/aluna/internal/postgres"
)

// listOwners serves the excerpts of the published lists of the sources' testdata and a page of made-up PEPs, as
// their owners publish them, and at /SDN-2.CSV the SDN list after HENAO MONTOYA (6717) left it.
func listOwners(t *testing.T) compliance.ListSources {
	t.Helper()
	files := map[string]string{
		"/SDN.CSV": "internal/adapters/sources/testdata/sdn.csv", "/ALT.CSV": "internal/adapters/sources/testdata/alt.csv",
		"/consolidated.xml": "internal/adapters/sources/testdata/un.xml",
	}
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/pep.json" {
			_, _ = w.Write([]byte(`[{":id":"row-pep-1","numero_documento":"1000000001","nombre_pep":"ANA PEREZ RUIZ"}]`))
			return
		}
		body, err := os.ReadFile(files[strings.Replace(r.URL.Path, "-2", "", 1)])
		if r.URL.Path == "/SDN-2.CSV" {
			body = regexp.MustCompile(`(?m)^6717,.*\n`).ReplaceAll(body, nil)
		}
		if err != nil {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write(body)
	}))
	t.Cleanup(ts.Close)
	return compliance.ListSources{
		OFACSDN: ts.URL + "/SDN.CSV", OFACAlt: ts.URL + "/ALT.CSV", UN: ts.URL + "/consolidated.xml",
		PEP: ts.URL + "/pep.json", Client: ts.Client(),
	}
}

// identityService says who entity's parties are, by the start of their external reference.
func identityService(t *testing.T, entity ids.ID) *httptest.Server {
	t.Helper()
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		party, _ := strings.CutPrefix(r.URL.Path, "/"+entity.String()+"/party/")
		switch {
		case strings.HasPrefix(party, "sanctioned-"):
			_, _ = w.Write([]byte(`{"names": ["Miguel Ángel Rodríguez Orejuela"], "documents": []}`))
		case strings.HasPrefix(party, "pep-"):
			_, _ = w.Write([]byte(`{"names": ["Ana Pérez Ruiz"], "documents": ["1.000.000.001"]}`))
		case strings.HasPrefix(party, "clean-"):
			_, _ = w.Write([]byte(`{"names": ["Carolina Restrepo Uribe"], "documents": ["52111222"]}`))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(ts.Close)
	return ts
}

// listWorld is a world whose service screens against the lists, with the identity service of identityService.
func listWorld(t *testing.T) *world {
	t.Helper()
	w := &world{entity: newWorld(t).entity, events: &recorder{}}
	ts := identityService(t, w.entity)
	svc, err := compliance.NewWithLists(t.Context(), w.events, testPool, compliance.ListScreeningConfig{
		IdentityURL: ts.URL, IdentityToken: "t0k3n", Threshold: 0.92, MaxAge: 48 * time.Hour, Client: ts.Client(), Log: slog.Default(),
	})
	if err != nil {
		t.Fatal(err)
	}
	w.svc = svc
	return w
}

// refresh refreshes the lists from sources.
func refresh(t *testing.T, sources compliance.ListSources) {
	t.Helper()
	if err := compliance.RefreshLists(t.Context(), postgres.NewUnitOfWork(testPool), sources); err != nil {
		t.Fatal(err)
	}
}

// entries is how many entries the lists have, as stored.
func entries(t *testing.T) int {
	t.Helper()
	var n int
	if err := testPool.QueryRow(t.Context(), `SELECT coalesce(sum(entries), 0) FROM compliance.lists`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// screened screens p and is the outcome and the vendor's reference its case keeps.
func (w *world) screened(t *testing.T, p party.Party) (compliance.Outcome, string) {
	t.Helper()
	var outcome compliance.Outcome
	if err := w.do(t, func(tx postgres.Tx) error {
		var err error
		outcome, err = w.svc.ScreenParty(t.Context(), tx, p)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	var ref string
	if err := testPool.QueryRow(t.Context(), `SELECT coalesce(vendor_ref, '') FROM compliance.cases WHERE subject_id = $1`, p.ID).Scan(&ref); err != nil {
		t.Fatal(err)
	}
	return outcome, ref
}

// TestAPartyIsScreenedAgainstTheOfficialLists (FR-42, ADR-0018): a service loaded before the lists screens with
// none, failing closed, and keeps doing so right after they are refreshed from their owners, until it reads them
// again: a sanctioned party waits, never cleared. One loaded after the refresh finds a sanctioned party by its name
// in another order and with accents, and a PEP by its document, both waiting with the entry named, and clears anyone
// else. Refreshing the same lists again keeps them; a list that changed is replaced.
func TestAPartyIsScreenedAgainstTheOfficialLists(t *testing.T) {
	before := listWorld(t)
	if outcome, ref := before.screened(t, before.partyOf("clean-")); outcome != compliance.OutcomePending || ref != "" {
		t.Fatalf("screening without lists = %s (%q), want it pending with no answer", outcome, ref)
	}
	sources := listOwners(t)
	refresh(t, sources)
	refresh(t, sources)
	if outcome, _ := before.screened(t, before.partyOf("sanctioned-")); outcome != compliance.OutcomePending {
		t.Fatalf("a sanctioned party before the lists are read again = %s, want it pending", outcome)
	}

	w := listWorld(t)
	for prefix, want := range map[string]struct {
		outcome compliance.Outcome
		ref     string
	}{
		"sanctioned-": {compliance.OutcomePending, "ofac_sdn:4108~1.00"},
		"pep-":        {compliance.OutcomePending, "pep_co:row-pep-1=document"},
		"clean-":      {compliance.OutcomeClear, "lists"},
	} {
		if outcome, ref := w.screened(t, w.partyOf(prefix)); outcome != want.outcome || ref != want.ref {
			t.Errorf("a party %s = %s (%q), want %s (%q)", prefix, outcome, ref, want.outcome, want.ref)
		}
	}
	if n := entries(t); n != 6 {
		t.Fatalf("entries = %d, want the 3 of OFAC, 2 of the UN and 1 PEP", n)
	}
	sources.OFACSDN = strings.Replace(sources.OFACSDN, "SDN.CSV", "SDN-2.CSV", 1)
	refresh(t, sources)
	if n := entries(t); n != 5 {
		t.Fatalf("entries after HENAO MONTOYA left = %d, want 5", n)
	}
}
