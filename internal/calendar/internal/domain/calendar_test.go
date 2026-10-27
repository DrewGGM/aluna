package domain

import (
	"fmt"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"pgregory.net/rapid"
)

// officialHolidays son los festivos nacionales de cada año (Ley 51 de 1983 y, desde 2026, Ley 2578 de
// 2026: Nuestra Señora del Rosario de Chiquinquirá), como mes-día. Son 18 festivos hasta 2025 y 19 desde
// 2026; en 2025, 2030 y 2038 el Sagrado Corazón y San Pedro y San Pablo caen el mismo lunes, así que hay
// una fecha menos.
var officialHolidays = map[int]string{
	2025: "01-01 01-06 03-24 04-17 04-18 05-01 06-02 06-23 06-30 07-20 08-07 08-18 10-13 11-03 11-17 12-08 12-25",
	2026: "01-01 01-12 03-23 04-02 04-03 05-01 05-18 06-08 06-15 06-29 07-13 07-20 08-07 08-17 10-12 11-02 11-16 12-08 12-25",
	2027: "01-01 01-11 03-22 03-25 03-26 05-01 05-10 05-31 06-07 07-05 07-12 07-20 08-07 08-16 10-18 11-01 11-15 12-08 12-25",
	2028: "01-01 01-10 03-20 04-13 04-14 05-01 05-29 06-19 06-26 07-03 07-10 07-20 08-07 08-21 10-16 11-06 11-13 12-08 12-25",
	2029: "01-01 01-08 03-19 03-29 03-30 05-01 05-14 06-04 06-11 07-02 07-09 07-20 08-07 08-20 10-15 11-05 11-12 12-08 12-25",
	2030: "01-01 01-07 03-25 04-18 04-19 05-01 06-03 06-24 07-01 07-15 07-20 08-07 08-19 10-14 11-04 11-11 12-08 12-25",
	2038: "01-01 01-11 03-22 04-22 04-23 05-01 06-07 06-28 07-05 07-12 07-20 08-07 08-16 10-18 11-01 11-15 12-08 12-25",
}

func TestColombianHolidaysMatchTheOfficialCalendar(t *testing.T) {
	for year, days := range officialHolidays {
		t.Run(strconv.Itoa(year), func(t *testing.T) {
			var want []Date
			for md := range strings.FieldsSeq(days) {
				var m, d int
				if _, err := fmt.Sscanf(md, "%d-%d", &m, &d); err != nil {
					t.Fatal(err)
				}
				want = append(want, Date{Year: year, Month: time.Month(m), Day: d})
			}
			if got := ColombianHolidays(year); !slices.Equal(got, want) {
				t.Errorf("\n got %v\nwant %v", got, want)
			}
		})
	}
}

func TestEaster(t *testing.T) {
	for _, want := range []Date{{2008, time.March, 23}, {2011, time.April, 24}, {2025, time.April, 20}, {2026, time.April, 5}, {2038, time.April, 25}} {
		if got := easter(want.Year); got != want {
			t.Errorf("Pascua %d = %v, se esperaba %v", want.Year, got, want)
		}
	}
}

// TestEasterIsASundayInItsWindow: en todo el calendario gregoriano la Pascua es un domingo entre el 22
// de marzo y el 25 de abril.
func TestEasterIsASundayInItsWindow(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		e := easter(rapid.IntRange(1583, 4099).Draw(t, "año"))
		earliest, latest := Date{e.Year, time.March, 22}, Date{e.Year, time.April, 25}
		if e.utc().Weekday() != time.Sunday || e.utc().Before(earliest.utc()) || e.utc().After(latest.utc()) {
			t.Fatalf("Pascua %v", e)
		}
	})
}

func isColombianHoliday(d Date) bool { return slices.Contains(ColombianHolidays(d.Year), d) }

