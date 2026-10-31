package outbox

import (
	"encoding/json"
	"strings"
	"time"

	"github.com/DrewGGM/aluna/internal/ids"
)

// Event is a domain event (architecture §8): Type is aluna.<module>.<aggregate>.<event>.v<N>, Subject
// the resource it is about and Data its content, which is stored as JSON. It carries identifiers,
// amounts and statuses, never personal data (Constitution VII). A zero ID is filled in on emit.
type Event struct {
	ID                           ids.ID
	Type, Subject                string
	LegalEntityID, CorrelationID ids.ID
	SchemaVersion                int
	Data                         any
}

// StoredEvent is an event as a consumer receives it: Data is the stored JSON and Time the moment it was
// emitted.
type StoredEvent struct {
	ID                           ids.ID
	Type, Subject                string
	LegalEntityID, CorrelationID ids.ID
	SchemaVersion                int
	Data                         json.RawMessage
	Time                         time.Time
}

// cloudEvent is the CloudEvents 1.0 JSON envelope, with Aluna's extensions.
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

// MarshalJSON writes the event as a CloudEvent 1.0. source is aluna/<module>, the second segment of the
// type.
func (e StoredEvent) MarshalJSON() ([]byte, error) {
	module, _, _ := strings.Cut(strings.TrimPrefix(e.Type, "aluna."), ".")
	return json.Marshal(cloudEvent{
		SpecVersion: "1.0", ID: e.ID, Source: "aluna/" + module, Type: e.Type, Subject: e.Subject,
		Time: e.Time.UTC(), DataContentType: "application/json", Data: e.Data,
		LegalEntityID: e.LegalEntityID, CorrelationID: e.CorrelationID, SchemaVersion: e.SchemaVersion,
	})
}
