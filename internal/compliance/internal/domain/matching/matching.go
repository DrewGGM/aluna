// Package matching decides whether a subject is on a restrictive list (FR-42, ADR-0018): by a document, exactly,
// or by a name close enough to one of an entry's names. Names are compared as industry screening does: in
// capitals, without accents, punctuation or the particles of Spanish names, their words in alphabetical order
// so that "RODRIGUEZ OREJUELA, Miguel Angel" and "Miguel Angel Rodríguez Orejuela" are the same, and scored with
// Jaro-Winkler.
package matching

import (
	"slices"
	"strings"
	"unicode"
)

// Entry is an entry of a list: its id in the list, its names (the first one and its aliases) and the numbers of
// its documents.
type Entry struct {
	List, ID  string
	Names     []string
	Documents []string
}

// Hit is an entry a subject matches, and why: its document, or the score of its closest name.
type Hit struct {
	List, ID string
	Document bool
	Score    float64
}

// particles are words of Spanish names that tell nothing apart: "de la Torre" is "Torre".
var particles = map[string]bool{"DE": true, "DEL": true, "LA": true, "LAS": true, "LOS": true, "Y": true}

// unaccented are the Latin letters with diacritics of the lists' languages, Spanish and those of the sanctioned
// names (Slavic, Turkish, Romanian), as the letters without them.
var unaccented = strings.NewReplacer(
	"Á", "A", "À", "A", "Â", "A", "Ä", "A", "Ã", "A", "Å", "A", "Ā", "A",
	"É", "E", "È", "E", "Ê", "E", "Ë", "E", "Ē", "E",
	"Í", "I", "Ì", "I", "Î", "I", "Ï", "I", "Ī", "I",
	"Ó", "O", "Ò", "O", "Ô", "O", "Ö", "O", "Õ", "O", "Ø", "O", "Ō", "O",
	"Ú", "U", "Ù", "U", "Û", "U", "Ü", "U", "Ū", "U",
	"Ñ", "N", "Ç", "C", "Ý", "Y", "Ÿ", "Y",
	"Ą", "A", "Ă", "A", "Ć", "C", "Č", "C", "Ď", "D", "Đ", "D", "Ę", "E", "Ě", "E", "Ğ", "G", "İ", "I", "Ł", "L", "Ľ", "L",
	"Ń", "N", "Ň", "N", "Ő", "O", "Ř", "R", "Ś", "S", "Š", "S", "Ş", "S", "Ș", "S", "Ť", "T", "Ț", "T", "Ů", "U", "Ű", "U",
	"Ź", "Z", "Ż", "Z", "Ž", "Z", "Æ", "AE", "Œ", "OE", "ß", "SS",
)

// Words are name's words, normalized and in alphabetical order: capitals without accents, letters and digits only,
// without particles. An accent written apart from its letter (Unicode's decomposed form) is dropped, not a break
// between words.
func Words(name string) []string {
	plain := unaccented.Replace(strings.Map(func(r rune) rune {
		if unicode.Is(unicode.Mn, r) {
			return -1
		}
		return r
	}, strings.ToUpper(name)))
	words := strings.FieldsFunc(plain, func(r rune) bool { return !unicode.IsLetter(r) && !unicode.IsDigit(r) })
	words = slices.DeleteFunc(words, func(w string) bool { return particles[w] })
	slices.Sort(words)
	return words
}

// documentKeys are number as documents are compared: capitals and digits only, so that "16.215.230" is
// "16215230"; and, for a NIT with its check digit, the number without it too, so that a NIT matches with and
// without it. A NIT is a number whose last digit is the DIAN check digit of the others, written apart from them
// ("16215230-1"), or run on in a number that is no cédula: of 10 digits from 8 or 9 on, a company's, or of 9 digits
// from 1 to 7 on, a person's (no cédula has 9 digits). A 10-digit number from 1 on is a cédula, whose last digit is
// a check digit for one cédula in eleven: taken as a NIT, it would match another person's number.
func documentKeys(number string) []string {
	key := strings.Map(func(r rune) rune {
		r = unicode.ToUpper(r)
		if r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' {
			return r
		}
		return -1
	}, number)
	switch {
	case key == "":
		return nil
	case isNIT(key, checkDigitApart(number)):
		return []string{key, key[:len(key)-1]}
	default:
		return []string{key}
	}
}

