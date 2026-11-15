package sources

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/DrewGGM/aluna/internal/compliance/internal/domain/matching"
)

// owner serves the lists of testdata, excerpts of the published ones, and two pages of made-up PEPs.
func owner(t *testing.T) *httptest.Server {
	t.Helper()
	files := map[string]string{"/SDN.CSV": "testdata/sdn.csv", "/ALT.CSV": "testdata/alt.csv", "/consolidated.xml": "testdata/un.xml"}
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/pep.json" {
			if q := r.URL.Query(); q.Get("$offset") == "0" && strings.HasPrefix(q.Get("$select"), ":id,") {
				_, _ = w.Write([]byte(`[{":id":"row-a","numero_documento":"1000000001","nombre_pep":"ANA PEREZ RUIZ"},
					{":id":"row-b","numero_documento":"1000000001","nombre_pep":"ANA MARIA PEREZ RUIZ"},
					{":id":"row-c","numero_documento":"1000000002","nombre_pep":"LUIS GOMEZ"}]`))
				return
			}
			_, _ = w.Write([]byte(`[]`))
			return
		}
		file, served := files[r.URL.Path]
		if !served {
			http.NotFound(w, r)
			return
		}
		body, err := os.ReadFile(file)
		if err != nil {
			t.Error(err)
		}
		_, _ = w.Write(body)
	}))
	t.Cleanup(ts.Close)
	return ts
}

func byID(entries []matching.Entry, id string) matching.Entry {
	for _, e := range entries {
		if e.ID == id {
			return e
		}
	}
	return matching.Entry{}
}

// TestOFACIsReadWithItsAliasesAndDocuments (ADR-0018): an SDN entry has its name, the aliases of ALT.CSV and of
// its remarks, and the documents of its remarks, each once.
func TestOFACIsReadWithItsAliasesAndDocuments(t *testing.T) {
	ts := owner(t)

	version, entries, err := OFAC(ts.Client(), ts.URL+"/SDN.CSV", ts.URL+"/ALT.CSV").Fetch(t.Context())

	if err != nil || len(entries) != 3 || version == "" {
		t.Fatalf("OFAC = %d entries, version %q (%v), want the 3 of the excerpt", len(entries), version, err)
	}
	if got := byID(entries, "36").Names; !slices.Equal(got, []string{"AEROCARIBBEAN AIRLINES", "AERO-CARIBBEAN"}) {
		t.Errorf("names of 36 = %v, want its own and its ALT.CSV alias", got)
	}
	if got := byID(entries, "4108"); got.Names[1] != "EL SENOR" || !slices.Equal(got.Documents, []string{"6095803"}) {
		t.Errorf("4108 = %+v, want the aliases and the cédula of its remarks", got)
	}
	if got := byID(entries, "6717").Documents; !slices.Equal(got, []string{"16215230", "16215230-1"}) {
		t.Errorf("documents of 6717 = %v, want its cédula (its passport's number) and its NIT, once each", got)
	}
}

// TestTheUNListIsReadWithItsAliasesAndDocuments (ADR-0018): an individual is its names together, its aliases and
// its documents; an entity, its name and aliases.
func TestTheUNListIsReadWithItsAliasesAndDocuments(t *testing.T) {
	ts := owner(t)

	_, entries, err := UN(ts.Client(), ts.URL+"/consolidated.xml").Fetch(t.Context())

	if err != nil || len(entries) != 2 {
		t.Fatalf("UN = %d entries (%v), want an individual and an entity", len(entries), err)
	}
	person := byID(entries, "6908010")
	if !slices.Equal(matching.Words(person.Names[0]), []string{"BAUDOIN", "MYAMURO", "NGARUYE", "WA"}) ||
		!slices.Contains(person.Names, "Colonel Baudoin NGARUYE") || !slices.Equal(person.Documents, []string{"1-78-09-44621-80"}) {
		t.Errorf("individual = %+v", person)
	}
	if entity := byID(entries, "6908402"); entity.Names[0] != "ADF" || !slices.Contains(entity.Names, "Allied Democratic Forces") {
		t.Errorf("entity = %+v", entity)
	}
}

