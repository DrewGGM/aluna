//go:build integration

package webhooks_test

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/DrewGGM/aluna/internal/ids"
	"github.com/DrewGGM/aluna/internal/postgres"
	"github.com/DrewGGM/aluna/internal/postgres/pgtest"
	"github.com/DrewGGM/aluna/internal/webhooks"
	pg "github.com/DrewGGM/aluna/internal/webhooks/internal/adapters/postgres"
	"github.com/DrewGGM/aluna/internal/webhooks/internal/app"
)

// testSecret is the secret of the endpoints these tests register: whsec_ and a key of 32 bytes.
var testSecret = "whsec_" + base64.StdEncoding.EncodeToString([]byte("0123456789abcdef0123456789abcdef"))

// receiver is an endpoint that refuses the first failures requests and then takes them, and checks each signature as
// a receiver would, by the Standard Webhooks rules and not with Aluna's code.
type receiver struct {
	mu         sync.Mutex
	failures   int
	ids        []string
	timestamps []int64
	bodies     [][]byte
	bad        []string
}

func (r *receiver) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	body, err := io.ReadAll(req.Body)
	r.mu.Lock()
	defer r.mu.Unlock()
	id, timestamp := req.Header.Get("webhook-id"), req.Header.Get("webhook-timestamp")
	key, _ := base64.StdEncoding.DecodeString(strings.TrimPrefix(testSecret, "whsec_"))
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte(id + "." + timestamp + "." + string(body)))
	if want := "v1," + base64.StdEncoding.EncodeToString(mac.Sum(nil)); err != nil || req.Header.Get("webhook-signature") != want {
		r.bad = append(r.bad, req.Header.Get("webhook-signature"))
	}
	r.ids, r.bodies = append(r.ids, id), append(r.bodies, body)
	if at, err := strconv.ParseInt(timestamp, 10, 64); err == nil {
		r.timestamps = append(r.timestamps, at)
	}
	if len(r.ids) <= r.failures {
		w.WriteHeader(http.StatusServiceUnavailable)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// endpoint registers for a new entity an endpoint at url with testSecret, as the owner: the registration refuses the
// test server's loopback address.
func endpoint(t *testing.T, url string) (entity, id ids.ID) {
	t.Helper()
	entity, id = ids.New(), ids.New()
	pgtest.AsOwner(t, testPool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(t.Context(), `INSERT INTO calendar.legal_entities (id, name, business_date, functional_currency)
			VALUES ($1, 'Entity', '2026-03-02', 'COP')`, entity); err != nil {
			return err
		}
		_, err := tx.Exec(t.Context(), `INSERT INTO webhooks.endpoints (id, legal_entity_id, url, secret, created_by) VALUES ($1, $2, $3, $4, 'tester')`,
			id, entity, url, testSecret)
		return err
	})
	return entity, id
}

// at is a clock that reads t.
func at(t time.Time) func() time.Time { return func() time.Time { return t } }

// delivery is where an endpoint's delivery stands.
type delivery struct {
	status   string
	attempts int
	due      time.Time
	lastErr  string
}

func deliveryOf(t *testing.T, endpointID ids.ID) (delivery, bool) {
	t.Helper()
	var d delivery
	var lastErr *string
	err := testPool.QueryRow(t.Context(), `SELECT status, attempts, next_attempt_at, last_error FROM webhooks.deliveries WHERE endpoint_id = $1`,
		endpointID).Scan(&d.status, &d.attempts, &d.due, &lastErr)
	if errors.Is(err, pgx.ErrNoRows) {
		return delivery{}, false
	}
	if err != nil {
		t.Fatal(err)
	}
	if lastErr != nil {
		d.lastErr = *lastErr
	}
	return d, true
}

// deliverAt runs the delivery at now until the endpoint's delivery has had attempts attempts: its event shows up once
// the transactions older than its own have ended.
func deliverAt(t *testing.T, sender webhooks.Sender, endpointID ids.ID, now time.Time, attempts int) delivery {
	t.Helper()
	for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
		if err := webhooks.New().Deliver(t.Context(), postgres.NewUnitOfWork(testPool), sender, at(now), 50); err != nil {
			t.Fatal(err)
		}
		if d, ok := deliveryOf(t, endpointID); ok && d.attempts >= attempts {
			return d
		}
	}
	t.Fatalf("endpoint %s has no attempt %d", endpointID, attempts)
	return delivery{}
}

