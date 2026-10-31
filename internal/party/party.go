// Package party is the core's party reference data (FR-16): who the entity's customers are, with their
// KYC status, risk level, PEP flag and consents, versioned. Documentary data lives in the onboarding
// system; the core keeps only external_ref pointing to it.
package party

import (
	"context"
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

// KYCSatisfies reports whether the current KYC status of party partyID of the entity is among allowed
// (FR-17).
func (s *Service) KYCSatisfies(ctx context.Context, q postgres.Querier, entity, partyID ids.ID, allowed []KYCStatus) (bool, error) {
	p, err := pg.Get(ctx, q, entity, partyID)
	if err != nil {
		return false, err
	}
	return slices.Contains(allowed, p.Current.KYCStatus), nil
}
