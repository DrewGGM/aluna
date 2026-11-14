package app

import (
	"errors"
	"testing"
)

// TestEachOutcomeGoesItsOwnWay (FR-31): a return is no outcome of a sent order and only a return is taken as one,
// so neither rejects nor refunds by mistake; a return without its reason is invalid. None reaches the database.
func TestEachOutcomeGoesItsOwnWay(t *testing.T) {
	for name, err := range map[string]error{
		"a return as an outcome":    TakeOutcome(t.Context(), nil, RailOutcome{EndToEndID: "E2E", Status: RailReturned, Reason: "AC04"}),
		"a rejection as a return":   TakeReturn(t.Context(), nil, RailOutcome{EndToEndID: "E2E", Status: RailRejected, Reason: "AC04"}),
		"an acceptance as a return": TakeReturn(t.Context(), nil, RailOutcome{EndToEndID: "E2E", Status: RailAccepted}),
		"a return without a reason": TakeReturn(t.Context(), nil, RailOutcome{EndToEndID: "E2E", Status: RailReturned}),
	} {
		if !errors.Is(err, ErrInvalidNotification) {
			t.Errorf("%s = %v, want ErrInvalidNotification", name, err)
		}
	}
}