// TestPEPsAreReadPageByPage (ADR-0018): the PEPs are read until a page comes short, one entry per person with the
// names of all its posts, its id its first post's row id, never its document.
func TestPEPsAreReadPageByPage(t *testing.T) {
	ts := owner(t)

	_, entries, err := PEP(ts.Client(), ts.URL+"/pep.json").Fetch(t.Context())

	if err != nil || len(entries) != 2 || !slices.Equal(byID(entries, "row-a").Names, []string{"ANA PEREZ RUIZ", "ANA MARIA PEREZ RUIZ"}) ||
		!slices.Equal(byID(entries, "row-a").Documents, []string{"1000000001"}) {
		t.Fatalf("PEP = %+v (%v), want two people, the first with both its posts' names", entries, err)
	}
}

// TestAPEPSourceWithoutRowIDsIsAnError (ADR-0018): a post with a document and no row id cannot be an entry, whose id
// would otherwise be the document.
func TestAPEPSourceWithoutRowIDsIsAnError(t *testing.T) {
	if _, _, err := parsePEP([]byte(`[{"numero_documento":"1000000001","nombre_pep":"ANA PEREZ RUIZ"}]`)); !errors.Is(err, errNoRowID) {
		t.Fatalf("a post without its row id = %v, want errNoRowID", err)
	}
}

// TestAListThatDoesNotDownloadIsAnError: an owner that does not answer 200 is errDownload, naming the source
// without its query, which may carry a token.
func TestAListThatDoesNotDownloadIsAnError(t *testing.T) {
	ts := owner(t)
	_, _, err := UN(ts.Client(), ts.URL+"/missing.xml?app_token=s3cr3t").Fetch(t.Context())
	if !errors.Is(err, errDownload) || !strings.Contains(err.Error(), "/missing.xml") || strings.Contains(err.Error(), "s3cr3t") {
		t.Fatalf("a list that is not there = %v, want errDownload naming it without its query", err)
	}
}

// TestAFullPageOfPEPsIsNotTheLast (ADR-0018): a page as long as the page size is followed by the next one, even if
// a post of it has no document and is no entry; the query the resource has is kept; a source that never comes
// short, ignoring the offset, is an error, not a list.
func TestAFullPageOfPEPsIsNotTheLast(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Query().Get("app_token") != "kept":
			w.WriteHeader(http.StatusForbidden)
		case r.URL.Path == "/forever.json":
			_, _ = w.Write([]byte(`[{":id":"row-1","numero_documento":"1","nombre_pep":"A B"},{":id":"row-2","numero_documento":"2","nombre_pep":"C D"}]`))
		case r.URL.Query().Get("$offset") == "0":
			_, _ = w.Write([]byte(`[{":id":"row-0","nombre_pep":"SIN DOCUMENTO"},{":id":"row-1","numero_documento":"1","nombre_pep":"ANA PEREZ"}]`))
		default:
			_, _ = w.Write([]byte(`[{":id":"row-2","numero_documento":"2","nombre_pep":"LUIS GOMEZ"}]`))
		}
	}))
	t.Cleanup(ts.Close)

	_, entries, err := fetchPEP(t.Context(), ts.Client(), ts.URL+"/pep.json?app_token=kept", 2)
	if err != nil || len(entries) != 2 || entries[1].ID != "row-2" {
		t.Fatalf("PEPs = %+v (%v), want both pages read", entries, err)
	}
	if _, _, err := fetchPEP(t.Context(), ts.Client(), ts.URL+"/forever.json?app_token=kept", 2); !errors.Is(err, errTooManyPages) {
		t.Fatalf("a source that never ends = %v, want errTooManyPages", err)
	}
}
