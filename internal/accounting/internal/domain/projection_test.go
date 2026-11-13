package domain_test

import (
	"errors"
	"slices"
	"testing"

	"github.com/DrewGGM/aluna/internal/accounting/internal/domain"
	"github.com/DrewGGM/aluna/internal/money"
)

func amount(t *testing.T, units int64, code string) money.Amount {
	t.Helper()
	c, err := money.CurrencyOf(code)
	if err != nil {
		t.Fatal(err)
	}
	return money.New(units, c)
}

func total(t *testing.T, account string, debits, credits int64, currency string) domain.AccountTotal {
	t.Helper()
	return domain.AccountTotal{Account: account, Debits: amount(t, debits, currency), Credits: amount(t, credits, currency)}
}

// line is a GL line as these tests compare it.
type line struct {
	code, currency  string
	debits, credits int64
}

func lines(gl []domain.Line) []line {
	out := make([]line, len(gl))
	for i, l := range gl {
		out[i] = line{l.CUIFCode, l.Debits.Currency().Code(), l.Debits.Units(), l.Credits.Units()}
	}
	return out
}

// TestADayAddsUpByCUIFCode (FR-36, US-13): accounts of a code add up into its line, by currency, and the lines come
// in the order of their code and currency; debits and credits stay apart, so the day sums zero.
func TestADayAddsUpByCUIFCode(t *testing.T) {
	mappings := []domain.Mapping{
		mapping(t, "deposits/", "210505", "2026-01-01", "2027-01-01"),
		mapping(t, "A.CASH.", "110505", "2026-01-01", "2027-01-01"),
		mapping(t, "I.FEES.", "416500", "2026-01-01", "2027-01-01"),
	}
	totals := []domain.AccountTotal{
		total(t, "deposits/a", 10_00, 100_00, "COP"),
		total(t, "deposits/b", 0, 30_00, "COP"),
		total(t, "A.CASH.VAULT", 130_00, 10_00, "COP"),
		total(t, "I.FEES.SAVINGS", 0, 0, "COP"),
		total(t, "deposits/c", 0, 5_00, "USD"),
		total(t, "A.CASH.USD", 5_00, 0, "USD"),
	}

	got, err := domain.Project(totals, mappings, date(t, "2026-03-02"))
	if err != nil {
		t.Fatal(err)
	}

	want := []line{
		{"110505", "COP", 130_00, 10_00},
		{"110505", "USD", 5_00, 0},
		{"210505", "COP", 10_00, 130_00},
		{"210505", "USD", 0, 5_00},
		{"416500", "COP", 0, 0},
	}
	if !slices.Equal(lines(got), want) {
		t.Fatalf("lines = %v, want %v", lines(got), want)
	}
}

// TestADayWithAnUnmappedAccountIsNotProjected: half a day would not sum zero.
func TestADayWithAnUnmappedAccountIsNotProjected(t *testing.T) {
	mappings := []domain.Mapping{mapping(t, "A.", "110505", "2026-01-01", "2027-01-01")}
	_, err := domain.Project([]domain.AccountTotal{total(t, "A.CASH", 1, 0, "COP"), total(t, "L.OTHER", 0, 1, "COP")}, mappings, date(t, "2026-03-02"))
	if !errors.Is(err, domain.ErrNoMapping) {
		t.Fatalf("Project = %v, want ErrNoMapping", err)
	}
}

// TestTheMappingIsTheOneOfTheDay: the same account is another code on another day.
func TestTheMappingIsTheOneOfTheDay(t *testing.T) {
	mappings := []domain.Mapping{
		mapping(t, "A.", "110505", "2026-01-01", "2028-01-01"),
		mapping(t, "A.", "1105050001", "2028-01-01", "2029-01-01"),
	}
	got, err := domain.Project([]domain.AccountTotal{total(t, "A.CASH", 1, 1, "COP")}, mappings, date(t, "2028-01-01"))
	if err != nil || len(got) != 1 || got[0].CUIFCode != "1105050001" {
		t.Fatalf("lines = %v, %v", lines(got), err)
	}
}