// TestAWebhookIsRetriedUntilTheEndpointTakesIt (FR-46, US-15): an endpoint that fails 3 times gets the event signed,
// with the same webhook-id, every time its backoff is due, and the fourth time takes it.
func TestAWebhookIsRetriedUntilTheEndpointTakesIt(t *testing.T) {
	r := &receiver{failures: 3}
	server := httptest.NewTLSServer(r)
	defer server.Close()
	entity, endpointID := endpoint(t, server.URL+"/hooks")
	emitted := emit(t, entity, opened)

	now := time.Date(2026, time.March, 2, 10, 0, 0, 0, time.UTC)
	d := deliverAt(t, server.Client(), endpointID, now, 1)
	if wait := (72 * time.Hour / 255).Truncate(time.Second); d.status != "pending" || !d.due.Equal(now.Add(wait)) || d.lastErr == "" {
		t.Fatalf("after the first attempt %+v, want pending until %s", d, now.Add(wait))
	}
	for attempt := 2; attempt <= 4; attempt++ {
		d = deliverAt(t, server.Client(), endpointID, d.due, attempt)
	}

	if d, _ := deliveryOf(t, endpointID); d.status != "delivered" || d.attempts != 4 {
		t.Fatalf("delivery %+v, want delivered on the fourth attempt", d)
	}
	r.expectDelivered(t, emitted[0], 4)
	var after ids.ID
	if err := testPool.QueryRow(t.Context(), `SELECT after_id FROM webhooks.endpoints WHERE id = $1`, endpointID).Scan(&after); err != nil || after != emitted[0] {
		t.Fatalf("the endpoint got to %s (%v), want its event", after, err)
	}
}

// blocking is a sender whose first request waits until release is closed, after closing sending.
type blocking struct {
	inner            webhooks.Sender
	sending, release chan struct{}
	once             sync.Once
}

func (b *blocking) Do(r *http.Request) (*http.Response, error) {
	b.once.Do(func() {
		close(b.sending)
		<-b.release
	})
	return b.inner.Do(r)
}

// TestADeliveryInFlightIsNotSentTwice: while a run sends a delivery, another run at the same time does not take it.
func TestADeliveryInFlightIsNotSentTwice(t *testing.T) {
	r := &receiver{}
	server := httptest.NewTLSServer(r)
	defer server.Close()
	entity, endpointID := endpoint(t, server.URL)
	read(t, entity, "", "", 50, len(emit(t, entity, opened))) // the event shows up
	now := time.Date(2026, time.March, 2, 10, 0, 0, 0, time.UTC)
	slow := &blocking{inner: server.Client(), sending: make(chan struct{}), release: make(chan struct{})}
	first := make(chan error, 1)
	go func() {
		first <- webhooks.New().Deliver(t.Context(), postgres.NewUnitOfWork(testPool), slow, at(now), 50)
	}()
	<-slow.sending

	err := webhooks.New().Deliver(t.Context(), postgres.NewUnitOfWork(testPool), server.Client(), at(now), 50)
	close(slow.release)

	if err != nil {
		t.Fatal(err)
	}
	if err := <-first; err != nil {
		t.Fatal(err)
	}
	if d, _ := deliveryOf(t, endpointID); d.status != "delivered" || d.attempts != 1 {
		t.Fatalf("delivery %+v, want delivered once", d)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.ids) != 1 {
		t.Fatalf("the endpoint got %d requests, want 1", len(r.ids))
	}
}

// expectDelivered checks that the receiver got event attempts times, always as the same webhook, signed.
func (r *receiver) expectDelivered(t *testing.T, event ids.ID, attempts int) {
	t.Helper()
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.ids) != attempts || r.ids[0] != r.ids[attempts-1] || len(r.bad) != 0 {
		t.Fatalf("webhook ids %v, bad signatures %v", r.ids, r.bad)
	}
	var got struct{ ID, Type string }
	if err := json.Unmarshal(r.bodies[attempts-1], &got); err != nil || got.ID != event.String() || got.Type != opened {
		t.Fatalf("delivered %s (%v)", r.bodies[attempts-1], err)
	}
}

// TestAWebhookFailsAfterEightRetries: an endpoint that never takes it is attempted 9 times in 72 hours, and then the
// delivery is failed and not attempted again.
func TestAWebhookFailsAfterEightRetries(t *testing.T) {
	r := &receiver{failures: 100}
	server := httptest.NewTLSServer(r)
	defer server.Close()
	entity, endpointID := endpoint(t, server.URL)
	emit(t, entity, settled)

	start := time.Date(2026, time.March, 2, 10, 0, 0, 0, time.UTC)
	d, last := deliverAt(t, server.Client(), endpointID, start, 1), start
	for attempt := 2; attempt <= 9; attempt++ {
		if d.due.Sub(last) <= 0 {
			t.Fatalf("attempt %d due at %s, not after %s", attempt, d.due, last)
		}
		last = d.due
		d = deliverAt(t, server.Client(), endpointID, d.due, attempt)
	}
	if err := webhooks.New().Deliver(t.Context(), postgres.NewUnitOfWork(testPool), server.Client(), at(last.Add(30*24*time.Hour)), 50); err != nil {
		t.Fatal(err)
	}

	if d, _ := deliveryOf(t, endpointID); d.status != "failed" || d.attempts != 9 || last.Sub(start) > 72*time.Hour || last.Sub(start) < 71*time.Hour {
		t.Fatalf("delivery %+v, last attempt %s after the first; want failed after 9 attempts in 72h", d, last.Sub(start))
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.ids) != 9 {
		t.Fatalf("%d attempts reached the endpoint, want 9", len(r.ids))
	}
}

