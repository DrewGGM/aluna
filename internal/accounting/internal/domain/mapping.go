// Package domain holds accounting's rules: how an internal account maps to a CUIF code on a day (ADR-0011).
package domain

import (
	"bytes"
	"encoding/csv"
	"errors"
	"fmt"
	"io"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/DrewGGM/aluna/internal/calendar"
)

var (
	// ErrInvalidMapping means a mapping or a file of them that is not valid.
	ErrInvalidMapping = errors.New("invalid CUIF mapping")
	// ErrMappingOverlap means two mappings of a pattern hold on some day.
	ErrMappingOverlap = errors.New("another mapping of the pattern holds in that period")
	// ErrNoMapping means no mapping in force covers an account.
	ErrNoMapping = errors.New("no CUIF mapping in force")
)

var (
	validPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._/-]{0,63}$`)
	// A CUIF code has 6 digits today and 10 from 2028 (ADR-0011).
	validCode = regexp.MustCompile(`^[0-9]{6,10}$`)
)

// mappingColumns are the columns of a file of mappings, its header.
var mappingColumns = []string{"pattern", "cuif_code", "valid_from", "valid_to"}

// maxRows is the most mappings a file has: checking them against each other is quadratic.
const maxRows = 5000

// latestEnd bounds valid_to: a mapping is never shortened, so a typo in its end would hold the pattern for good.
var latestEnd = time.Date(2200, 1, 1, 0, 0, 0, 0, time.UTC)

// Mapping says that the internal accounts whose code starts with Pattern are CUIFCode from ValidFrom (included)
// to ValidTo (excluded).
type Mapping struct {
	Pattern, CUIFCode  string
	ValidFrom, ValidTo time.Time
}

func (m Mapping) holdsOn(day time.Time) bool {
	return !day.Before(m.ValidFrom) && day.Before(m.ValidTo)
}

func (m Mapping) overlaps(o Mapping) bool {
	return m.Pattern == o.Pattern && m.ValidFrom.Before(o.ValidTo) && o.ValidFrom.Before(m.ValidTo)
}

// Resolve is the CUIF code of the internal account with code account on day: that of the longest pattern in force
// that starts the code. ErrNoMapping if none does. The result does not depend on the order of mappings: two
// mappings of a pattern do not hold on the same day.
func Resolve(mappings []Mapping, account string, day calendar.Date) (string, error) {
	at := time.Date(day.Year, day.Month, day.Day, 0, 0, 0, 0, time.UTC)
	var best Mapping
	for _, m := range mappings {
		if m.holdsOn(at) && strings.HasPrefix(account, m.Pattern) && len(m.Pattern) > len(best.Pattern) {
			best = m
		}
	}
	if best.Pattern == "" {
		return "", fmt.Errorf("%w: account %s on %s", ErrNoMapping, account, at.Format(time.DateOnly))
	}
	return best.CUIFCode, nil
}

// CheckAgainst reports whether mappings can join stored: ErrMappingOverlap if one overlaps one of its pattern.
func CheckAgainst(stored, mappings []Mapping) error {
	for _, m := range mappings {
		if slices.ContainsFunc(stored, m.overlaps) {
			return fmt.Errorf("%w: pattern %s", ErrMappingOverlap, m.Pattern)
		}
	}
	return nil
}

// CheckStart reports whether mappings start no earlier than the entity's business date: a mapping that started
// before would change the CUIF code of days already closed and reported. ErrInvalidMapping otherwise.
func CheckStart(mappings []Mapping, businessDate calendar.Date) error {
	today := time.Date(businessDate.Year, businessDate.Month, businessDate.Day, 0, 0, 0, 0, time.UTC)
	for _, m := range mappings {
		if m.ValidFrom.Before(today) {
			return fmt.Errorf("%w: pattern %s starts on %s, before the business date %s", ErrInvalidMapping, m.Pattern,
				m.ValidFrom.Format(time.DateOnly), today.Format(time.DateOnly))
		}
	}
	return nil
}

// bom is the mark some spreadsheets write at the start of a UTF-8 file.
var bom = []byte("\uFEFF")

// ParseMappings reads a CSV file of mappings, with its header, and checks them: ErrInvalidMapping if a row is not
// valid, and ErrMappingOverlap if two of them overlap. Errors name the line of the file.
func ParseMappings(file io.Reader) ([]Mapping, error) {
	data, err := io.ReadAll(file)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrInvalidMapping, err)
	}
	r := csv.NewReader(bytes.NewReader(bytes.TrimPrefix(data, bom)))
	if header, err := r.Read(); err != nil || !slices.Equal(header, mappingColumns) {
		return nil, fmt.Errorf("%w: a file starts with the header %s", ErrInvalidMapping, strings.Join(mappingColumns, ","))
	}
	var mappings []Mapping
	for {
		m, err := readMapping(r)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, err
		}
		line, _ := r.FieldPos(0)
		if len(mappings) == maxRows {
			return nil, fmt.Errorf("%w: a file has up to %d rows", ErrInvalidMapping, maxRows)
		}
		if slices.ContainsFunc(mappings, m.overlaps) {
			return nil, fmt.Errorf("%w: line %d, pattern %s", ErrMappingOverlap, line, m.Pattern)
		}
		mappings = append(mappings, m)
	}
	if len(mappings) == 0 {
		return nil, fmt.Errorf("%w: a file has at least one row", ErrInvalidMapping)
	}
	return mappings, nil
}

// readMapping reads the next row of r as a mapping: io.EOF at the end of the file.
func readMapping(r *csv.Reader) (Mapping, error) {
	row, err := r.Read()
	if errors.Is(err, io.EOF) {
		return Mapping{}, err
	}
	if err != nil {
		return Mapping{}, fmt.Errorf("%w: %w", ErrInvalidMapping, err)
	}
	m, err := parseMapping(row)
	if err != nil {
		line, _ := r.FieldPos(0)
		return Mapping{}, fmt.Errorf("line %d: %w", line, err)
	}
	return m, nil
}

func parseMapping(row []string) (Mapping, error) {
	// The parse error is not wrapped: it repeats the field, of any length, and the message reaches the client.
	from, err := time.Parse(time.DateOnly, row[2])
	if err != nil {
		return Mapping{}, fmt.Errorf("%w: valid_from is a date yyyy-mm-dd", ErrInvalidMapping)
	}
	to, err := time.Parse(time.DateOnly, row[3])
	if err != nil {
		return Mapping{}, fmt.Errorf("%w: valid_to is a date yyyy-mm-dd", ErrInvalidMapping)
	}
	switch {
	case !validPattern.MatchString(row[0]):
		return Mapping{}, fmt.Errorf("%w: a pattern is the start of an internal code, of up to 64 characters", ErrInvalidMapping)
	case !validCode.MatchString(row[1]):
		return Mapping{}, fmt.Errorf("%w: a CUIF code has 6 to 10 digits", ErrInvalidMapping)
	case !from.Before(to):
		return Mapping{}, fmt.Errorf("%w: valid_from comes before valid_to", ErrInvalidMapping)
	case !to.Before(latestEnd):
		return Mapping{}, fmt.Errorf("%w: valid_to is before %s", ErrInvalidMapping, latestEnd.Format(time.DateOnly))
	}
	return Mapping{Pattern: row[0], CUIFCode: row[1], ValidFrom: from, ValidTo: to}, nil
}
