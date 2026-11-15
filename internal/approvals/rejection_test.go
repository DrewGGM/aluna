package approvals

import (
	"errors"
	"fmt"
	"testing"
)

// TestARejectionKeepsItsCode (Q-97 a): Rejected is ErrCommandRejected and the error it wraps, and records its code;
// a rejection wrapped by hand has none, so its request fails without one.
func TestARejectionKeepsItsCode(t *testing.T) {
	cause := errors.New("the transaction was already reversed")
	err := fmt.Errorf("approve: %w", Rejected("already_reversed", cause))
	if !errors.Is(err, ErrCommandRejected) || !errors.Is(err, cause) || failureCode(err) != "already_reversed" {
		t.Fatalf("%v: rejected %v, cause %v, code %q", err, errors.Is(err, ErrCommandRejected), errors.Is(err, cause), failureCode(err))
	}
	if byHand := fmt.Errorf("%w: %w", ErrCommandRejected, cause); failureCode(byHand) != "" {
		t.Errorf("a rejection wrapped by hand has code %q, want none", failureCode(byHand))
	}
}
