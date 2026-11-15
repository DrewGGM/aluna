package matching

// prefixLen is how much of a word finds its candidates: a name only scores against entries with a word that
// starts as one of its words, as the pre-filter of screening tools does, so that a query does not score every
// name of every list.
const prefixLen = 3

// Index is the entries of the lists, indexed to find a subject's candidates.
type Index struct {
	entries    []indexed
	byPrefix   map[string][]int
	byDocument map[string][]int
}

// indexed is an entry with its names as Words.
type indexed struct {
	entry Entry
	names [][]string
}

// NewIndex indexes entries.
func NewIndex(entries []Entry) *Index {
	x := &Index{byPrefix: map[string][]int{}, byDocument: map[string][]int{}}
	for i, e := range entries {
		in := indexed{entry: e}
		for _, name := range e.Names {
			words := Words(name)
			if len(words) == 0 {
				continue
			}
			in.names = append(in.names, words)
			for _, w := range words {
				x.byPrefix[prefix(w)] = append(x.byPrefix[prefix(w)], i)
			}
		}
		for _, d := range e.Documents {
			for _, key := range documentKeys(d) {
				x.byDocument[key] = append(x.byDocument[key], i)
			}
		}
		x.entries = append(x.entries, in)
	}
	return x
}

func prefix(word string) string { return word[:min(prefixLen, len(word))] }

// Match are the entries, of the lists include takes, that a subject with names and documents matches: by one of
// its documents exactly, or by a name whose score with one of the entry's is at least threshold. Each entry is
// one hit, its best.
func (x *Index) Match(names, documents []string, threshold float64, include func(list string) bool) []Hit {
	best := x.byDocuments(documents, include)
	for _, name := range names {
		words := Words(name)
		for i := range x.candidates(words) {
			if best[i].Document || !include(x.entries[i].entry.List) {
				continue
			}
			if score := x.entries[i].closest(words); score >= threshold && score > best[i].Score {
				best[i] = Hit{List: x.entries[i].entry.List, ID: x.entries[i].entry.ID, Score: score}
			}
		}
	}
	hits := make([]Hit, 0, len(best))
	for _, h := range best {
		hits = append(hits, h)
	}
	return hits
}

// byDocuments are the hits, by entry, of the entries of the lists include takes with one of documents.
func (x *Index) byDocuments(documents []string, include func(list string) bool) map[int]Hit {
	hits := map[int]Hit{}
	for _, d := range documents {
		for _, key := range documentKeys(d) {
			for _, i := range x.byDocument[key] {
				if include(x.entries[i].entry.List) {
					hits[i] = Hit{List: x.entries[i].entry.List, ID: x.entries[i].entry.ID, Document: true, Score: 1}
				}
			}
		}
	}
	return hits
}

// candidates are the entries with a word that starts as one of words.
func (x *Index) candidates(words []string) map[int]bool {
	found := map[int]bool{}
	for _, w := range words {
		for _, i := range x.byPrefix[prefix(w)] {
			found[i] = true
		}
	}
	return found
}

// closest is the best score of words with one of in's names.
func (in indexed) closest(words []string) float64 {
	best := 0.0
	for _, name := range in.names {
		best = max(best, score(words, name))
	}
	return best
}
