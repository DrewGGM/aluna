// Package ids generates and parses UUIDv7 identifiers (RFC 9562 §5.7). They sort by time, so
// PostgreSQL's B-tree indexes grow at the end instead of fragmenting.
package ids

import (
	"crypto/rand"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"sync"
	"time"
)

// ErrInvalid means text that is not a UUID in canonical form.
var ErrInvalid = errors.New("invalid uuid")

// ID is a 16-byte UUID.
type ID [16]byte

const (
	maxCounter   = 0xFFF // rand_a has 12 bits
	counterSeeds = 0x800 // the counter starts in the lower half to leave room for increments
)

var process = &generator{now: time.Now}

// New generates a UUIDv7. The IDs a single process generates are strictly increasing.
func New() ID { return process.next() }

// generator applies method 1 of RFC 9562 §6.2: the 12 rand_a bits are a counter that starts at a random
// value on each new millisecond. If the counter runs out or the clock goes back, it advances the logical
// millisecond instead of repeating or going back.
type generator struct {
	mu      sync.Mutex
	now     func() time.Time
	lastMs  int64
	counter uint16
}

func (g *generator) next() ID {
	var random [10]byte
	rand.Read(random[:]) // never fails since Go 1.24: if the OS provides no entropy, the process aborts

	g.mu.Lock()
	ms := g.now().UnixMilli()
	seed := binary.BigEndian.Uint16(random[:2]) % counterSeeds
	switch {
	case ms > g.lastMs:
		g.lastMs, g.counter = ms, seed
	case g.counter < maxCounter:
		g.counter++
	default:
		g.lastMs, g.counter = g.lastMs+1, seed
	}
	ms, counter := g.lastMs, g.counter
	g.mu.Unlock()

	var id ID
	binary.BigEndian.PutUint64(id[:8], uint64(ms)<<16)  //nolint:gosec // ms is a positive Unix instant
	binary.BigEndian.PutUint16(id[6:8], 0x7000|counter) // version 7 + 12-bit counter
	copy(id[8:], random[2:])
	id[8] = id[8]&0x3F | 0x80 // RFC 9562 variant
	return id
}

// String returns the canonical lowercase 8-4-4-4-12 form.
func (id ID) String() string {
	var buf [36]byte
	hex.Encode(buf[0:8], id[0:4])
	hex.Encode(buf[9:13], id[4:6])
	hex.Encode(buf[14:18], id[6:8])
	hex.Encode(buf[19:23], id[8:10])
	hex.Encode(buf[24:36], id[10:16])
	buf[8], buf[13], buf[18], buf[23] = '-', '-', '-', '-'
	return string(buf[:])
}

// Parse reads a UUID in canonical form, lowercase or uppercase.
func Parse(s string) (ID, error) {
	if len(s) != 36 || s[8] != '-' || s[13] != '-' || s[18] != '-' || s[23] != '-' {
		return ID{}, fmt.Errorf("%w: %q", ErrInvalid, s)
	}
	var id ID
	digits := s[0:8] + s[9:13] + s[14:18] + s[19:23] + s[24:36]
	if _, err := hex.Decode(id[:], []byte(digits)); err != nil {
		return ID{}, fmt.Errorf("%w: %q", ErrInvalid, s)
	}
	return id, nil
}

// MarshalText implements encoding.TextMarshaler (JSON and logs use the canonical form).
func (id ID) MarshalText() ([]byte, error) { return []byte(id.String()), nil }

// UnmarshalText implements encoding.TextUnmarshaler.
func (id *ID) UnmarshalText(text []byte) error {
	parsed, err := Parse(string(text))
	if err != nil {
		return err
	}
	*id = parsed
	return nil
}
