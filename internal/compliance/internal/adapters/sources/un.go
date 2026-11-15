package sources

import (
	"context"
	"encoding/xml"
	"fmt"
	"net/http"
	"strings"

	"github.com/DrewGGM/aluna/internal/compliance/internal/app"
	"github.com/DrewGGM/aluna/internal/compliance/internal/domain/matching"
)

// UN is the consolidated list of the UN Security Council at url, in its XML (consolidated.xml), read through
// client: the list a Colombian entity must screen against (Ley 1121 de 2006, art. 20).
func UN(client *http.Client, url string) app.Source {
	return app.Source{List: app.ListUN, Fetch: func(ctx context.Context) (string, []matching.Entry, error) {
		document, err := download(ctx, client, url)
		if err != nil {
			return "", nil, err
		}
		entries, err := parseUN(document)
		return versionOf(document), entries, err
	}}
}

// unAliases are the aliases of an individual or an entity.
type unAliases []struct {
	Name string `xml:"ALIAS_NAME"`
}

// unList is the part of the consolidated list screening reads.
type unList struct {
	Individuals []struct {
		ID        string    `xml:"DATAID"`
		First     string    `xml:"FIRST_NAME"`
		Second    string    `xml:"SECOND_NAME"`
		Third     string    `xml:"THIRD_NAME"`
		Fourth    string    `xml:"FOURTH_NAME"`
		Aliases   unAliases `xml:"INDIVIDUAL_ALIAS"`
		Documents []struct {
			Number string `xml:"NUMBER"`
		} `xml:"INDIVIDUAL_DOCUMENT"`
	} `xml:"INDIVIDUALS>INDIVIDUAL"`
	Entities []struct {
		ID      string    `xml:"DATAID"`
		Name    string    `xml:"FIRST_NAME"`
		Aliases unAliases `xml:"ENTITY_ALIAS"`
	} `xml:"ENTITIES>ENTITY"`
}

// parseUN are the individuals and entities of document, with their aliases and, of an individual, its documents.
func parseUN(document []byte) ([]matching.Entry, error) {
	var list unList
	if err := xml.Unmarshal(document, &list); err != nil {
		return nil, fmt.Errorf("consolidated.xml: %w", err)
	}
	entries := make([]matching.Entry, 0, len(list.Individuals)+len(list.Entities))
	for _, in := range list.Individuals {
		name := strings.Join([]string{in.First, in.Second, in.Third, in.Fourth}, " ")
		e := matching.Entry{List: app.ListUN, ID: in.ID, Names: append([]string{name}, in.Aliases.names()...)}
		for _, d := range in.Documents {
			if d.Number != "" {
				e.Documents = append(e.Documents, d.Number)
			}
		}
		entries = append(entries, e)
	}
	for _, en := range list.Entities {
		entries = append(entries, matching.Entry{List: app.ListUN, ID: en.ID, Names: append([]string{en.Name}, en.Aliases.names()...)})
	}
	return entries, nil
}

// names are the aliases that have a name.
func (a unAliases) names() []string {
	var names []string
	for _, alias := range a {
		if alias.Name != "" {
			names = append(names, alias.Name)
		}
	}
	return names
}
