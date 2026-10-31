// Package party is the core's party reference data (FR-16): who the entity's customers are, with their
// KYC status, risk level, PEP flag and consents, versioned. Documentary data lives in the onboarding
// system; the core keeps only external_ref pointing to it.
package party

import (
	"context"
	"fmt"
	"slices"

	"github.com/DrewGGM/aluna/internal/ids"
	"github.com/DrewGGM/aluna/internal/outbox"
	pg "github.com/DrewGGM/aluna/internal/party/internal/adapters/postgres"
	"github.com/DrewGGM/aluna/internal/party/internal/domain"
	"github.com/DrewGGM/aluna/internal/postgres"
)

type (
	// Party is the reference data of one of the entity's customers, with its current version.
	Party = domain.Party
	// Version is a party's state from its ValidFrom onward.
	Version = domain.Version
	// Kind is the kind of party: natural or legal person.
	Kind = domain.Kind
	// KYCStatus is the know-your-customer status.
	KYCStatus = domain.KYCStatus
	// RiskLevel is the SARLAFT risk level.
	RiskLevel = domain.RiskLevel
	// RegisterInput is a new party as onboarding hands it over.
	RegisterInput = domain.Registration
)

// Kinds, KYC statuses and risk levels.
const (
	KindNatural  = domain.KindNatural
	KindLegal    = domain.KindLegal
	KYCPending   = domain.KYCPending
	KYCApproved  = domain.KYCApproved
	KYCRejected  = domain.KYCRejected
	KYCSuspended = domain.KYCSuspended
	RiskLow      = domain.RiskLow
	RiskMedium   = domain.RiskMedium
	RiskHigh     = domain.RiskHigh
)

var (
	// ErrInvalidParty means the party data is invalid.
	ErrInvalidParty = domain.ErrInvalidParty
	// ErrAlreadyRegistered means the entity already registered that external_ref.
	ErrAlreadyRegistered = domain.ErrAlreadyRegistered
	// ErrNotFound means the party does not exist in the entity.
	ErrNotFound = domain.ErrNotFound
	// ErrInvalidKYCTransition means the KYC status cannot move from the current status to the requested one.
	ErrInvalidKYCTransition = domain.ErrInvalidKYCTransition
	// ErrStaleKYCChange means the party changed after the KYC change was requested.
	ErrStaleKYCChange = domain.ErrStaleKYCChange
)

// typeRegistered is the event of a registered party (architecture §8).
const typeRegistered = "aluna.party.party.registered.v1"

// Events publishes party events; outbox.Outbox implements it.
type Events interface {
	Emit(ctx context.Context, tx postgres.Tx, e outbox.Event) error
}

// Service registers and reads parties.
type Service struct {
	events Events
}

// New creates the service; it publishes its events to events.
func New(events Events) *Service { return &Service{events: events} }

// Register stores a new party within tx, with its first version in KYC pending, and emits
// aluna.party.party.registered.v1. An external_ref the entity already registered is ErrAlreadyRegistered.
func (s *Service) Register(ctx context.Context, tx postgres.Tx, in RegisterInput) (Party, error) {
	p, err := domain.NewParty(ids.New(), in)
	if err != nil {
		return Party{}, err
	}
	if p, err = pg.Insert(ctx, tx, p); err != nil {
		return Party{}, err
	}
	// without external_ref: depending on onboarding, it may be a document number
	err = s.events.Emit(ctx, tx, outbox.Event{
		Type: typeRegistered, Subject: "parties/" + p.ID.String(), LegalEntityID: p.LegalEntityID, SchemaVersion: 1,
		Data: registeredPayload{PartyID: p.ID, Kind: p.Kind, KYCStatus: p.Current.KYCStatus, RiskLevel: p.Current.RiskLevel, PEP: p.Current.PEP},
	})
	if err != nil {
		return Party{}, err
	}
	return p, nil
}

type registeredPayload struct {
	PartyID   ids.ID    `json:"party_id"`
	Kind      Kind      `json:"kind"`
	KYCStatus KYCStatus `json:"kyc_status"`
	RiskLevel RiskLevel `json:"risk_level"`
	PEP       bool      `json:"pep"`
}

// Get returns party id of the entity with its current version, or ErrNotFound: another entity's party
// does not exist for it.
func (s *Service) Get(ctx context.Context, q postgres.Querier, entity, id ids.ID) (Party, error) {
	return pg.Get(ctx, q, entity, id)
}

// CheckKYCChange reports whether party id of the entity may move to KYC status to now, and returns the
// version it checked against, or ErrNotFound or ErrInvalidKYCTransition: a change request is checked before
// it waits for approval, and carries that version.
func (s *Service) CheckKYCChange(ctx context.Context, q postgres.Querier, entity, id ids.ID, to KYCStatus) (int, error) {
	p, err := pg.Get(ctx, q, entity, id)
	if err != nil {
		return 0, err
	}
	return p.Current.Number, domain.CheckKYCTransition(p.Current.KYCStatus, to)
}

// ChangeKYC records within tx a new version of party id of the entity with KYC status to, keeping the rest
// of its state (FR-16), if the party is still at version seen, the one the change was requested against; if
// not, it is ErrStaleKYCChange, even when the status went back to the same value. It works under a lock that
// serializes the party's changes.
func (s *Service) ChangeKYC(ctx context.Context, tx postgres.Tx, entity, id ids.ID, seen int, to KYCStatus) (Party, error) {
	if err := pg.LockForChange(ctx, tx, id); err != nil {
		return Party{}, err
	}
	p, err := pg.Get(ctx, tx, entity, id)
	if err != nil {
		return Party{}, err
	}
	if p.Current.Number != seen {
		return Party{}, fmt.Errorf("%w: requested at version %d, now at %d", ErrStaleKYCChange, seen, p.Current.Number)
	}
	if err := domain.CheckKYCTransition(p.Current.KYCStatus, to); err != nil {
		return Party{}, err
	}
	next := p.Current
	next.Number, next.KYCStatus = p.Current.Number+1, to
	if p.Current, err = pg.InsertVersion(ctx, tx, p.ID, next); err != nil {
		return Party{}, err
	}
	return p, nil
}

// KYCSatisfies reports whether the current KYC status of party partyID of the entity is among allowed
// (FR-17).
func (s *Service) KYCSatisfies(ctx context.Context, q postgres.Querier, entity, partyID ids.ID, allowed []KYCStatus) (bool, error) {
	p, err := pg.Get(ctx, q, entity, partyID)
	if err != nil {
		return false, err
	}
	return slices.Contains(allowed, p.Current.KYCStatus), nil
}
