// Package compliance is the facade of screening (FR-42): every party being registered and every outgoing
// payment is screened against the sanction lists, behind a Screener that fails closed (AD-3), and what the
// vendor does not clear waits as a case until compliance resolves it.
package compliance

import (
	"context"
	"fmt"
	"time"

	"github.com/DrewGGM/aluna/internal/compliance/internal/adapters/fakescreening"
	pg "github.com/DrewGGM/aluna/internal/compliance/internal/adapters/postgres"
	"github.com/DrewGGM/aluna/internal/compliance/internal/app"
	"github.com/DrewGGM/aluna/internal/ids"
	"github.com/DrewGGM/aluna/internal/outbox"
	"github.com/DrewGGM/aluna/internal/party"
	"github.com/DrewGGM/aluna/internal/postgres"
)

// Types published by compliance.
type (
	// Outcome is what screening decides about an operation.
	Outcome = app.Outcome
	// Subject is who is screened.
	Subject = app.Subject
	// CaseKind is what a case screened.
	CaseKind = app.CaseKind
	// CaseStatus is where a case stands: pending, cleared or confirmed.
	CaseStatus = app.CaseStatus
	// Case is a screening of an operation's subject.
	Case = app.Case
)

// Outcomes, subject kinds and case kinds.
const (
	OutcomeClear       = app.OutcomeClear
	OutcomePending     = app.OutcomePending
	SubjectBeneficiary = app.SubjectBeneficiary
	CaseParty          = app.CaseParty
	CasePayment        = app.CasePayment
	CaseCleared        = app.CaseCleared
	CaseConfirmed      = app.CaseConfirmed
)

// Errors of compliance.
var (
	ErrCaseNotFound = app.ErrCaseNotFound
	ErrCaseResolved = app.ErrCaseResolved
)

// eventTypes are the screening events (architecture §8), by where the case stands.
var eventTypes = map[CaseStatus]string{
	app.CasePending: "aluna.compliance.screening.pending.v1",
	CaseCleared:     "aluna.compliance.screening.cleared.v1",
	CaseConfirmed:   "aluna.compliance.screening.confirmed.v1",
}

// Events publishes compliance events; outbox.Outbox implements it.
type Events interface {
	Emit(ctx context.Context, tx postgres.Tx, e outbox.Event) error
}

// Resolved is what a module does, within the resolution's tx, when a case of its kind is resolved: subjectID
// is the case's party or payment order in entity, and cleared says whether there was no match.
type Resolved func(ctx context.Context, tx postgres.Tx, entity, subjectID ids.ID, cleared bool) error

// Service screens operations and resolves their cases.
type Service struct {
	screener *app.Screener
	events   Events
	resolved map[CaseKind]Resolved
}

// New creates the service; it publishes its events to events. Its vendor is the fake one until a real one is
// chosen (Q-16, D-2).
func New(events Events) *Service {
	return &Service{screener: app.NewScreener(fakescreening.Vendor{}, time.Now), events: events, resolved: map[CaseKind]Resolved{}}
}

// OnResolved registers, during wiring, what resolving a case of kind does; registering it again replaces it.
func (s *Service) OnResolved(kind CaseKind, fn Resolved) { s.resolved[kind] = fn }

// ScreenParty screens party p before it is registered, within tx, and records its case (FR-42).
func (s *Service) ScreenParty(ctx context.Context, tx postgres.Tx, p party.Party) (Outcome, error) {
	subject := Subject{Kind: app.SubjectParty, ID: p.ID, ExternalRef: p.ExternalRef}
	return s.screen(ctx, tx, p.LegalEntityID, CaseParty, p.ID, subject)
}

// ScreenPayment screens the beneficiary of payment order orderID of entity, within tx, and records its case
// (FR-42).
func (s *Service) ScreenPayment(ctx context.Context, tx postgres.Tx, entity, orderID ids.ID, beneficiary Subject) (Outcome, error) {
	return s.screen(ctx, tx, entity, CasePayment, orderID, beneficiary)
}