func TestAdjust(t *testing.T) {
	cases := []struct {
		name string
		from Date
		c    Convention
		want Date
	}{
		// el sábado 28 de junio de 2025 va antes del lunes 30, festivo
		{"following salta el fin de semana y el festivo", Date{2025, time.June, 28}, Following, Date{2025, time.July, 1}},
		{"preceding vuelve al viernes", Date{2025, time.June, 28}, Preceding, Date{2025, time.June, 27}},
		{"unadjusted no mueve", Date{2025, time.June, 28}, Unadjusted, Date{2025, time.June, 28}},
		{"una convención desconocida no mueve", Date{2025, time.June, 28}, Convention("x"), Date{2025, time.June, 28}},
		{"un día hábil no se mueve", Date{2025, time.June, 27}, Following, Date{2025, time.June, 27}},
		{"modified following vuelve si el siguiente hábil es de otro mes", Date{2025, time.June, 28}, ModifiedFollowing, Date{2025, time.June, 27}},
		// el sábado 31 de mayo de 2025: el siguiente hábil es el 3 de junio (el 2 es la Ascensión)
		{"modified following no cambia de mes", Date{2025, time.May, 31}, ModifiedFollowing, Date{2025, time.May, 30}},
		// el sábado 21 de junio de 2025: el lunes 23 es Corpus Christi
		{"modified following dentro del mes", Date{2025, time.June, 21}, ModifiedFollowing, Date{2025, time.June, 24}},
		{"modified following del 1 al hábil siguiente", Date{2025, time.June, 1}, ModifiedFollowing, Date{2025, time.June, 3}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := Adjust(tc.from, tc.c, isColombianHoliday); got != tc.want {
				t.Errorf("Adjust(%v, %s) = %v, se esperaba %v", tc.from, tc.c, got, tc.want)
			}
		})
	}
}

func TestIsBusinessDay(t *testing.T) {
	cases := map[string]struct {
		d    Date
		want bool
	}{
		"viernes": {Date{2025, time.June, 27}, true},
		"sábado":  {Date{2025, time.June, 28}, false},
		"domingo": {Date{2025, time.June, 29}, false},
		"festivo": {Date{2025, time.June, 30}, false},
		"martes":  {Date{2025, time.July, 1}, true},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			if got := IsBusinessDay(tc.d, isColombianHoliday); got != tc.want {
				t.Errorf("IsBusinessDay(%v) = %v, se esperaba %v", tc.d, got, tc.want)
			}
		})
	}
}

// holidaysGen son los festivos de Colombia más hasta 10 días inhábiles al azar alrededor de from.
func holidaysGen(t *rapid.T, from Date) func(Date) bool {
	extra := rapid.SliceOfN(rapid.IntRange(-20, 40), 0, 10).Draw(t, "inhábiles")
	return func(d Date) bool {
		for _, off := range extra {
			if from.addDays(off) == d {
				return true
			}
		}
		return isColombianHoliday(d)
	}
}

func dateGen(t *rapid.T) Date {
	day := rapid.IntRange(0, 365*120).Draw(t, "día")
	return Date{1990, time.January, 1}.addDays(day)
}

// TestAdjustProperties: Following y Preceding devuelven el primer día hábil hacia adelante o hacia atrás;
// ModifiedFollowing devuelve el de Following si es del mismo mes y si no el de Preceding, y solo cambia
// de mes si todo el resto del mes es inhábil.
func TestAdjustProperties(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		from := dateGen(t)
		isHoliday := holidaysGen(t, from)
		following, preceding := Adjust(from, Following, isHoliday), Adjust(from, Preceding, isHoliday)
		checkNearest(t, from, following, 1, isHoliday)
		checkNearest(t, from, preceding, -1, isHoliday)
		if got := Adjust(from, Unadjusted, isHoliday); got != from {
			t.Fatalf("Unadjusted(%v) = %v", from, got)
		}
		want := preceding
		if following.Month == from.Month {
			want = following
		}
		got := Adjust(from, ModifiedFollowing, isHoliday)
		if got != want {
			t.Fatalf("ModifiedFollowing(%v) = %v, se esperaba %v", from, got, want)
		}
		if got.Month != from.Month {
			for d := from; d.Month == from.Month; d = d.addDays(1) {
				if IsBusinessDay(d, isHoliday) {
					t.Fatalf("ModifiedFollowing(%v) = %v cambió de mes, pero %v es hábil", from, got, d)
				}
			}
		}
	})
}

// checkNearest: got es hábil y es el primero desde from en la dirección step (1 adelante, -1 atrás).
func checkNearest(t *rapid.T, from, got Date, step int, isHoliday func(Date) bool) {
	if !IsBusinessDay(got, isHoliday) {
		t.Fatalf("Adjust(%v, paso %d) = %v, inhábil", from, step, got)
	}
	for d := from; d != got; d = d.addDays(step) {
		if IsBusinessDay(d, isHoliday) {
			t.Fatalf("Adjust(%v, paso %d) = %v, pero %v es hábil y está antes", from, step, got, d)
		}
	}
}
