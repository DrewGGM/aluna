package simrail

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"

	"github.com/DrewGGM/aluna/internal/money"
	"github.com/DrewGGM/aluna/internal/payments/internal/app"
)

var (
	secret   = []byte("a shared secret of the rail")
	signedAt = time.Date(2026, time.March, 2, 9, 0, 0, 0, time.UTC)
)

// received is a notification as the core's endpoint gets it, and when.
type received struct {
	path, id, timestamp, signature string
	body                           []byte
	at                             time.Time
}

// receiver is the core's notification endpoint: it passes each notification to the test.
func receiver(t *testing.T) (*httptest.Server, <-chan received) {
	t.Helper()
	got := make(chan received, 4)
	ts := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Error(err)
		}
		got <- received{
			path: r.URL.Path, id: r.Header.Get("webhook-id"), timestamp: r.Header.Get("webhook-timestamp"),
			signature: r.Header.Get("webhook-signature"), body: body, at: time.Now(),
		}
	}))
	t.Cleanup(ts.Close)
	return ts, got
}

// simulator is the rail notifying ts, signing at signedAt.
func simulator(ts *httptest.Server, log *slog.Logger) *Simulator {
	rail := New(ts.URL, secret, ts.Client(), log)
	rail.now = func() time.Time { return signedAt }
	return rail
}

func discard() *slog.Logger { return slog.New(slog.DiscardHandler) }

// message is a transfer of pesos COP whose end-to-end id ends in n.
func message(t *testing.T, pesos int64, n int) app.OutboundMessage {
	t.Helper()
	cop, err := money.CurrencyOf("COP")
	if err != nil {
		t.Fatal(err)
	}
	return app.OutboundMessage{
		EndToEndID: "ALUNA" + strconv.Itoa(n), InstrID: "INSTR1", TxID: "TX" + strconv.Itoa(n), Amount: money.New(pesos*100, cop),
		Debtor: app.AccountRef{Number: "123456789"}, Creditor: app.AccountRef{Key: "@beneficiary"},
	}
}

// notified waits for a notification, or fails after 5 seconds.
func notified(t *testing.T, got <-chan received) (received, notification) {
	t.Helper()
	select {
	case r := <-got:
		var n notification
		if err := json.Unmarshal(r.body, &n); err != nil {
			t.Fatalf("notification %s: %v", r.body, err)
		}
		return r, n
	case <-time.After(5 * time.Second):
		t.Fatal("no notification after 5 s")
		return received{}, notification{}
	}
}

// nothingFor checks that no notification comes for d.
func nothingFor(t *testing.T, got <-chan received, d time.Duration) {
	t.Helper()
	select {
	case r := <-got:
		t.Fatalf("notified %s too soon", r.body)
	case <-time.After(d):
	}
}

// TestTheSignatureIsStandardWebhooks: the signature of the Standard Webhooks test vector is the one its spec
// publishes.
func TestTheSignatureIsStandardWebhooks(t *testing.T) {
	key, err := base64.StdEncoding.DecodeString("MfKQ9r8GKYqrTwjUPD8ILPZIo2LaLaSw")
	if err != nil {
		t.Fatal(err)
	}

	got := sign(key, "msg_p5jXN8AQM9LWM0D4loKWxJek", "1614265330", []byte(`{"test": 2432232314}`))

	if want := "g0hM9SsE+OTPJTGt/tmIKtSyZlE3uFJELVlNIOLJ1OE="; got != want {
		t.Fatalf("signature = %q, want %q", got, want)
	}
}

// TestTheRailTakesAMessageAndNotifiesItsOutcome (FR-31): the rail takes a message and answers at once with its
// reference; the outcome then comes to /v1/rails/simbreb/notifications, signed with the rail's reference as id
// at the time of signing: accepted, or rejected as AC04 for an amount ending in 13 pesos.
func TestTheRailTakesAMessageAndNotifiesItsOutcome(t *testing.T) {
	ts, got := receiver(t)
	rail := simulator(ts, discard())
	cases := []struct {
		pesos  int64
		status app.RailStatus
		reason string
	}{{50000, app.RailAccepted, ""}, {50013, app.RailRejected, "AC04"}}

	for i, tc := range cases {
		msg := message(t, tc.pesos, i)
		ack, err := rail.Send(t.Context(), msg)
		if err != nil || ack.RailRef == "" {
			t.Fatalf("send %d = %+v (%v), want the rail's reference", tc.pesos, ack, err)
		}
		r, n := notified(t, got)
		want := notification{EndToEndID: msg.EndToEndID, TxID: msg.TxID, RailRef: ack.RailRef, Status: tc.status, Reason: tc.reason}
		if n != want {
			t.Errorf("notification of %d = %+v, want %+v", tc.pesos, n, want)
		}
		if r.path != "/v1/rails/"+rail.Name()+"/notifications" || r.id != ack.RailRef || r.timestamp != strconv.FormatInt(signedAt.Unix(), 10) {
			t.Errorf("notification of %d went to %s as %q at %s", tc.pesos, r.path, r.id, r.timestamp)
		}
		if r.signature != "v1,"+sign(secret, r.id, r.timestamp, r.body) {
			t.Errorf("notification of %d signed %q", tc.pesos, r.signature)
		}
	}
}