// isNIT reports whether key, a document's capitals and digits, is a NIT with its check digit: apart says whether
// the check digit was written apart from the others.
func isNIT(key string, apart bool) bool {
	if strings.Trim(key, "0123456789") != "" || len(key) < 2 || len(key) > len(nitWeights)+1 {
		return false
	}
	return (apart || noCedula(key)) && checkDigit(key[:len(key)-1]) == key[len(key)-1]
}

// noCedula reports whether digits, a document's, can be no cédula: 10 digits from 8 or 9 on, a company's NIT, or 9
// digits from 1 to 7 on, a person's.
func noCedula(digits string) bool {
	return len(digits) == 10 && (digits[0] == '8' || digits[0] == '9') || len(digits) == 9 && digits[0] >= '1' && digits[0] <= '7'
}

// checkDigitApart reports whether number ends in a digit written apart from what comes before it, as a NIT's
// check digit is ("16215230-1").
func checkDigitApart(number string) bool {
	number = strings.TrimSpace(number)
	n := len(number)
	return n >= 2 && number[n-1] >= '0' && number[n-1] <= '9' && !unicode.IsLetter(rune(number[n-2])) && !unicode.IsDigit(rune(number[n-2]))
}

// nitWeights are the weights of the DIAN check digit, from a NIT's last digit to its first.
var nitWeights = [...]int{3, 7, 13, 17, 19, 23, 29, 37, 41, 43, 47, 53, 59, 67, 71}

// checkDigit is the DIAN check digit of nit, its digits only and fewer than 16.
func checkDigit(nit string) byte {
	sum := 0
	for i := range len(nit) {
		sum += int(nit[len(nit)-1-i]-'0') * nitWeights[i]
	}
	r := sum % 11
	if r > 1 {
		return byte('0' + 11 - r)
	}
	return byte('0' + r)
}

// score is how close name a, as its Words, is to name b, from 0 to 1: the Jaro-Winkler similarity of their words
// in order, or, when one name has at least two words, the average of each of its words' closest one in the other,
// which a name with fewer words than the entry's still scores high on.
func score(a, b []string) float64 {
	whole := jaroWinkler(strings.Join(a, " "), strings.Join(b, " "))
	shorter, longer := a, b
	if len(b) < len(a) {
		shorter, longer = b, a
	}
	if len(shorter) < 2 {
		return whole
	}
	var sum float64
	for _, w := range shorter {
		best := 0.0
		for _, other := range longer {
			best = max(best, jaroWinkler(w, other))
		}
		sum += best
	}
	return max(whole, sum/float64(len(shorter)))
}

// jaroWinkler is the Jaro-Winkler similarity of a and b, from 0 to 1, with the usual prefix scale of 0.1 over
// up to four characters.
func jaroWinkler(a, b string) float64 {
	similarity := jaro(a, b)
	prefix := 0
	for prefix < min(4, len(a), len(b)) && a[prefix] == b[prefix] {
		prefix++
	}
	return similarity + float64(prefix)*0.1*(1-similarity)
}

// jaro is the Jaro similarity of a and b, from 0 to 1, over their bytes: names are compared once normalized, in
// ASCII but for the rare letters unaccented leaves, which compare byte by byte.
func jaro(a, b string) float64 {
	if a == b {
		return 1
	}
	if a == "" || b == "" {
		return 0
	}
	matchedA, matchedB, matches := matched(a, b)
	if matches == 0 {
		return 0
	}
	transpositions, j := 0, 0
	for i := range len(a) {
		if !matchedA[i] {
			continue
		}
		for !matchedB[j] {
			j++
		}
		if a[i] != b[j] {
			transpositions++
		}
		j++
	}
	m := float64(matches)
	return (m/float64(len(a)) + m/float64(len(b)) + (m-float64(transpositions)/2)/m) / 3
}

// matched are the bytes of a and of b that jaro matches, those equal within half the longer's length of each
// other, and how many.
func matched(a, b string) ([]bool, []bool, int) {
	window := max(len(a), len(b))/2 - 1
	matchedA, matchedB := make([]bool, len(a)), make([]bool, len(b))
	matches := 0
	for i := range len(a) {
		for j := max(0, i-window); j < min(len(b), i+window+1); j++ {
			if !matchedB[j] && a[i] == b[j] {
				matchedA[i], matchedB[j] = true, true
				matches++
				break
			}
		}
	}
	return matchedA, matchedB, matches
}
