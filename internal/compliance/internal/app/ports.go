// Package app holds compliance's use cases and the ports they need: the screening vendor (FR-42) behind
// a Screener that fails closed (AD-3), and the cash totals of each party (FR-43).
package app

import (
	"context"
	"time"

	"github.com/DrewGGM/aluna/internal/compliance/internal/domain/matching"
	"github.com/DrewGGM/aluna/internal/ids"
)

// SubjectKind is what is screened.
type SubjectKind string

// Subject kinds: a party when it is registered, and the beneficiary of an outgoing payment, by its key or by its
// account, which may be written the same and are not the same.
const (
	SubjectParty   SubjectKind = "party"
	SubjectKey     SubjectKind = "key"
	SubjectAccount SubjectKind = "account"
)

// Subject is who is screened. Aluna keeps no names or documents: ExternalRef is the subject's reference in
// the system that has them, which the vendor's adapter resolves, among LegalEntityID's: a party's reference is
// only unique within its entity.
type Subject struct {
	Kind          SubjectKind
	ID            ids.ID
	LegalEntityID ids.ID
	ExternalRef   string
}

// Status is the vendor's answer about a subject.
type Status string

// Statuses: the subject is on no list, or it may be on one and compliance decides.
const (
	StatusClear         Status = "clear"
	StatusPossibleMatch Status = "possible_match"
)

// Result is the vendor's answer, with its own reference to the check.
type Result struct {
	Status Status
	Ref    string
}

// ScreeningPort is the screening vendor (sanction lists and PEPs, D-2).
type ScreeningPort interface {
	Screen(ctx context.Context, subject Subject) (Result, error)
}

// Identity is who a subject is, as the entity's identity service says: Aluna keeps no names or documents.
type Identity struct {
	Names, Documents []string
}

// Identities resolve a subject's external reference to who it is: ErrUnknownSubject if they do not know it.
type Identities interface {
	Identify(ctx context.Context, s Subject) (Identity, error)
}

// LoadedLists are the restrictive lists as loaded: their entries, indexed, and when each was last refreshed.
type LoadedLists struct {
	Index     *matching.Index
	Refreshed map[string]time.Time
}

// Watchlist is the restrictive lists as last loaded, without touching the database: screening runs within the
// transaction of the operation it screens.
type Watchlist interface {
	Current() (LoadedLists, error)
}

// ListStore keeps the restrictive lists.
type ListStore interface {
	// Stored is the version of list as last stored and how many entries it has: "" and 0 if it never was.
	Stored(ctx context.Context, list string) (version string, entries int, err error)
	// Replace stores entries as list's, of version, refreshed now, in place of what it had.
	Replace(ctx context.Context, list, version string, entries []matching.Entry) error
	// Touch records that list, still of the same version, was refreshed now.
	Touch(ctx context.Context, list string) error
}