// TestAnAmountEndingIn99IsSlow (FR-31): the rail takes a message of an amount ending in 99 pesos after its
// delay (over 20 seconds) and notifies its outcome then. A sender that stops waiting gets its context's error,
// and the notification still comes, after the delay: until then the sender does not know the outcome.
func TestAnAmountEndingIn99IsSlow(t *testing.T) {
	ts, got := receiver(t)
	rail := simulator(ts, discard())
	if rail.delay <= 20*time.Second {
		t.Fatalf("the rail's delay is %s, want over 20 s", rail.delay)
	}
	rail.delay = 300 * time.Millisecond

	start := time.Now()
	if _, err := rail.Send(t.Context(), message(t, 50099, 1)); err != nil || time.Since(start) < rail.delay {
		t.Fatalf("a slow send = %v after %s, want it taken after %s", err, time.Since(start), rail.delay)
	}
	if r, _ := notified(t, got); r.at.Sub(start) < rail.delay {
		t.Fatalf("notified %s after sending, before the delay", r.at.Sub(start))
	}
	impatient, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
	defer cancel()
	start = time.Now()
	_, err := rail.Send(impatient, message(t, 50099, 2))

	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("a sender that stopped waiting = %v, want DeadlineExceeded", err)
	}
	nothingFor(t, got, rail.delay-time.Since(start)-100*time.Millisecond)
	notified(t, got)
}

// TestAMessageTheRailDoesNotTake (FR-31): a message without end-to-end id, with an amount that is not
// positive, or whose debtor or creditor is not one account or one key, is app.ErrRailRefused; a sender that
// already gave up sends nothing. Neither is notified.
func TestAMessageTheRailDoesNotTake(t *testing.T) {
	ts, got := receiver(t)
	rail := simulator(ts, discard())
	cases := map[string]func(*app.OutboundMessage){
		"no end-to-end id":       func(m *app.OutboundMessage) { m.EndToEndID = "" },
		"zero":                   func(m *app.OutboundMessage) { m.Amount = money.New(0, m.Amount.Currency()) },
		"negative":               func(m *app.OutboundMessage) { m.Amount = money.New(-5001300, m.Amount.Currency()) },
		"a debtor of nowhere":    func(m *app.OutboundMessage) { m.Debtor = app.AccountRef{} },
		"a creditor of two ways": func(m *app.OutboundMessage) { m.Creditor = app.AccountRef{Number: "1", Key: "@k"} },
	}
	for name, change := range cases {
		msg := message(t, 50000, 1)
		change(&msg)
		if _, err := rail.Send(t.Context(), msg); !errors.Is(err, app.ErrRailRefused) {
			t.Errorf("%s = %v, want ErrRailRefused", name, err)
		}
	}
	gone, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := rail.Send(gone, message(t, 50000, 2)); !errors.Is(err, context.Canceled) {
		t.Errorf("a sender that gave up = %v, want Canceled", err)
	}
	nothingFor(t, got, 200*time.Millisecond)
}

// TestANotificationTheCoreDoesNotTakeIsLogged (FR-31): a notification the core answers with an error is
// logged, with the rail's reference.
func TestANotificationTheCoreDoesNotTakeIsLogged(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusUnauthorized) }))
	t.Cleanup(ts.Close)
	logged := make(chan slog.Record, 1)
	rail := simulator(ts, slog.New(handlerFunc(func(r slog.Record) { logged <- r })))

	ack, err := rail.Send(t.Context(), message(t, 50000, 1))
	if err != nil {
		t.Fatal(err)
	}

	select {
	case r := <-logged:
		attrs := map[string]string{}
		r.Attrs(func(a slog.Attr) bool { attrs[a.Key] = a.Value.String(); return true })
		if r.Message != "deliver rail notification" || attrs["rail_ref"] != ack.RailRef {
			t.Fatalf("logged %q with %v, want the delivery and rail_ref %s", r.Message, attrs, ack.RailRef)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("nothing logged after 5 s")
	}
}

// handlerFunc is a slog handler that passes each record to a function.
type handlerFunc func(slog.Record)

func (h handlerFunc) Enabled(context.Context, slog.Level) bool { return true }

func (h handlerFunc) Handle(_ context.Context, r slog.Record) error {
	h(r)
	return nil
}

func (h handlerFunc) WithAttrs([]slog.Attr) slog.Handler { return h }

func (h handlerFunc) WithGroup(string) slog.Handler { return h }

// deaf is a rail that keeps the deadline its Send got and, with ended, does not return until ended closes,
// whatever its context says, or 5 seconds pass: a regression fails the test instead of hanging it.
type deaf struct {
	at    time.Time
	ended chan struct{}
}

func (d *deaf) Name() string { return "deaf" }

func (d *deaf) Send(ctx context.Context, _ app.OutboundMessage) (app.SendAck, error) {
	d.at, _ = ctx.Deadline()
	if d.ended != nil {
		select {
		case <-d.ended:
		case <-time.After(5 * time.Second):
		}
	}
	return app.SendAck{}, nil
}

// TestSendWaitsForTheRail15Seconds (FR-31): the core gives a rail 15 seconds to take a message, and stops
// waiting when its context ends even if the rail does not return.
func TestSendWaitsForTheRail15Seconds(t *testing.T) {
	rail := &deaf{}
	before := time.Now()
	if _, err := app.Send(t.Context(), rail, message(t, 50000, 1)); err != nil {
		t.Fatal(err)
	}
	if waited := rail.at.Sub(before); waited < 15*time.Second || waited > 16*time.Second {
		t.Fatalf("the rail had %s, want 15 s", waited)
	}
	holding := &deaf{ended: make(chan struct{})}
	t.Cleanup(func() { close(holding.ended) })
	impatient, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
	defer cancel()

	start := time.Now()
	_, err := app.Send(impatient, holding, message(t, 50000, 2))

	if !errors.Is(err, context.DeadlineExceeded) || time.Since(start) > time.Second {
		t.Fatalf("a rail that does not return = %v after %s, want DeadlineExceeded at once", err, time.Since(start))
	}
}
