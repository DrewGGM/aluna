package outbox

import (
	"encoding/json"
	"strings"
	"time"

	"github.com/DrewGGM/aluna/internal/ids"
)

// Event es un evento de dominio (architecture §8): Type es aluna.<modulo>.<agregado>.<evento>.v<N>,
// Subject el recurso del que habla y Data su contenido, que se guarda como JSON. Lleva identificadores,
// montos y estados, nunca datos personales (Constitución VII). Un ID cero se completa al emitir.
type Event struct {
	ID                           ids.ID
	Type, Subject                string
	LegalEntityID, CorrelationID ids.ID
	SchemaVersion                int
	Data                         any
}

// StoredEvent es un evento como lo recibe un consumidor: Data es el JSON guardado y Time el momento en que
// se emitió.
type StoredEvent struct {
	ID                           ids.ID
	Type, Subject                string
	LegalEntityID, CorrelationID ids.ID
	SchemaVersion                int
	Data                         json.RawMessage
	Time                         time.Time
}

// cloudEvent es el envelope CloudEvents 1.0 en JSON, con las extensiones de Aluna.
type cloudEvent struct {
	SpecVersion     string          `json:"specversion"`
	ID              ids.ID          `json:"id"`
	Source          string          `json:"source"`
	Type            string          `json:"type"`
	Subject         string          `json:"subject"`
	Time            time.Time       `json:"time"`
	DataContentType string          `json:"datacontenttype"`
	Data            json.RawMessage `json:"data"`
	LegalEntityID   ids.ID          `json:"legalentityid"`
	CorrelationID   ids.ID          `json:"correlationid,omitzero"`
	SchemaVersion   int             `json:"schemaversion"`
}

// MarshalJSON escribe el evento como CloudEvent 1.0. source es aluna/<modulo>, el segundo segmento del
// tipo.
func (e StoredEvent) MarshalJSON() ([]byte, error) {
	module, _, _ := strings.Cut(strings.TrimPrefix(e.Type, "aluna."), ".")
	return json.Marshal(cloudEvent{
		SpecVersion: "1.0", ID: e.ID, Source: "aluna/" + module, Type: e.Type, Subject: e.Subject,
		Time: e.Time.UTC(), DataContentType: "application/json", Data: e.Data,
		LegalEntityID: e.LegalEntityID, CorrelationID: e.CorrelationID, SchemaVersion: e.SchemaVersion,
	})
}
