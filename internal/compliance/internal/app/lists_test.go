package app

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/DrewGGM/aluna/internal/compliance/internal/domain/matching"
	"github.com/DrewGGM/aluna/internal/ids"
)

type identities map[string]Identity

func (i identities) Identify(_ context.Context, s Subject) (Identity, error) {
	who, known := i[s.ExternalRef]
	if !known {
		return Identity{}, ErrUnknownSubject
	}
	return who, nil
}

type loaded LoadedLists

func (l loaded) Current() (LoadedLists, error) { return LoadedLists(l), nil }

// screening is a ListScreening of a sanctioned party and a PEP, the lists refreshed refreshed ago, at noon.
func screening(refreshed time.Duration) ListScreening {
	noon := time.Date(2026, time.March, 2, 12, 0, 0, 0, time.UTC)
	at := map[string]time.Time{ListOFAC: noon.Add(-refreshed), ListUN: noon.Add(-refreshed), ListPEP: noon.Add(-refreshed)}
	index := matching.NewIndex([]matching.Entry{
		{List: ListOFAC, ID: "4108", Names: []string{"RODRIGUEZ OREJUELA, Miguel Angel"}, Documents: []string{"6095803"}},
		{List: ListPEP, ID: "row-aaaa-bbbb-cccc", Names: []string{"ANA PEREZ RUIZ"}, Documents: []string{"1000000001"}},
	})
	return ListScreening{
		Identities: identities{
			"onboarding-1": {Names: []string{"Miguel Ángel Rodríguez Orejuela"}},
			"onboarding-2": {Names: []string{"Ana Pérez"}, Documents: []string{"1.000.000.001"}},
			"onboarding-3": {Names: []string{"Ana María Gómez"}, Documents: []string{"52111222"}},
			"blank":        {Names: []string{"", " ... ", "de la"}},
			"nameless":     {Documents: []string{"6.095.803"}},
		},
		Lists: loaded{Index: index, Refreshed: at}, Threshold: 0.92, MaxAge: 48 * time.Hour, Now: func() time.Time { return noon },
	}
}

// TestASubjectIsScreenedAgainstTheLists (FR-42, ADR-0018): a sanctioned subject is a possible match, by its name,
// naming the entry and the score, or by its document even with no name; a PEP is one, by its document, when
// registered as a party, not as a beneficiary; a subject the identity service does not know, or knows no name of,
// waits as a possible match too, without being the vendor failing; anyone else is clear.
func TestASubjectIsScreenedAgainstTheLists(t *testing.T) {
	l := screening(time.Hour)
	for name, tc := range map[string]struct {
		subject Subject
		want    Result
	}{
		"a sanctioned party":         {Subject{Kind: SubjectParty, ID: ids.New(), ExternalRef: "onboarding-1"}, Result{StatusPossibleMatch, "ofac_sdn:4108~1.00"}},
		"a sanctioned beneficiary":   {Subject{Kind: SubjectKey, ID: ids.New(), ExternalRef: "onboarding-1"}, Result{StatusPossibleMatch, "ofac_sdn:4108~1.00"}},
		"a PEP party":                {Subject{Kind: SubjectParty, ID: ids.New(), ExternalRef: "onboarding-2"}, Result{StatusPossibleMatch, "pep_co:row-aaaa-bbbb-cccc=document"}},
		"a PEP beneficiary":          {Subject{Kind: SubjectKey, ID: ids.New(), ExternalRef: "onboarding-2"}, Result{StatusClear, clearRef}},
		"an unknown subject":         {Subject{Kind: SubjectParty, ID: ids.New(), ExternalRef: "nobody"}, Result{StatusPossibleMatch, unknownRef}},
		"a subject with no name":     {Subject{Kind: SubjectParty, ID: ids.New(), ExternalRef: "blank"}, Result{StatusPossibleMatch, unknownRef}},
		"a listed document, no name": {Subject{Kind: SubjectParty, ID: ids.New(), ExternalRef: "nameless"}, Result{StatusPossibleMatch, "ofac_sdn:4108=document"}},
		"anyone else":                {Subject{Kind: SubjectParty, ID: ids.New(), ExternalRef: "onboarding-3"}, Result{StatusClear, clearRef}},
	} {
		if got, err := l.Screen(t.Context(), tc.subject); err != nil || got != tc.want {
			t.Errorf("%s = %+v (%v), want %+v", name, got, err, tc.want)
		}
	}
}

