package compliance

import (
	"errors"
	"log/slog"
	"math"
	"net/http"
	"testing"
	"time"
)

// TestListScreeningIsCheckedBeforeItScreens (ADR-0018): a threshold out of 0.5 to 1, or not a number, which would
// match no name and clear nearly everyone, a maximum age of none or over a week, no client or no log are refused
// before any list is read.
func TestListScreeningIsCheckedBeforeItScreens(t *testing.T) {
	valid := ListScreeningConfig{
		IdentityURL: "https://onboarding.example/identity", IdentityToken: "t0k3n", Threshold: 0.92, MaxAge: 48 * time.Hour,
		Client: &http.Client{}, Log: slog.Default(),
	}
	for name, change := range map[string]func(*ListScreeningConfig){
		"a low threshold":    func(l *ListScreeningConfig) { l.Threshold = 0.3 },
		"a high threshold":   func(l *ListScreeningConfig) { l.Threshold = 1.5 },
		"not a number":       func(l *ListScreeningConfig) { l.Threshold = math.NaN() },
		"no maximum age":     func(l *ListScreeningConfig) { l.MaxAge = 0 },
		"over a week":        func(l *ListScreeningConfig) { l.MaxAge = 169 * time.Hour },
		"no identity client": func(l *ListScreeningConfig) { l.Client = nil },
		"no log":             func(l *ListScreeningConfig) { l.Log = nil },
	} {
		l := valid
		change(&l)
		if _, err := NewWithLists(t.Context(), nil, nil, l); !errors.Is(err, errListScreening) {
			t.Errorf("%s = %v, want errListScreening", name, err)
		}
	}
}
