package app_test

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/DrewGGM/aluna/internal/ids"
	"github.com/DrewGGM/aluna/internal/webhooks/internal/app"
)

// TestEightRetriesTake72Hours (FR-46): the waits double and add up to just under 72 hours, and after the eighth retry
// a delivery is failed.
func TestEightRetriesTake72Hours(t *testing.T) {
	var total time.Duration
	for retry := 1; retry <= app.MaxRetries; retry++ {
		if retry > 1 && app.Backoff(retry) != 2*app.Backoff(retry-1) {
			t.Errorf("retry %d waits %s, after %s", retry, app.Backoff(retry), app.Backoff(retry-1))
		}
		total += app.Backoff(retry)
	}
	if total > 72*time.Hour || total < 71*time.Hour+55*time.Minute {
		t.Errorf("the retries take %s, want just under 72h", total)
	}

	now := time.Date(2026, time.March, 2, 10, 0, 0, 0, time.UTC)
	failed := app.Result{Status: 500, Error: "the endpoint answered 500"}
	for _, c := range []struct {
		attempts int
		r        app.Result
		want     app.Attempt
	}{
		{0, app.Result{Delivered: true, Status: 200}, app.Attempt{N: 1, Status: app.StatusDelivered, Due: now}},
		{0, failed, app.Attempt{N: 1, Status: app.StatusPending, Due: now.Add(app.Backoff(1))}},
		{7, failed, app.Attempt{N: 8, Status: app.StatusPending, Due: now.Add(app.Backoff(8))}},
		{8, failed, app.Attempt{N: 9, Status: app.StatusFailed, Due: now}},
	} {
		c.want.Result, c.want.At = c.r, now
		if got := app.Attempted(app.Delivery{Attempts: c.attempts}, c.r, now); got != c.want {
			t.Errorf("after %d attempts (%+v): %+v, want %+v", c.attempts, c.r, got, c.want)
		}
	}
}

// failing is a sender that fails with err.
type failing struct{ err error }

func (f failing) Do(*http.Request) (*http.Response, error) { return nil, f.err }

// TestAFailureIsKeptAsValidText: an error longer than what is kept is cut without splitting a character, and loses its
// NUL and invalid bytes, since it quotes what the integrator chose and the database takes only valid UTF-8 without
// NUL.
func TestAFailureIsKeptAsValidText(t *testing.T) {
	secret, err := app.NewSecret()
	if err != nil {
		t.Fatal(err)
	}
	d := app.Delivery{ID: ids.New(), URL: "https://hooks.example.com/", Secret: secret}
	// one of the prefixes puts a two-byte ñ across the cut
	for _, prefix := range []string{"", "a"} {
		r := app.Send(context.Background(), failing{errors.New(prefix + strings.Repeat("ñ", 600))}, d, time.Now())
		if !utf8.ValidString(r.Error) || len(r.Error) > 1000 || len(r.Error) < 998 || r.Delivered {
			t.Errorf("prefix %q: error of %d bytes, valid %v", prefix, len(r.Error), utf8.ValidString(r.Error))
		}
	}
	// a certificate valid for a name with a NUL, as Go reports it on Linux, and an invalid byte
	r := app.Send(context.Background(), failing{errors.New("x509: certificate is valid for evil\x00name\xff, not localhost")}, d, time.Now())
	if r.Error != "x509: certificate is valid for evilname, not localhost" {
		t.Errorf("error %q", r.Error)
	}
}
