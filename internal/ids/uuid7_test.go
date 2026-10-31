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
		t.Fatalf("%s: version = %d, want 7", id, v)
	}
	if variant := id[8] >> 6; variant != 0b10 {
		t.Fatalf("%s: variant = %02b, want 10", id, variant)
	}
}

// TestGeneratorWritesTheClockTimestamp uses its own generator with a fixed clock: the global one moves
// its logical millisecond ahead when thousands of IDs are generated per millisecond, so it is not
// compared with time.Now.
func TestGeneratorWritesTheClockTimestamp(t *testing.T) {
	base := time.UnixMilli(1_800_000_000_000)
	g := &generator{now: func() time.Time { return base }}

	id := g.next()

	checkVersionAndVariant(t, id)
	if got := timestampMs(id); got != uint64(base.UnixMilli()) {
		t.Fatalf("timestamp = %d ms, want %d", got, base.UnixMilli())
	}
}

func TestNewIsStrictlyIncreasingInSeries(t *testing.T) {
	prev := New()
	for range 10_000 {
		id := New()
		checkVersionAndVariant(t, id)
		if bytes.Compare(id[:], prev[:]) <= 0 {
			t.Fatalf("%s is not greater than %s", id, prev)
		}
		prev = id
	}
}

func TestGeneratorStaysMonotonicWhenClockStallsOrGoesBack(t *testing.T) {
	base := time.UnixMilli(1_800_000_000_000)
	clocks := map[string][]time.Time{
		"stalled clock, more IDs than the counter": repeat(base, 5000),
		"clock going back":                         {base, base.Add(-time.Second), base.Add(-time.Minute), base},
	}
	for name, ticks := range clocks {
		t.Run(name, func(t *testing.T) {
			i := 0
			g := &generator{now: func() time.Time { tick := ticks[i]; i++; return tick }}
			prev := g.next()
			for range len(ticks) - 1 {
				id := g.next()
				if bytes.Compare(id[:], prev[:]) <= 0 {
					t.Fatalf("%s is not greater than %s", id, prev)
				}
				if timestampMs(id) < uint64(base.UnixMilli()) {
					t.Fatalf("timestamp %d went back", timestampMs(id))
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
		t.Fatalf("uppercase Parse = %s, %v", upper, err)
	}
}

func TestStringIsCanonical(t *testing.T) {
	id := ID{0x01, 0x9a, 0x2b, 0x3c, 0x4d, 0x5e, 0x7f, 0x01, 0x82, 0x03, 0x04, 0x05, 0x06, 0x07, 0x08, 0x09}

	if got, want := id.String(), "019a2b3c-4d5e-7f01-8203-040506070809"; got != want {
		t.Fatalf("String = %s, want %s", got, want)
	}
}

func TestParseRejectsInvalid(t *testing.T) {
	invalid := []string{
		"",
		"019a2b3c-4d5e-7f01-8203-04050607080",   // short
		"019a2b3c-4d5e-7f01-8203-0405060708090", // long
		"019a2b3c4d5e-7f01-8203-0405060708090",  // misplaced hyphen
		"019a2b3c-4d5e-7f01-8203-04050607080g",  // not hexadecimal
		"{019a2b3c-4d5e-7f01-8203-040506070809}",
	}
	for _, s := range invalid {
		if _, err := Parse(s); err == nil {
			t.Errorf("Parse(%q) did not fail", s)
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
	if err := back.UnmarshalText([]byte("not-a-uuid")); err == nil {
		t.Fatal("UnmarshalText accepted an invalid value")
	}
}
