package app

import (
	"crypto/sha256"
	"encoding/json"
	"testing"
	"time"
)

func goldenRecords() []Record {
	return []Record{
		{
			Seq: 7, RecordedAt: time.Date(2026, time.October, 27, 10, 4, 5, 123456000, time.FixedZone("COT", -5*3600)),
			Actor: "operador", Role: "tesorero", Channel: "api", IP: "10.0.0.7", CorrelationID: "c-1",
			Command: "POST /v1/transactions", Targets: []string{"t-1", "t-2"}, Result: "ok",
			After: json.RawMessage(`{"id": "t-1"}`),
		},
		{
			Seq: 9, RecordedAt: time.Date(2026, time.October, 27, 15, 4, 6, 0, time.UTC),
			Actor: "señora <ops> & cía", Role: "api", Channel: "api", IP: "::1", CorrelationID: "c-2",
			Command: "POST /v1/holds/{id}/release", Targets: []string{}, Result: "hold_not_pending",
			Before: json.RawMessage(`{"status": "released"}`),
		},
	}
}

// TestTheBlockHashIsPinned (ADR-0012): el formato de la evidencia no cambia. El hash de un bloque es
// SHA-256 del hash anterior seguido de un JSON compacto por registro, terminado en '\n', con todos sus
// campos en este orden, recorded_at en UTC con microsegundos, before/after compactados y sin escapar <, >
// ni & (cualquier lenguaje lo recalcula igual).
func TestTheBlockHashIsPinned(t *testing.T) {
	prev := sha256.Sum256([]byte("bloque anterior"))
	lines := `{"seq":7,"recorded_at":"2026-10-27T15:04:05.123456Z","actor":"operador","role":"tesorero","channel":"api",` +
		`"ip":"10.0.0.7","correlation_id":"c-1","command":"POST /v1/transactions","targets":["t-1","t-2"],"result":"ok",` +
		`"before":null,"after":{"id":"t-1"}}` + "\n" +
		`{"seq":9,"recorded_at":"2026-10-27T15:04:06.000000Z","actor":"señora <ops> & cía","role":"api","channel":"api",` +
		`"ip":"::1","correlation_id":"c-2","command":"POST /v1/holds/{id}/release","targets":[],"result":"hold_not_pending",` +
		`"before":{"status":"released"},"after":null}` + "\n"
	want := sha256.Sum256(append(prev[:], lines...))

	got, err := blockHash(prev, goldenRecords())

	if err != nil || got != want {
		t.Fatalf("hash = %x (%v), se esperaba %x", got, err, want)
	}
}