// TestListScreeningFailsClosed (AD-3, ADR-0018): lists older than their maximum age are an error, which the
// Screener turns into a pending case, never a clear one; stale PEPs hold up a party, not a beneficiary, which is
// not screened against them.
func TestListScreeningFailsClosed(t *testing.T) {
	if _, err := screening(49*time.Hour).Screen(t.Context(), Subject{Kind: SubjectParty, ExternalRef: "onboarding-3"}); !errors.Is(err, ErrStaleLists) {
		t.Errorf("screening against stale lists = %v, want ErrStaleLists", err)
	}
	l := screening(time.Hour)
	current, _ := l.Lists.Current()
	current.Refreshed[ListPEP] = current.Refreshed[ListPEP].Add(-48 * time.Hour)
	if _, err := l.Screen(t.Context(), Subject{Kind: SubjectParty, ExternalRef: "onboarding-3"}); !errors.Is(err, ErrStaleLists) {
		t.Errorf("a party with stale PEPs = %v, want ErrStaleLists", err)
	}
	if got, err := l.Screen(t.Context(), Subject{Kind: SubjectAccount, ExternalRef: "onboarding-3"}); err != nil || got.Status != StatusClear {
		t.Errorf("a beneficiary with stale PEPs = %+v (%v), want it screened", got, err)
	}
}

// TestStalePEPsDoNotOpenTheCircuitForBeneficiaries (AD-3, ADR-0018): parties held up by stale PEPs wait, without
// counting as the vendor failing, so that a beneficiary, screened against fresh sanctions, is still cleared.
func TestStalePEPsDoNotOpenTheCircuitForBeneficiaries(t *testing.T) {
	l := screening(time.Hour)
	current, _ := l.Lists.Current()
	current.Refreshed[ListPEP] = current.Refreshed[ListPEP].Add(-48 * time.Hour)
	s := NewScreener(l, l.Now)

	for range 2 * failuresToOpen {
		if o, _ := s.Screen(t.Context(), Subject{Kind: SubjectParty, ExternalRef: "onboarding-3"}); o != OutcomePending {
			t.Fatalf("a party with stale PEPs = %s, want it pending", o)
		}
	}
	if o, ref := s.Screen(t.Context(), Subject{Kind: SubjectAccount, ExternalRef: "onboarding-3"}); o != OutcomeClear || ref != clearRef {
		t.Fatalf("a beneficiary after them = %s (%q), want it cleared", o, ref)
	}
}

// TestACaseNamesTheEntriesThatFit: a reference names the entries matched, in order, as many as the case keeps, and
// how many more there are.
func TestACaseNamesTheEntriesThatFit(t *testing.T) {
	hits := make([]matching.Hit, 40)
	for i := range hits {
		hits[i] = matching.Hit{List: ListOFAC, ID: fmt.Sprintf("%05d", i), Score: 0.95}
	}
	if got := refOf(hits[:2]); got != "ofac_sdn:00000~0.95 ofac_sdn:00001~0.95" {
		t.Errorf("two hits = %q", got)
	}
	if got := refOf(hits); len(got) > maxRef || !strings.HasPrefix(got, "ofac_sdn:00000~0.95 ") || !strings.HasSuffix(got, " more") {
		t.Errorf("forty hits = %q (%d characters), want those that fit and how many more", got, len(got))
	}
}

// fakeStore is a ListStore in memory, recording what it was asked to do.
type fakeStore struct {
	versions map[string]string
	entries  map[string]int
	did      []string
}

func (f *fakeStore) Stored(_ context.Context, list string) (string, int, error) {
	return f.versions[list], f.entries[list], nil
}

func (f *fakeStore) Replace(_ context.Context, list, version string, entries []matching.Entry) error {
	f.versions[list], f.entries[list] = version, len(entries)
	f.did = append(f.did, "replace "+list)
	return nil
}

func (f *fakeStore) Touch(_ context.Context, list string) error {
	f.did = append(f.did, "touch "+list)
	return nil
}

// TestListsAreRefreshedOneByOne (ADR-0018): a list that changed is replaced, one that did not only marks as
// refreshed; one that fails, comes empty or with less than half its entries keeps what it had while the others are
// refreshed, and is the error.
func TestListsAreRefreshedOneByOne(t *testing.T) {
	store := &fakeStore{versions: map[string]string{ListOFAC: "v1", ListUN: "v1", "shrunk": "v1"}, entries: map[string]int{"shrunk": 10}}
	one := []matching.Entry{{List: "x", ID: "1", Names: []string{"A"}}}
	source := func(list, version string, err error, entries []matching.Entry) Source {
		return Source{List: list, Fetch: func(context.Context) (string, []matching.Entry, error) { return version, entries, err }}
	}
	down := errors.New("the source is down")

	err := RefreshLists(t.Context(), store, []Source{
		source(ListOFAC, "v2", nil, one), source(ListUN, "v1", nil, one), source(ListPEP, "", down, nil), source("empty", "v1", nil, nil),
		source("shrunk", "v2", nil, one),
	})

	if want := []string{"replace " + ListOFAC, "touch " + ListUN}; len(store.did) != 2 || store.did[0] != want[0] || store.did[1] != want[1] {
		t.Errorf("did %v, want %v", store.did, want)
	}
	if !errors.Is(err, down) || !errors.Is(err, ErrEmptyList) || !errors.Is(err, ErrShrunkList) {
		t.Errorf("refresh = %v, want the failed, the empty and the shrunk sources' errors", err)
	}
}
