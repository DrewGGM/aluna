package domain_test

import (
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/DrewGGM/aluna/internal/accounting/internal/domain"
	"github.com/DrewGGM/aluna/internal/calendar"
)

func day(t *testing.T, s string) time.Time {
	t.Helper()
	d, err := time.Parse(time.DateOnly, s)
	if err != nil {
		t.Fatal(err)
	}
	return d
}

func date(t *testing.T, s string) calendar.Date {
	t.Helper()
	d := day(t, s)
	return calendar.Date{Year: d.Year(), Month: d.Month(), Day: d.Day()}
}

func mapping(t *testing.T, pattern, code, from, to string) domain.Mapping {
	t.Helper()
	return domain.Mapping{Pattern: pattern, CUIFCode: code, ValidFrom: day(t, from), ValidTo: day(t, to)}
}

// TestTheMostSpecificMappingInForceWins (FR-36): the longest prefix that holds on the day, whatever the order they
// come in.
func TestTheMostSpecificMappingInForceWins(t *testing.T) {
	mappings := []domain.Mapping{
		mapping(t, "L.", "210000", "2026-01-01", "2029-01-01"),
		mapping(t, "L.TAX.", "250000", "2026-01-01", "2029-01-01"),
		mapping(t, "L.TAX.GMF", "251500", "2026-01-01", "2028-01-01"),
		mapping(t, "L.TAX.GMF", "2515000001", "2028-01-01", "2029-01-01"),
	}
	for _, c := range []struct{ account, on, want string }{
		{"L.TAX.GMF", "2026-03-02", "251500"},
		{"L.TAX.GMF", "2027-12-31", "251500"},
		{"L.TAX.GMF", "2028-01-01", "2515000001"},
		{"L.TAX.ICA", "2026-03-02", "250000"},
		{"L.OTHER", "2026-03-02", "210000"},
	} {
		slices.Reverse(mappings)
		got, err := domain.Resolve(mappings, c.account, date(t, c.on))
		if err != nil || got != c.want {
			t.Errorf("%s on %s = %s, %v, want %s", c.account, c.on, got, err, c.want)
		}
	}
}

// TestWithoutAMappingInForceThereIsNoCode: nothing is guessed, before its start or from its end, which is excluded.
func TestWithoutAMappingInForceThereIsNoCode(t *testing.T) {
	mappings := []domain.Mapping{mapping(t, "L.TAX.", "250000", "2026-01-01", "2027-01-01")}
	for _, c := range []struct{ account, on string }{
		{"L.OTHER", "2026-03-02"},
		{"L.TAX.GMF", "2025-12-31"},
		{"L.TAX.GMF", "2027-01-01"},
		{"L.TA", "2026-03-02"},
	} {
		if got, err := domain.Resolve(mappings, c.account, date(t, c.on)); !errors.Is(err, domain.ErrNoMapping) {
			t.Errorf("%s on %s = %s, %v, want ErrNoMapping", c.account, c.on, got, err)
		}
	}
}

func TestAFileOfMappingsIsRead(t *testing.T) {
	got, err := domain.ParseMappings(strings.NewReader("pattern,cuif_code,valid_from,valid_to\n" +
		"L.TAX.GMF,251500,2026-01-01,2028-01-01\ndeposits/,210505,2026-01-01,2028-01-01\n"))
	if err != nil {
		t.Fatal(err)
	}
	want := []domain.Mapping{mapping(t, "L.TAX.GMF", "251500", "2026-01-01", "2028-01-01"), mapping(t, "deposits/", "210505", "2026-01-01", "2028-01-01")}
	if !slices.Equal(got, want) {
		t.Fatalf("mappings = %+v, want %+v", got, want)
	}
}

func TestAnInvalidFileIsRejected(t *testing.T) {
	const header = "pattern,cuif_code,valid_from,valid_to\n"
	for name, file := range map[string]string{
		"empty":              "",
		"only the header":    header,
		"another header":     "code,cuif,from,to\nL.,251500,2026-01-01,2028-01-01\n",
		"a short row":        header + "L.,251500,2026-01-01\n",
		"a blank pattern":    header + ",251500,2026-01-01,2028-01-01\n",
		"a long pattern":     header + strings.Repeat("L", 65) + ",251500,2026-01-01,2028-01-01\n",
		"a NUL pattern":      header + "L\x00,251500,2026-01-01,2028-01-01\n",
		"a short code":       header + "L.,25150,2026-01-01,2028-01-01\n",
		"a long code":        header + "L.,25150000001,2026-01-01,2028-01-01\n",
		"letters in a code":  header + "L.,25150A,2026-01-01,2028-01-01\n",
		"another date":       header + "L.,251500,01/01/2026,2028-01-01\n",
		"no end":             header + "L.,251500,2026-01-01,\n",
		"an empty period":    header + "L.,251500,2026-01-01,2026-01-01\n",
		"a backwards period": header + "L.,251500,2027-01-01,2026-01-01\n",
		"an end too far":     header + "L.,251500,2027-01-01,2200-01-01\n",
		"a stray quote":      header + "L.,25\"1500,2026-01-01,2027-01-01\n",
		"overlapping rows":   header + "L.,251500,2026-01-01,2027-01-01\nL.,251501,2026-06-01,2028-01-01\n",
		"too many rows":      header + manyRows(5001),
	} {
		if got, err := domain.ParseMappings(strings.NewReader(file)); !errors.Is(err, domain.ErrInvalidMapping) && !errors.Is(err, domain.ErrMappingOverlap) {
			t.Errorf("%s: %+v, %v, want it rejected", name, got, err)
		}
	}
}

