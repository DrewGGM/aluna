package domain

import (
	"errors"
	"slices"
	"testing"

	"github.com/DrewGGM/aluna/internal/ledger"
)

var statuses = []Status{Pending, Active, Frozen, Dormant, Closed}

// legal is the diagram of docs/architecture.md (FR-22): every other pair is illegal.
var legal = map[Status][]Status{
	Pending: {Active, Closed},
	Active:  {Frozen, Dormant, Closed},
	Frozen:  {Active},
	Dormant: {Active, Closed},
}

// TestEveryTransitionOfTheDiagram: each of the 25 pairs of statuses is legal exactly when the diagram has
// it; closed is final and no status goes to itself.
func TestEveryTransitionOfTheDiagram(t *testing.T) {
	for _, from := range statuses {
		for _, to := range statuses {
			err := CheckTransition(from, to)
			if want := slices.Contains(legal[from], to); want != (err == nil) {
				t.Errorf("%s → %s = %v, want legal = %v", from, to, err, want)
			}
			if err != nil && !errors.Is(err, ErrInvalidTransition) {
				t.Errorf("%s → %s = %v, want ErrInvalidTransition", from, to, err)
			}
		}
	}
}

// TestAnUnknownStatusHasNoTransition: a status outside the diagram goes nowhere and nothing goes to it.
func TestAnUnknownStatusHasNoTransition(t *testing.T) {
	if err := CheckTransition("blocked", Active); !errors.Is(err, ErrInvalidTransition) {
		t.Errorf("blocked → active = %v, want ErrInvalidTransition", err)
	}
	if err := CheckTransition(Active, "blocked"); !errors.Is(err, ErrInvalidTransition) {
		t.Errorf("active → blocked = %v, want ErrInvalidTransition", err)
	}
}

// TestAnAccountOpensActiveOnlyWithKYCApproved: with KYC approved the account opens active; with a status the
// product allows but not approved yet (pending, in a simplified deposit), it opens pending.
func TestAnAccountOpensActiveOnlyWithKYCApproved(t *testing.T) {
	if got := OpeningStatus(true); got != Active {
		t.Errorf("KYC approved opens %s, want active", got)
	}
	if got := OpeningStatus(false); got != Pending {
		t.Errorf("KYC allowed but not approved opens %s, want pending", got)
	}
}

// TestAnAccountClosesEmpty: closing needs a posted balance of 0 and no pending holds on either side.
func TestAnAccountClosesEmpty(t *testing.T) {
	cases := []struct {
		name                    string
		posted, debits, credits int64
		want                    error
	}{
		{"empty", 0, 0, 0, nil},
		{"with balance", -100, 0, 0, ErrNotEmpty},
		{"overdrawn", 100, 0, 0, ErrNotEmpty},
		{"with a pending debit hold", 0, 50, 0, ErrPendingHolds},
		{"with a pending credit hold", 0, 0, 50, ErrPendingHolds},
		{"with a balance and its hold", -100, 100, 0, ErrPendingHolds},
	}
	for _, c := range cases {
		b := ledger.Balance{Posted: c.posted, PendingDebits: c.debits, PendingCredits: c.credits}
		if err := CheckClosable(b); !errors.Is(err, c.want) {
			t.Errorf("%s = %v, want %v", c.name, err, c.want)
		}
	}
}
