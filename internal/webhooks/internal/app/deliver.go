package app

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/DrewGGM/aluna/internal/ids"
)

// MaxRetries is how many times a delivery is retried after its first attempt (FR-46), over retryWindow: then it is
// failed.
const MaxRetries = 8

// retryWindow is the time the retries of a delivery add up to.
const retryWindow = 72 * time.Hour

// Status is where a delivery stands.
type Status string

// Statuses of a delivery.
const (
	StatusPending   Status = "pending"
	StatusDelivered Status = "delivered"
	StatusFailed    Status = "failed"
)

// Subscription is a registered endpoint as the delivery sees it: whose events it gets and how far it got.
type Subscription struct {
	EndpointID, LegalEntityID ids.ID
	After                     Position
}

// Queued is a delivery to create: an event for an endpoint, with the body it is sent with.
type Queued struct {
	EventID ids.ID
	Body    []byte
}

// Queue is what queuing an endpoint's events needs, within the transaction that queues them.
type Queue interface {
	EventReader
	// Queue creates s's deliveries of the events, due at due, and moves s on to after.
	Queue(ctx context.Context, s Subscription, deliveries []Queued, due time.Time, after Position) error
}

// Enqueue queues for subscription s the next batch of events of its entity, in the Events API's order, due at now.
func Enqueue(ctx context.Context, q Queue, s Subscription, now time.Time, batch int) error {
	events, err := q.EventsAfter(ctx, s.LegalEntityID, "", s.After, batch)
	if err != nil || len(events) == 0 {
		return err
	}
	deliveries := make([]Queued, len(events))
	for i, ev := range events {
		body, err := json.Marshal(ev.StoredEvent)
		if err != nil {
			return fmt.Errorf("event %s: %w", ev.ID, err)
		}
		deliveries[i] = Queued{EventID: ev.ID, Body: body}
	}
	last := events[len(events)-1]
	return q.Queue(ctx, s, deliveries, now, Position{Xact: last.Xact, ID: last.ID})
}

// Delivery is a delivery to attempt: where, signed with what, what, and how many attempts it had.
type Delivery struct {
	ID          ids.ID
	URL, Secret string
	Body        []byte
	Attempts    int
}

// Result is what an attempt got: whether the endpoint took it (2xx), its HTTP status if it answered, and why it
// failed.
type Result struct {
	Delivered bool
	Status    int
	Error     string
}

// Sender sends HTTP requests.
type Sender interface {
	Do(r *http.Request) (*http.Response, error)
}

// maxError is the most of an attempt's error that is kept, in bytes.
const maxError = 1000

// Send POSTs d's body to its URL, signed at at (Standard Webhooks), and returns what it got.
func Send(ctx context.Context, s Sender, d Delivery, at time.Time) Result {
	signed, err := Sign(d.Secret, d.ID.String(), at, d.Body)
	if err != nil {
		return failure(0, err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, d.URL, bytes.NewReader(d.Body))
	if err != nil {
		return failure(0, err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("webhook-id", signed.ID)
	req.Header.Set("webhook-timestamp", signed.Timestamp)
	req.Header.Set("webhook-signature", signed.Signature)
	resp, err := s.Do(req)
	if err != nil {
		return failure(0, err)
	}
	defer resp.Body.Close()                                        //nolint:errcheck // the body is only drained
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64*1024)) // drained so the connection is reused
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return failure(resp.StatusCode, fmt.Errorf("the endpoint answered %d", resp.StatusCode))
	}
	return Result{Delivered: true, Status: resp.StatusCode}
}

// failure is a failed attempt with err's text as the database takes it: valid UTF-8 without NUL, of up to maxError
// bytes. The text quotes what the endpoint answered, its certificate and its URL, which the integrator chooses.
func failure(status int, err error) Result {
	text := strings.ReplaceAll(strings.ToValidUTF8(err.Error(), ""), "\x00", "")
	if len(text) > maxError {
		text = strings.ToValidUTF8(text[:maxError], "")
	}
	return Result{Status: status, Error: text}
}

// Attempt is an attempt of a delivery: its number, what it got, and where the delivery stands after it, due when.
type Attempt struct {
	N      int
	Result Result
	Status Status
	Due    time.Time
	At     time.Time
}

// Attempted is d's next attempt, which got r at at: delivered; failed once it was retried MaxRetries times; or pending,
// due after an exponential backoff whose MaxRetries waits add up to just under 72 hours.
func Attempted(d Delivery, r Result, at time.Time) Attempt {
	a := Attempt{N: d.Attempts + 1, Result: r, Status: StatusPending, At: at, Due: at}
	switch {
	case r.Delivered:
		a.Status = StatusDelivered
	case a.N > MaxRetries:
		a.Status = StatusFailed
	default:
		a.Due = at.Add(Backoff(a.N))
	}
	return a
}

// firstWait is the wait before the first retry, retryWindow/255 in whole seconds: the MaxRetries waits, doubling from
// it, add up to just under retryWindow.
var firstWait = (retryWindow / (1<<MaxRetries - 1)).Truncate(time.Second)

// Backoff is the wait before the retry-th retry: it doubles from firstWait.
func Backoff(retry int) time.Duration { return firstWait << (retry - 1) }
