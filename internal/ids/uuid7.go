// Package ids genera y parsea identificadores UUIDv7 (RFC 9562 §5.7). Son ordenables por tiempo, así
// que los índices B-tree de PostgreSQL crecen por el final en vez de fragmentarse.
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

// ErrInvalid indica un texto que no es un UUID en formato canónico.
var ErrInvalid = errors.New("uuid inválido")

// ID es un UUID de 16 bytes.
type ID [16]byte

const (
	maxCounter   = 0xFFF // rand_a tiene 12 bits
	counterSeeds = 0x800 // el contador arranca en la mitad baja para dejar espacio a incrementos
)

var process = &generator{now: time.Now}

// New genera un UUIDv7. Los IDs que genera un mismo proceso son estrictamente crecientes.
func New() ID { return process.next() }

// generator aplica el método 1 de RFC 9562 §6.2: los 12 bits rand_a son un contador que arranca en
// un valor aleatorio en cada milisegundo nuevo. Si el contador se agota o el reloj retrocede, avanza
// el milisegundo lógico en vez de repetir o retroceder.
type generator struct {
	mu      sync.Mutex
	now     func() time.Time
	lastMs  int64
	counter uint16
}

func (g *generator) next() ID {
	var random [10]byte
	rand.Read(random[:]) // nunca falla desde Go 1.24: si el SO no da entropía, el proceso aborta

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
	binary.BigEndian.PutUint64(id[:8], uint64(ms)<<16)  //nolint:gosec // ms es un instante Unix positivo
	binary.BigEndian.PutUint16(id[6:8], 0x7000|counter) // versión 7 + contador de 12 bits
	copy(id[8:], random[2:])
	id[8] = id[8]&0x3F | 0x80 // variante RFC 9562
	return id
}

// String devuelve el formato canónico 8-4-4-4-12 en minúsculas.
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

// Parse lee un UUID en formato canónico, en minúsculas o mayúsculas.
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

// MarshalText implementa encoding.TextMarshaler (JSON y logs usan el formato canónico).
func (id ID) MarshalText() ([]byte, error) { return []byte(id.String()), nil }

// UnmarshalText implementa encoding.TextUnmarshaler.
func (id *ID) UnmarshalText(text []byte) error {
	parsed, err := Parse(string(text))
	if err != nil {
		return err
	}
	*id = parsed
	return nil
}