func (s *Service) screen(ctx context.Context, tx postgres.Tx, entity ids.ID, kind CaseKind, subjectID ids.ID, subject Subject) (Outcome, error) {
	c, err := app.Open(ctx, pg.NewStore(tx), s.screener, entity, kind, subjectID, subject)
	if err != nil {
		return OutcomePending, err
	}
	if err := s.emit(ctx, tx, c); err != nil {
		return OutcomePending, err
	}
	if c.Status == app.CaseCleared {
		return OutcomeClear, nil
	}
	return OutcomePending, nil
}

// PartyScreening is a Service as the party.Screener of the parties being registered.
type PartyScreening struct{ s *Service }

// PartyScreener is s as the screening of the parties being registered.
func (s *Service) PartyScreener() PartyScreening { return PartyScreening{s} }

// ScreenParty reports whether s cleared p, OutcomeClear.
func (p PartyScreening) ScreenParty(ctx context.Context, tx postgres.Tx, registered party.Party) (bool, error) {
	o, err := p.s.ScreenParty(ctx, tx, registered)
	return o == OutcomeClear, err
}

// ResolvingParties is what resolving a party's case does: the party leaves pending_screening
// (party.ResolveScreening). cmd/aluna registers it with OnResolved(CaseParty, …).
func ResolvingParties(parties *party.Service) Resolved {
	return func(ctx context.Context, tx postgres.Tx, entity, subjectID ids.ID, cleared bool) error {
		_, err := parties.ResolveScreening(ctx, tx, entity, subjectID, cleared)
		return err
	}
}

// CheckResolution reports whether case id of entity can still be resolved: ErrCaseNotFound or
// ErrCaseResolved otherwise. q can be the pool or a Tx.
func (s *Service) CheckResolution(ctx context.Context, q postgres.Querier, entity, id ids.ID) error {
	return app.CheckResolution(ctx, pg.NewStore(q), entity, id)
}

// Resolve resolves pending case id of entity within tx as to, CaseCleared (not a match) or CaseConfirmed, by
// who: it emits the result and runs what its kind registered with OnResolved; a kind with nothing registered
// is not resolved, and tx undoes the case's update.
func (s *Service) Resolve(ctx context.Context, tx postgres.Tx, entity, id ids.ID, to CaseStatus, by string) (Case, error) {
	c, err := app.Resolve(ctx, pg.NewStore(tx), entity, id, to, by)
	if err != nil {
		return Case{}, err
	}
	fn, ok := s.resolved[c.Kind]
	if !ok {
		return Case{}, fmt.Errorf("nothing resolves %s screening cases", c.Kind)
	}
	if err := s.emit(ctx, tx, c); err != nil {
		return Case{}, err
	}
	if err := fn(ctx, tx, entity, c.SubjectID, to == CaseCleared); err != nil {
		return Case{}, err
	}
	return c, nil
}

// caseEvent is the data of a screening event: what was screened and the vendor's reference, nothing
// personal.
type caseEvent struct {
	CaseID    ids.ID   `json:"case_id"`
	Kind      CaseKind `json:"kind"`
	SubjectID ids.ID   `json:"subject_id"`
	VendorRef string   `json:"vendor_ref,omitempty"`
}

// emit publishes where case c stands: pending, cleared or confirmed.
func (s *Service) emit(ctx context.Context, tx postgres.Tx, c Case) error {
	return s.events.Emit(ctx, tx, outbox.Event{
		Type: eventTypes[c.Status], Subject: "screening-cases/" + c.ID.String(), LegalEntityID: c.LegalEntityID, SchemaVersion: 1,
		Data: caseEvent{CaseID: c.ID, Kind: c.Kind, SubjectID: c.SubjectID, VendorRef: c.VendorRef},
	})
}