// TestTheLimitsOfAMappingAreIn: the longest pattern and code are valid.
func TestTheLimitsOfAMappingAreIn(t *testing.T) {
	file := "pattern,cuif_code,valid_from,valid_to\n" + strings.Repeat("L", 64) + ",2515000001,2026-01-01,2028-01-01\n"
	if _, err := domain.ParseMappings(strings.NewReader(file)); err != nil {
		t.Fatal(err)
	}
}

// TestPeriodsThatOnlyTouchDoNotOverlap: the end of one is the start of the next, and another pattern is another
// mapping.
func TestPeriodsThatOnlyTouchDoNotOverlap(t *testing.T) {
	_, err := domain.ParseMappings(strings.NewReader("pattern,cuif_code,valid_from,valid_to\n" +
		"L.,251500,2026-01-01,2027-01-01\nL.,251501,2027-01-01,2028-01-01\nL.TAX.,251502,2026-01-01,2028-01-01\n"))
	if err != nil {
		t.Fatal(err)
	}
}

// manyRows are n rows of different patterns.
func manyRows(n int) string {
	var rows strings.Builder
	for i := range n {
		fmt.Fprintf(&rows, "P%d,251500,2026-01-01,2027-01-01\n", i)
	}
	return rows.String()
}

func TestAFileOfTheMostRowsIsRead(t *testing.T) {
	file := "pattern,cuif_code,valid_from,valid_to\n" + manyRows(5000)
	if got, err := domain.ParseMappings(strings.NewReader(file)); err != nil || len(got) != 5000 {
		t.Fatalf("%d mappings, %v", len(got), err)
	}
}

func TestAMappingIsCheckedAgainstTheStoredOnes(t *testing.T) {
	stored := []domain.Mapping{mapping(t, "L.", "210000", "2026-01-01", "2027-01-01")}
	if err := domain.CheckAgainst(stored, []domain.Mapping{mapping(t, "L.", "210001", "2026-06-01", "2028-01-01")}); !errors.Is(err, domain.ErrMappingOverlap) {
		t.Errorf("an overlap: %v", err)
	}
	for _, fine := range []domain.Mapping{mapping(t, "L.", "210001", "2027-01-01", "2028-01-01"), mapping(t, "A.", "110000", "2026-01-01", "2027-01-01")} {
		if err := domain.CheckAgainst(stored, []domain.Mapping{fine}); err != nil {
			t.Errorf("%+v: %v", fine, err)
		}
	}
}

// TestAFileFromASpreadsheetIsRead: the mark some spreadsheets put at the start, and their line endings, are fine.
func TestAFileFromASpreadsheetIsRead(t *testing.T) {
	got, err := domain.ParseMappings(strings.NewReader("\uFEFFpattern,cuif_code,valid_from,valid_to\r\nL.,251500,2026-01-01,2199-12-31\r\n"))
	if err != nil || len(got) != 1 || got[0].CUIFCode != "251500" {
		t.Fatalf("%+v, %v", got, err)
	}
}

// TestAnErrorNamesItsLine: the line of the file, which blank lines do not change.
func TestAnErrorNamesItsLine(t *testing.T) {
	for want, file := range map[string]string{
		"line 4":             "pattern,cuif_code,valid_from,valid_to\n\nL.,251500,2026-01-01,2027-01-01\nA.,1,2026-01-01,2027-01-01\n",
		"line 3, pattern L.": "pattern,cuif_code,valid_from,valid_to\nL.,251500,2026-01-01,2027-01-01\nL.,251501,2026-06-01,2028-01-01\n",
	} {
		if _, err := domain.ParseMappings(strings.NewReader(file)); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%v, want %q", err, want)
		}
	}
}

// TestAnErrorDoesNotRepeatTheField: the message reaches the client, and a field can be of any length.
func TestAnErrorDoesNotRepeatTheField(t *testing.T) {
	field := strings.Repeat("x", 300)
	for _, row := range []string{"L.,251500," + field + ",2027-01-01", "L.,251500,2026-01-01," + field} {
		_, err := domain.ParseMappings(strings.NewReader("pattern,cuif_code,valid_from,valid_to\n" + row + "\n"))
		if err == nil || strings.Contains(err.Error(), field) {
			t.Errorf("%v, want an error without the field", err)
		}
	}
}

// TestAMappingStartsOnTheBusinessDateOrLater: one that started before would change the code of closed days.
func TestAMappingStartsOnTheBusinessDateOrLater(t *testing.T) {
	today := date(t, "2026-03-02")
	if err := domain.CheckStart([]domain.Mapping{mapping(t, "L.", "210000", "2026-03-02", "2027-01-01")}, today); err != nil {
		t.Errorf("from the business date: %v", err)
	}
	if err := domain.CheckStart([]domain.Mapping{mapping(t, "L.", "210000", "2026-03-01", "2027-01-01")}, today); !errors.Is(err, domain.ErrInvalidMapping) {
		t.Errorf("from the day before: %v, want ErrInvalidMapping", err)
	}
}
