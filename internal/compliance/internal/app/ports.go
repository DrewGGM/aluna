// Package app holds compliance's use cases and the ports they need: the screening vendor (FR-42) behind
// a Screener that fails closed (AD-3), and the cash totals of each party (FR-43).
package app

import (
	"context"

	"github.com/DrewGGM/aluna/internal/ids"
)

// SubjectKind is what is screened.
type SubjectKind string

// Subject kinds: a party when it is registered and the beneficiary of an outgoing payment.
const (
	SubjectParty       SubjectKind = "party"
	SubjectBeneficiary SubjectKind = "beneficiary"
)

// Subject is who is screened. Aluna keeps no names or documents: ExternalRef is the subject's reference in
// the system that has them, which the vendor's adapter resolves.
type Subject struct {
	Kind        SubjectKind
	ID          ids.ID
	ExternalRef string
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
