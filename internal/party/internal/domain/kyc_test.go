package domain

import (
	"errors"
	"testing"
)

// TestKYCTransitions: pending goes to approved or rejected, approved to suspended and suspended back to
// approved; every other change, including staying in the same status, is ErrInvalidKYCTransition.
func TestKYCTransitions(t *testing.T) {
	statuses := []KYCStatus{KYCPending, KYCApproved, KYCRejected, KYCSuspended}
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