// TestAWebhookDoesNotReachThePrivateNetwork (SSRF): the sender does not connect to an address that is not public,
// even if the endpoint got registered with one, and does not follow redirects.
func TestAWebhookDoesNotReachThePrivateNetwork(t *testing.T) {
	r := &receiver{}
	server := httptest.NewTLSServer(r)
	defer server.Close()
	entity, endpointID := endpoint(t, server.URL)
	emit(t, entity, opened)

	d := deliverAt(t, webhooks.NewSender(), endpointID, time.Now(), 1)

	if d.status != "pending" || !strings.Contains(d.lastErr, "not a public address") {
		t.Fatalf("delivery %+v, want refused before connecting", d)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.ids) != 0 {
		t.Fatalf("the private endpoint got %d requests", len(r.ids))
	}
}

// TestAnEndpointIsRegisteredWithASecret (FR-46): a public https URL gets a whsec_ secret; a private one is refused.
func TestAnEndpointIsRegisteredWithASecret(t *testing.T) {
	entity, _ := endpoint(t, "https://93.184.216.34/other")
	var registered webhooks.Endpoint
	err := postgres.NewUnitOfWork(testPool).Do(t.Context(), func(tx postgres.Tx) error {
		var err error
		registered, err = webhooks.New().RegisterEndpoint(t.Context(), tx, entity, "https://93.184.216.34/hooks", "integrator-a")
		return err
	})
	if err != nil || !strings.HasPrefix(registered.Secret, "whsec_") || registered.URL != "https://93.184.216.34/hooks" || registered.CreatedAt.IsZero() {
		t.Fatalf("registered %+v, %v", registered, err)
	}
	for _, url := range []string{"https://127.0.0.1/hooks", "https://169.254.169.254/latest/meta-data/", "http://93.184.216.34/hooks"} {
		err := postgres.NewUnitOfWork(testPool).Do(t.Context(), func(tx postgres.Tx) error {
			_, err := webhooks.New().RegisterEndpoint(t.Context(), tx, entity, url, "integrator-a")
			return err
		})
		if !errors.Is(err, webhooks.ErrInvalidEndpoint) {
			t.Errorf("%s: %v, want ErrInvalidEndpoint", url, err)
		}
	}
}

// TestAnAttemptIsRecordedOnce: a run whose claim expired does not overwrite the attempt another run recorded.
func TestAnAttemptIsRecordedOnce(t *testing.T) {
	r := &receiver{failures: 100}
	server := httptest.NewTLSServer(r)
	defer server.Close()
	entity, endpointID := endpoint(t, server.URL)
	emit(t, entity, opened)
	now := time.Date(2026, time.March, 2, 10, 0, 0, 0, time.UTC)
	before := deliverAt(t, server.Client(), endpointID, now, 1)
	var id ids.ID
	if err := testPool.QueryRow(t.Context(), `SELECT id FROM webhooks.deliveries WHERE endpoint_id = $1`, endpointID).Scan(&id); err != nil {
		t.Fatal(err)
	}

	stale := app.Attempt{N: 1, Result: app.Result{Delivered: true, Status: 204}, Status: app.StatusDelivered, Due: now, At: now}
	if err := pg.NewStore(testPool).Record(t.Context(), id, stale); err != nil {
		t.Fatal(err)
	}

	if after, _ := deliveryOf(t, endpointID); after != before {
		t.Fatalf("delivery %+v after a stale record, want it as it was: %+v", after, before)
	}
}

// ticking is a clock that moves a minute every time it is read.
type ticking struct {
	mu  sync.Mutex
	now time.Time
}

func (c *ticking) read() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(time.Minute)
	return c.now
}

// TestAnAttemptIsSignedWhenItIsSent: a run reads the time at each step, so the signature carries the time of its POST
// and the next attempt counts from when the answer came, not from when the run started.
func TestAnAttemptIsSignedWhenItIsSent(t *testing.T) {
	r := &receiver{failures: 1}
	server := httptest.NewTLSServer(r)
	defer server.Close()
	entity, endpointID := endpoint(t, server.URL)
	read(t, entity, "", "", 50, len(emit(t, entity, opened))) // the event shows up
	clock := &ticking{now: time.Date(2026, time.March, 2, 10, 0, 0, 0, time.UTC)}

	if err := webhooks.New().Deliver(t.Context(), postgres.NewUnitOfWork(testPool), server.Client(), clock.read, 50); err != nil {
		t.Fatal(err)
	}

	d, _ := deliveryOf(t, endpointID)
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.timestamps) != 1 || d.attempts != 1 {
		t.Fatalf("%d requests, delivery %+v", len(r.timestamps), d)
	}
	signedAt := time.Unix(r.timestamps[0], 0).UTC()
	// enqueue, claim and sign read the clock in that order, and the attempt reads it once more after the answer
	if want := clock.now.Add(-time.Minute); !signedAt.Equal(want) || !d.due.Equal(clock.now.Add((72 * time.Hour / 255).Truncate(time.Second))) {
		t.Fatalf("signed at %s, due %s; want signed at %s and due from %s", signedAt, d.due, want, clock.now)
	}
}
