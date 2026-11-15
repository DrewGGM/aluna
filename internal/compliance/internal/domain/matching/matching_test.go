package matching

import (
	"math"
	"slices"
	"testing"
)

// TestNamesCompareAsScreeningDoes: a name compares in capitals, without accents (written with their letter or
// apart from it), punctuation or Spanish particles, its words in order; a document by its letters and digits, and a
// NIT with and without its check digit.
func TestNamesCompareAsScreeningDoes(t *testing.T) {
	for name, want := range map[string][]string{
		"RODRIGUEZ OREJUELA, Miguel Angel": {"ANGEL", "MIGUEL", "OREJUELA", "RODRIGUEZ"},
		"Miguel Ángel Rodríguez Orejuela":  {"ANGEL", "MIGUEL", "OREJUELA", "RODRIGUEZ"},
		"Iñigo Muñoz Pérez":             {"INIGO", "MUNOZ", "PEREZ"}, // decomposed: each accent apart
		"María de los Ángeles de la Torre": {"ANGELES", "MARIA", "TORRE"},
		"PEÑA y LILLO S.A.":                {"A", "LILLO", "PENA", "S"},
		"  ":                               nil,
	} {
		if got := Words(name); !slices.Equal(got, want) {
			t.Errorf("Words(%q) = %v, want %v", name, got, want)
		}
	}
	for number, want := range map[string][]string{
		"16.215.230-1": {"162152301", "16215230"}, "8300720300": {"8300720300", "830072030"}, "1023456752": {"1023456752"},
		"1234567-2": {"12345672", "1234567"}, "1023456752-2": {"10234567522", "1023456752"}, "162152301": {"162152301", "16215230"},
		"830.072.030": {"830072030"}, "AB-123": {"AB123"}, "52111222": {"52111222"}, "-.-": nil,
	} {
		if got := documentKeys(number); !slices.Equal(got, want) {
			t.Errorf("documentKeys(%q) = %v, want %v", number, got, want)
		}
	}
}

// TestJaroWinklerIsTheKnownSimilarity: the textbook pairs give the textbook values.
func TestJaroWinklerIsTheKnownSimilarity(t *testing.T) {
	for _, tc := range []struct {
		a, b string
		want float64
	}{
		{"MARTHA", "MARHTA", 0.961},
		{"DWAYNE", "DUANE", 0.840},
		{"DIXON", "DICKSONX", 0.813},
		{"SAME", "SAME", 1},
		{"ABC", "", 0},
	} {
		if got := jaroWinkler(tc.a, tc.b); math.Abs(got-tc.want) > 0.001 {
			t.Errorf("jaroWinkler(%s, %s) = %.3f, want %.3f", tc.a, tc.b, got, tc.want)
		}
	}
}

// TestASubjectMatchesItsEntry (FR-42): a subject matches an entry by its document, or by a name close to one of the
// entry's, the alias included, in any order and with a typo, or with fewer words; a different person does not, nor
// an entry of a list left out.
func TestASubjectMatchesItsEntry(t *testing.T) {
	x := NewIndex([]Entry{
		{List: "ofac_sdn", ID: "4108", Names: []string{"RODRIGUEZ OREJUELA, Miguel Angel", "EL SENOR"}, Documents: []string{"6095803"}},
		{List: "pep_co", ID: "row-aaaa-bbbb-cccc", Names: []string{"ANA PEREZ RUIZ"}, Documents: []string{"1000000001"}},
		{List: "ofac_sdn", ID: "9999", Names: []string{"EXPORTADORA EJEMPLO S.A."}, Documents: []string{"8300720300"}},
		{List: "ofac_sdn", ID: "7777", Names: []string{"PEREZ GOMEZ, Jose"}, Documents: []string{"10234567", "10234567-5"}},
		{List: "un_consolidated", ID: "6908", Names: []string{"SESELJ Vojislav"}},
	})
	all := func(string) bool { return true }
	cases := map[string]struct {
		names, documents []string
		include          func(string) bool
		want             []string
	}{
		"by document":                           {nil, []string{"6.095.803"}, all, []string{"4108"}},
		"by a NIT without its check digit":      {nil, []string{"830.072.030"}, all, []string{"9999"}},
		"by name, in any order":                 {[]string{"Miguel Ángel Rodríguez Orejuela"}, nil, all, []string{"4108"}},
		"with a typo":                           {[]string{"Miguel Angel Rodriguez Orejeula"}, nil, all, []string{"4108"}},
		"with fewer words":                      {[]string{"Miguel Rodriguez Orejuela"}, nil, all, []string{"4108"}},
		"by an alias":                           {[]string{"El Señor"}, nil, all, []string{"4108"}},
		"another person":                        {[]string{"Miguel Angel Rojas Ortega"}, []string{"123"}, all, nil},
		"a cédula that looks like a listed NIT": {nil, []string{"1023456752"}, all, nil},
		"by a name with Slavic letters":         {[]string{"Vojislav Šešelj"}, nil, all, []string{"6908"}},
		"a list left out":                       {[]string{"Ana Pérez Ruiz"}, nil, func(l string) bool { return l != "pep_co" }, nil},
	}
	for name, tc := range cases {
		var got []string
		for _, h := range x.Match(tc.names, tc.documents, 0.92, tc.include) {
			got = append(got, h.ID)
		}
		if !slices.Equal(got, tc.want) {
			t.Errorf("%s: hits %v, want %v", name, got, tc.want)
		}
	}
}
