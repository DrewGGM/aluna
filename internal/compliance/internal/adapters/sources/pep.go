package sources

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"

	"github.com/DrewGGM/aluna/internal/compliance/internal/app"
	"github.com/DrewGGM/aluna/internal/compliance/internal/domain/matching"
)

// pepPage is how many PEPs a request reads, and maxPEPPages how many pages a list may have: Socrata pages its
// datasets, and the list has some 38,000 posts, so a source that pages forever (a mirror that ignores $offset)
// is an error, not a download that runs until the job's limit.
const (
	pepPage     = 20000
	maxPEPPages = 10
)

var (
	// errTooManyPages means a PEP source that did not end within maxPEPPages pages.
	errTooManyPages = errors.New("the PEP list did not end")
	// errNoRowID means a PEP source that does not give each post its row id (a mirror that ignores $select).
	errNoRowID = errors.New("a PEP post has no row id")
)

// PEP is the list of politically exposed persons of Decreto 830 de 2021, as Función Pública publishes it in
// datos.gov.co: the Socrata resource at resource (https://www.datos.gov.co/resource/3qxn-uc22.json, its query
// kept), read page by page through client until a page comes short of posts. An entry's id is the row id Socrata
// gives the person's first post, never the person's document: a case names the entry, and its reference goes
// into the case and its events, where no document may go.
func PEP(client *http.Client, resource string) app.Source {
	return app.Source{List: app.ListPEP, Fetch: func(ctx context.Context) (string, []matching.Entry, error) {
		return fetchPEP(ctx, client, resource, pepPage)
	}}
}

// fetchPEP reads the PEPs of resource in pages of size.
func fetchPEP(ctx context.Context, client *http.Client, resource string, size int) (string, []matching.Entry, error) {
	base, err := url.Parse(resource)
	if err != nil {
		return "", nil, fmt.Errorf("%w: its URL does not parse", errDownload)
	}
	read := sha256.New()
	var entries []matching.Entry
	for page := range maxPEPPages {
		query := base.Query()
		query.Set("$select", ":id,numero_documento,nombre_pep")
		query.Set("$order", ":id")
		query.Set("$limit", strconv.Itoa(size))
		query.Set("$offset", strconv.Itoa(page*size))
		target := *base
		target.RawQuery = query.Encode()
		body, err := download(ctx, client, target.String())
		if err != nil {
			return "", nil, err
		}
		posts, rows, err := parsePEP(body)
		if err != nil {
			return "", nil, err
		}
		read.Write(body)
		entries = append(entries, posts...)
		if rows < size {
			return version(read), dedupe(entries), nil
		}
	}
	return "", nil, fmt.Errorf("%w: over %d pages of %d", errTooManyPages, maxPEPPages, size)
}

// parsePEP are the PEPs of page, one entry per post with a document, its id the post's row id, and how many posts
// the page had: Socrata leaves out the fields that are null, so a post without a document is not an entry, but it
// counts for knowing whether the page was full.
func parsePEP(page []byte) ([]matching.Entry, int, error) {
	var posts []struct {
		Row      string `json:":id"`
		Document string `json:"numero_documento"`
		Name     string `json:"nombre_pep"`
	}
	if err := json.Unmarshal(page, &posts); err != nil {
		return nil, 0, fmt.Errorf("PEP: %w", err)
	}
	entries := make([]matching.Entry, 0, len(posts))
	for _, p := range posts {
		switch {
		case p.Document == "":
		case p.Row == "":
			return nil, 0, errNoRowID
		default:
			entries = append(entries, matching.Entry{List: app.ListPEP, ID: p.Row, Names: []string{p.Name}, Documents: []string{p.Document}})
		}
	}
	return entries, len(posts), nil
}

// dedupe is entries with one entry per person, its first post's, with its posts' names and documents together: a
// person with two posts, the same document however written (matching.DocumentKey), is listed twice. Every post's
// document stays, so a post grouped by a key it shares with another one still matches by all of its own. A document
// without digits ("-", "N/A") groups nothing.
func dedupe(entries []matching.Entry) []matching.Entry {
	byDocument := map[string]int{}
	var people []matching.Entry
	for _, e := range entries {
		key := matching.DocumentKey(e.Documents[0])
		if !strings.ContainsAny(key, "0123456789") {
			people = append(people, e)
			continue
		}
		if i, seen := byDocument[key]; seen {
			people[i].Names = append(people[i].Names, e.Names...)
			for _, d := range e.Documents {
				if !slices.Contains(people[i].Documents, d) {
					people[i].Documents = append(people[i].Documents, d)
				}
			}
			continue
		}
		byDocument[key] = len(people)
		people = append(people, e)
	}
	return people
}
