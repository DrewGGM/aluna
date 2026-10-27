package ids

import (
	"bytes"
	"encoding/binary"
	"strings"
	"testing"
	"time"
)

func timestampMs(id ID) uint64 {
	var b [8]byte
	copy(b[2:], id[:6])
	return binary.BigEndian.Uint64(b[:])
}

func checkVersionAndVariant(t *testing.T, id ID) {
	t.Helper()
	if v := id[6] >> 4; v != 7 {
		t.Fatalf("%s: versión = %d, se esperaba 7", id, v)
	}
	if variant := id[8] >> 6; variant != 0b10 {
		t.Fatalf("%s: variante = %02b, se esperaba 10", id, variant)
	}
}

// TestGeneratorWritesTheClockTimestamp usa un generador propio con reloj fijo: el global adelanta su
// milisegundo lógico cuando se generan miles de IDs por milisegundo, así que no se compara con time.Now.
func TestGeneratorWritesTheClockTimestamp(t *testing.T) {
	base := time.UnixMilli(1_800_000_000_000)
	g := &generator{now: func() time.Time { return base }}

	id := g.next()

	checkVersionAndVariant(t, id)
	if got := timestampMs(id); got != uint64(base.UnixMilli()) {
		t.Fatalf("timestamp = %d ms, se esperaba %d", got, base.UnixMilli())
	}
}

func TestNewIsStrictlyIncreasingInSeries(t *testing.T) {
	prev := New()
	for range 10_000 {
		id := New()
		checkVersionAndVariant(t, id)
		if bytes.Compare(id[:], prev[:]) <= 0 {
			t.Fatalf("%s no es mayor que %s", id, prev)
		}
		prev = id
	}
}

func TestGeneratorStaysMonotonicWhenClockStallsOrGoesBack(t *testing.T) {
	base := time.UnixMilli(1_800_000_000_000)
	clocks := map[string][]time.Time{
		"reloj detenido, más IDs que el contador": repeat(base, 5000),
		"reloj que retrocede":                     {base, base.Add(-time.Second), base.Add(-time.Minute), base},
	}
	for name, ticks := range clocks {
		t.Run(name, func(t *testing.T) {
			i := 0
			g := &generator{now: func() time.Time { tick := ticks[i]; i++; return tick }}
			prev := g.next()
			for range len(ticks) - 1 {
				id := g.next()
				if bytes.Compare(id[:], prev[:]) <= 0 {
					t.Fatalf("%s no es mayor que %s", id, prev)
				}
				if timestampMs(id) < uint64(base.UnixMilli()) {
					t.Fatalf("timestamp %d retrocedió", timestampMs(id))
				}
				prev = id
			}
		})
	}
}

func repeat(t time.Time, n int) []time.Time {
	ticks := make([]time.Time, n)
	for i := range ticks {
		ticks[i] = t
	}
	return ticks
}

func TestParseRoundTrip(t *testing.T) {
	id := New()

	got, err := Parse(id.String())
	if err != nil || got != id {
		t.Fatalf("Parse(%s) = %s, %v", id, got, err)
	}
	upper, err := Parse(strings.ToUpper(id.String()))
	if err != nil || upper != id {
		t.Fatalf("Parse en mayúsculas = %s, %v", upper, err)
	}
}

func TestStringIsCanonical(t *testing.T) {
	id := ID{0x01, 0x9a, 0x2b, 0x3c, 0x4d, 0x5e, 0x7f, 0x01, 0x82, 0x03, 0x04, 0x05, 0x06, 0x07, 0x08, 0x09}

	if got, want := id.String(), "019a2b3c-4d5e-7f01-8203-040506070809"; got != want {
		t.Fatalf("String = %s, se esperaba %s", got, want)
	}
}

func TestParseRejectsInvalid(t *testing.T) {
	invalid := []string{
		"",
		"019a2b3c-4d5e-7f01-8203-04050607080",   // corto
		"019a2b3c-4d5e-7f01-8203-0405060708090", // largo
		"019a2b3c4d5e-7f01-8203-0405060708090",  // guion corrido
		"019a2b3c-4d5e-7f01-8203-04050607080g",  // no hexadecimal
		"{019a2b3c-4d5e-7f01-8203-040506070809}",
	}
	for _, s := range invalid {
		if _, err := Parse(s); err == nil {
			t.Errorf("Parse(%q) no falló", s)
		}
	}
}

func TestTextMarshaling(t *testing.T) {
	id := New()

	text, err := id.MarshalText()
	if err != nil || string(text) != id.String() {
		t.Fatalf("MarshalText = %s, %v", text, err)
	}
	var back ID
	if err := back.UnmarshalText(text); err != nil || back != id {
		t.Fatalf("UnmarshalText = %s, %v", back, err)
	}
	if err := back.UnmarshalText([]byte("no-es-un-uuid")); err == nil {
		t.Fatal("UnmarshalText aceptó un valor inválido")
	}
}
