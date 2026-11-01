package domain

import (
	"errors"
	"testing"
)

// TestKYCTransitions: pending goes to approved or rejected, approved to suspended and suspended back to
// approved; every other change, including staying in the same status and any from or to pending_screening,
// is ErrInvalidKYCTransition.
func TestKYCTransitions(t *testing.T) {
	statuses := []KYCStatus{KYCPending, KYCApproved, KYCRejected, KYCSuspended, KYCPendingScreening}
	valid := map[[2]KYCStatus]bool{
		{KYCPending, KYCApproved}:   true,
		{KYCPending, KYCRejected}:   true,
		{KYCApproved, KYCSuspended}: true,
		{KYCSuspended, KYCApproved}: true,
	}
	for _, from := range statuses {
		for _, to := range statuses {
			err := CheckKYCTransition(from, to)
			if valid[[2]KYCStatus{from, to}] != (err == nil) {
				t.Errorf("%s to %s = %v, want allowed = %v", from, to, err, valid[[2]KYCStatus{from, to}])
			}
			if err != nil && !errors.Is(err, ErrInvalidKYCTransition) {
				t.Errorf("%s to %s = %v, want ErrInvalidKYCTransition", from, to, err)
			}
		}
	}
}

// TestAnUnknownStatusHasNoTransition: a status outside the four known ones goes nowhere.
func TestAnUnknownStatusHasNoTransition(t *testing.T) {
	if err := CheckKYCTransition(KYCPending, "verified"); !errors.Is(err, ErrInvalidKYCTransition) {
		t.Fatalf("pending to verified = %v, want ErrInvalidKYCTransition", err)
	}
}

// TestAScreeningIsResolvedOnce (FR-42): a party pending screening goes to pending if the case is cleared and
// to rejected if the match is confirmed; a party in any other status has no screening to resolve.
func TestAScreeningIsResolvedOnce(t *testing.T) {
	cleared, clearErr := ResolveScreening(KYCPendingScreening, true)
	confirmed, confirmErr := ResolveScreening(KYCPendingScreening, false)

	if clearErr != nil || cleared != KYCPending || confirmErr != nil || confirmed != KYCRejected {
		t.Fatalf("cleared = %s (%v) and confirmed = %s (%v), want pending and rejected", cleared, clearErr, confirmed, confirmErr)
	}
	for _, from := range []KYCStatus{KYCPending, KYCApproved, KYCRejected, KYCSuspended} {
		if _, err := ResolveScreening(from, true); !errors.Is(err, ErrNotPendingScreening) {
			t.Errorf("resolving the screening of a party %s = %v, want ErrNotPendingScreening", from, err)
		}
	}
}
