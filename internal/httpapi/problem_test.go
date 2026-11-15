package httpapi

import (
	"regexp"
	"testing"
)

// TestProblemCodesFitAFailedRequest: every problem code is lowercase letters, digits and underscores, up to 64,
// as approvals.requests.failure takes it (migration 0047): a code it refused would leave an approval that its
// command rejected pending, failing with 500, instead of failed.
func TestProblemCodesFitAFailedRequest(t *testing.T) {
	code := regexp.MustCompile(`^[a-z][a-z0-9_]{0,63}$`)
	all := []Problem{notFound, forbidden}
	for _, p := range problems {
		all = append(all, p.p)
	}
	for _, p := range all {
		if !code.MatchString(p.Code) {
			t.Errorf("problem code %q does not fit approvals.requests.failure", p.Code)
		}
	}
}
