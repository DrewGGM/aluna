// Package domain models a party's reference data (FR-16): its kind, the reference to the onboarding
// system and its versioned state. It keeps no names or documents.
package domain

import (
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/DrewGGM/aluna/internal/ids"
)

// Kind is the kind of party: a natural or a legal person.
type Kind string

// Party kinds.
const (
	KindNatural Kind = "natural"
	KindLegal   Kind = "legal"
)

// Valid reports whether k is a known kind.
func (k Kind) Valid() bool { return k == KindNatural || k == KindLegal }

// KYCStatus is the know-your-customer status.
type KYCStatus string

// KYC statuses.
const (
	KYCPending   KYCStatus = "pending"
	KYCApproved  KYCStatus = "approved"
	KYCRejected  KYCStatus = "rejected"
	KYCSuspended KYCStatus = "suspended"
)

// RiskLevel is the SARLAFT risk level assigned by onboarding.
type RiskLevel string

// Risk levels.
const (
	RiskLow    RiskLevel = "low"
	RiskMedium RiskLevel = "medium"
	RiskHigh   RiskLevel = "high"
)

// Valid reports whether r is a known risk level.
func (r RiskLevel) Valid() bool { return r == RiskLow || r == RiskMedium || r == RiskHigh }

// Limits of what a party stores forever (its versions are immutable): an external_ref of up to 255
// characters, and up to 32 consents named like personal_data.
const (
	maxExternalRef = 255
	maxConsents    = 32
)

var consentName = regexp.MustCompile(`^[a-z][a-z0-9_]{0,63}$`)

var (
	// ErrInvalidParty means the party data is invalid.
	ErrInvalidParty = errors.New("invalid party")
	// ErrAlreadyRegistered means the entity already registered that external_ref.
	ErrAlreadyRegistered = errors.New("party already registered")
	// ErrNotFound means the party does not exist in the entity.
	ErrNotFound = errors.New("party not found")
)

// Party is the reference data of one of the entity's customers, with its current version.
type Party struct {
	ID, LegalEntityID ids.ID
	Kind              Kind
	ExternalRef       string
	CreatedAt         time.Time
	Current           Version
}

// Version is a party's state from ValidFrom onward. Number starts at 1 and every change adds one.
type Version struct {
	Number                int
	KYCStatus             KYCStatus
	RiskLevel             RiskLevel
	PEP                   bool
	Consents              map[string]bool
	ValidFrom, RecordedAt time.Time
}

// Registration is a new party as onboarding hands it over.
type Registration struct {
	LegalEntityID ids.ID
	Kind          Kind
	ExternalRef   string
	RiskLevel     RiskLevel
	PEP           bool
	Consents      map[string]bool
}

// NewParty builds party id from its registration: its first version is always KYC pending, because the KYC
// status only changes through maker-checker. The external_ref is trimmed, and no consents is an empty set.
func NewParty(id ids.ID, in Registration) (Party, error) {
	externalRef := strings.TrimSpace(in.ExternalRef)
	switch {
	case !in.Kind.Valid():
		return Party{}, fmt.Errorf("%w: kind %q", ErrInvalidParty, in.Kind)
	case !validExternalRef(externalRef):
		return Party{}, fmt.Errorf("%w: external_ref must be 1 to %d visible characters", ErrInvalidParty, maxExternalRef)
	case !in.RiskLevel.Valid():
		return Party{}, fmt.Errorf("%w: risk level %q", ErrInvalidParty, in.RiskLevel)
	case len(in.Consents) > maxConsents:
		return Party{}, fmt.Errorf("%w: more than %d consents", ErrInvalidParty, maxConsents)
	}
	consents := make(map[string]bool, len(in.Consents)) // never nil: the database stores an object
	for name, given := range in.Consents {
		if !consentName.MatchString(name) {
			return Party{}, fmt.Errorf("%w: consent name %q", ErrInvalidParty, name)
		}
		consents[name] = given
	}
	return Party{
		ID: id, LegalEntityID: in.LegalEntityID, Kind: in.Kind, ExternalRef: externalRef,
		Current: Version{Number: 1, KYCStatus: KYCPending, RiskLevel: in.RiskLevel, PEP: in.PEP, Consents: consents},
	}, nil
}

// validExternalRef reports whether ref is valid UTF-8 of 1 to maxExternalRef characters without control or
// invisible format characters (PostgreSQL rejects NUL, and "ref" and "ref" plus a zero-width space would be
// two parties).
func validExternalRef(ref string) bool {
	n := utf8.RuneCountInString(ref)
	return utf8.ValidString(ref) && n >= 1 && n <= maxExternalRef && strings.IndexFunc(ref, hidden) < 0
}

func hidden(r rune) bool { return unicode.IsControl(r) || unicode.Is(unicode.Cf, r) }
