package sources

import (
	"bytes"
	"context"
	"encoding/csv"
	"fmt"
	"net/http"
	"regexp"
	"slices"
	"strings"

	"github.com/DrewGGM/aluna/internal/compliance/internal/app"
	"github.com/DrewGGM/aluna/internal/compliance/internal/domain/matching"
)

// OFAC is the SDN list of OFAC at sdnURL, with the aliases of altURL, as OFAC publishes them in CSV (SDN.CSV and
// ALT.CSV of its Sanctions List Service), read through client.
func OFAC(client *http.Client, sdnURL, altURL string) app.Source {
	return app.Source{List: app.ListOFAC, Fetch: func(ctx context.Context) (string, []matching.Entry, error) {
		sdn, err := download(ctx, client, sdnURL)
		if err != nil {
			return "", nil, err
		}
		alt, err := download(ctx, client, altURL)
		if err != nil {
			return "", nil, err
		}
		entries, err := parseOFAC(sdn, alt)
		return versionOf(sdn, alt), entries, err
	}}
}

// OFAC's CSV columns, and the value of an empty field.
const (
	sdnNum, sdnName, sdnRemarks = 0, 1, 11
	altNum, altName             = 0, 3
	ofacNull                    = "-0-"
)

var (
	// ofacDocuments are the documents OFAC writes in an entry's remarks that a Colombian subject may carry: its
	// cédula, its NIT (with or without its check digit), its passport or another national id.
	ofacDocuments = regexp.MustCompile(`(?:Cedula No\.|NIT #|Passport|National ID No\.|RUC #|D\.N\.I\.)\s*([A-Z0-9][A-Z0-9.-]*[A-Z0-9])`)
	// ofacAliases are the aliases OFAC writes in the remarks, besides those of ALT.CSV.
	ofacAliases = regexp.MustCompile(`a\.k\.a\. '([^']+)'`)
)

// parseOFAC are the entries of sdn, with the aliases of alt and the documents and aliases of their remarks.
func parseOFAC(sdn, alt []byte) ([]matching.Entry, error) {
	rows, err := csvRows(sdn, sdnRemarks+1)
	if err != nil {
		return nil, fmt.Errorf("SDN.CSV: %w", err)
	}
	entries := make([]matching.Entry, 0, len(rows))
	byNum := map[string]int{}
	for _, row := range rows {
		if field(row[sdnNum]) == "" {
			continue // no entry without its number, which a case names
		}
		e := matching.Entry{List: app.ListOFAC, ID: field(row[sdnNum]), Names: []string{field(row[sdnName])}}
		remarks := field(row[sdnRemarks])
		for _, m := range ofacAliases.FindAllStringSubmatch(remarks, -1) {
			e.Names = append(e.Names, m[1])
		}
		for _, m := range ofacDocuments.FindAllStringSubmatch(remarks, -1) {
			if !slices.Contains(e.Documents, m[1]) {
				e.Documents = append(e.Documents, m[1])
			}
		}
		byNum[e.ID] = len(entries)
		entries = append(entries, e)
	}
	aliases, err := csvRows(alt, altName+1)
	if err != nil {
		return nil, fmt.Errorf("ALT.CSV: %w", err)
	}
	for _, row := range aliases {
		if i, listed := byNum[field(row[altNum])]; listed {
			entries[i].Names = append(entries[i].Names, field(row[altName]))
		}
	}
	return entries, nil
}

// csvRows are the rows of document with at least columns fields: OFAC's files have no header, and a blank last
// line.
func csvRows(document []byte, columns int) ([][]string, error) {
	r := csv.NewReader(bytes.NewReader(document))
	r.FieldsPerRecord, r.LazyQuotes = -1, true
	rows, err := r.ReadAll()
	if err != nil {
		return nil, err
	}
	kept := rows[:0]
	for _, row := range rows {
		if len(row) >= columns {
			kept = append(kept, row)
		}
	}
	return kept, nil
}

// field is an OFAC field, trimmed, with its empty value as "".
func field(s string) string {
	s = strings.TrimSpace(s)
	if s == ofacNull {
		return ""
	}
	return s
}
