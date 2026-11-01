//go:build integration

package payments_test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/riverdriver/riverpgxv5"
	"github.com/riverqueue/river/rivertype"

	"github.com/DrewGGM/aluna/internal/calendar"
	"github.com/DrewGGM/aluna/internal/ids"
	"github.com/DrewGGM/aluna/internal/money"
	"github.com/DrewGGM/aluna/internal/outbox"
	"github.com/DrewGGM/aluna/internal/payments"
	"github.com/DrewGGM/aluna/internal/postgres"
	"github.com/DrewGGM/aluna/internal/products"
)

// railSecret is the secret the test rail signs its notifications with.
var railSecret = []byte("a secret of the simulated rail, for the tests")

// testRail is the simulated rail, notifying the test. If answer is set, the rail gives it instead of taking a
// message; if outcome is set, the core reads that instead of the notification. sends counts the messages.
type testRail struct {
	payments.Rail
	notified chan payments.Notification
	answer   func() error
	outcome  func(payments.Notification) (payments.RailOutcome, error)
	sends    atomic.Int32
}

// newTestRail is the simulated rail notifying a test server, which passes each notification to the test.
func newTestRail(t *testing.T) *testRail {
	t.Helper()
	r := &testRail{notified: make(chan payments.Notification, 8)}
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		body, err := io.ReadAll(req.Body)
		if err != nil {
			t.Error(err)
		}
		r.notified <- payments.Notification{Header: req.Header.Clone(), Body: body}
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(ts.Close)
	r.Rail = payments.NewSimulatedRail(ts.URL, railSecret, ts.Client(), slog.New(slog.DiscardHandler))
	return r
}

func (r *testRail) Send(ctx context.Context, msg payments.OutboundMessage) (payments.SendAck, error) {
	r.sends.Add(1)
	if r.answer != nil {
		return payments.SendAck{}, r.answer()
	}
	return r.Rail.Send(ctx, msg)
}

func (r *testRail) Outcome(n payments.Notification) (payments.RailOutcome, error) {
	if r.outcome != nil {
		return r.outcome(n)
	}
	return r.Rail.Outcome(n)
}

// notification waits up to 5 seconds for the rail's next notification.
func (r *testRail) notification(t *testing.T) payments.Notification {
	t.Helper()
	select {
	case n := <-r.notified:
		return n
	case <-time.After(5 * time.Second):
		t.Fatal("the rail did not notify in 5 s")
		return payments.Notification{}
	}
}

// send sends order id as payments.send does.
func (w world) send(t *testing.T, id ids.ID) error {
	t.Helper()
	return w.payments.Send(t.Context(), postgres.NewUnitOfWork(testPool), id)
}

// take reads notification n, as the rails' server does, and takes its outcome in its own transaction.
func (w world) take(t *testing.T, n payments.Notification) error {
	t.Helper()
	out, err := w.payments.ReadNotification(n)
	if err != nil {
		return err
	}
	return do(t, func(tx postgres.Tx) error { return w.payments.TakeOutcome(t.Context(), tx, out) })
}

// sentOrder is a payment of units initiated, held and sent to the rail.
func (w world) sentOrder(t *testing.T, units int64) payments.Order {
	t.Helper()
	o, err := w.initiate(t, w.input(t, units, "@beneficiary"))
	if err != nil || o.Status != payments.StatusHeld {
		t.Fatalf("initiate = %+v (%v), want it held", o, err)
	}
	if err := w.send(t, o.ID); err != nil {
		t.Fatal(err)
	}
	return o
}

// posted are the units the payment entries of the world's entity post to each account (the customer's deposit
// account is "customer"), and how many transactions they are.
func (w world) posted(t *testing.T) (map[string]int64, int) {
	t.Helper()
	rows, err := testPool.Query(t.Context(), `SELECT CASE WHEN a.id = $2 THEN 'customer' ELSE a.code END, e.amount, t.id
		FROM ledger.entries e JOIN ledger.transactions t ON t.id = e.transaction_id JOIN ledger.accounts a ON a.id = e.account_id
		WHERE t.legal_entity_id = $1 AND t.code IN ('hold_capture', 'payments.charges')`, w.entity, w.account.LedgerAccountID)
	if err != nil {
		t.Fatal(err)
	}
	units, transactions := map[string]int64{}, map[ids.ID]bool{}
	var (
		account string
		amount  int64
		tx      ids.ID
	)
	if _, err := pgx.ForEachRow(rows, []any{&account, &amount, &tx}, func() error {
		units[account] += amount
		transactions[tx] = true
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	return units, len(transactions)
}

func holdStatus(t *testing.T, id ids.ID) string {
	t.Helper()
	var status string
	if err := testPool.QueryRow(t.Context(), `SELECT status FROM ledger.holds WHERE id = $1`, id).Scan(&status); err != nil {
		t.Fatal(err)
	}
	return status
}

func railRefAndReason(t *testing.T, id ids.ID) (railRef, reason string) {
	t.Helper()
	if err := testPool.QueryRow(t.Context(), `SELECT coalesce(rail_ref, ''), coalesce(reason, '') FROM payments.orders WHERE id = $1`, id).
		Scan(&railRef, &reason); err != nil {
		t.Fatal(err)
	}
	return railRef, reason
}

func events(types ...string) []string {
	all := []string{"aluna.payments.order.initiated.v1", "aluna.payments.order.held.v1"}
	for _, typ := range types {
		all = append(all, "aluna.payments.order."+typ+".v1")
	}
	return all
}

// TestAnAcceptedPaymentSettles (FR-31): an order is sent before the rail takes it; when the rail accepts it,
// it settles: its hold is captured and the customer is debited, by their natural debit, its amount to the
// rail's clearing account, its fee to the product's fee income and its GMF to the GMF payable account.
func TestAnAcceptedPaymentSettles(t *testing.T) {
	w := worldOf(t, "valid-tiered-savings.json", true)
	o := w.sentOrder(t, 30000000)
	if s := storedOrder(t, o.ID); s.status != "sent" || !slices.Equal(eventsOf(t, o.ID), events("sent")) {
		t.Fatalf("sent order = %+v, events %v; want it sent and published", s, eventsOf(t, o.ID))
	}

	if err := w.take(t, w.rail.notification(t)); err != nil {
		t.Fatal(err)
	}

	expectSettled(t, o)
	total := 30000000 + 300000 + o.GMF.Units()
	want := map[string]int64{"customer": total, "L.CLEARING.SIMBREB": -30000000, "I.FEES.TIERED": -300000, "L.TAX.GMF": -o.GMF.Units()}
	if units, transactions := w.posted(t); !maps.Equal(units, want) || transactions != 2 || o.GMF.Units() <= 0 {
		t.Fatalf("posted %v in %d transactions, want %v in the capture and the charges", units, transactions, want)
	}
	if b := w.balance(t); b.Posted != -100000000+total || b.PendingDebits != 0 {
		t.Fatalf("balance = %+v, want %d posted and nothing pending", b, -100000000+total)
	}
}

// expectSettled checks that o settled with the rail's reference, published it and captured its hold.
func expectSettled(t *testing.T, o payments.Order) {
	t.Helper()
	if s := storedOrder(t, o.ID); s.status != "settled" || !slices.Equal(eventsOf(t, o.ID), events("sent", "settled")) {
		t.Fatalf("order = %+v, events %v; want it settled and published", s, eventsOf(t, o.ID))
	}
	if ref, _ := railRefAndReason(t, o.ID); ref != "SIM"+o.EndToEndID {
		t.Fatalf("rail reference = %q, want the rail's", ref)
	}
	if status := holdStatus(t, o.HoldID); status != "captured" {
		t.Fatalf("hold = %s, want captured", status)
	}
}

// TestAPaymentWithoutChargesPostsOnlyItsCapture: a payment from an account exempt from GMF (the UVT of 2026 is
// 52,374 pesos), of a product without fees, captures its whole hold to the clearing account and posts no
// charges.
func TestAPaymentWithoutChargesPostsOnlyItsCapture(t *testing.T) {
	w := newWorld(t, true)
	events, err := outbox.New(testPool)
	if err != nil {
		t.Fatal(err)
	}
	uvt, err := money.ParsePrecise("52374")
	if err != nil {
		t.Fatal(err)
	}
	mustDo(t, func(tx postgres.Tx) error {
		err := products.New(events).LoadMarketParameter(t.Context(), tx, products.MarketParameter{
			Kind: products.UVT, ValidFrom: calendar.Date{Year: 2026, Month: time.January, Day: 1},
			ValidTo: calendar.Date{Year: 2027, Month: time.January, Day: 1}, Value: uvt,
		})
		if err != nil {
			return err
		}
		_, err = w.deposits.MarkGMFExempt(t.Context(), tx, w.entity, w.account.ID)
		return err
	})
	o := w.sentOrder(t, 30000000)

	if err := w.take(t, w.rail.notification(t)); err != nil {
		t.Fatal(err)
	}

	want := map[string]int64{"customer": 30000000, "L.CLEARING.SIMBREB": -30000000}
	if units, transactions := w.posted(t); !maps.Equal(units, want) || transactions != 1 || !o.GMF.IsZero() || len(o.Fees) != 0 {
		t.Fatalf("posted %v in %d transactions for %+v, want %v in the capture alone", units, transactions, o, want)
	}
}

// TestARejectedPaymentReleasesItsHold (FR-31): an order the rail rejects (an amount ending in 13 pesos) is
// rejected for the rail's reason and its hold released, once: the funds are available again and nothing is
// posted.
func TestARejectedPaymentReleasesItsHold(t *testing.T) {
	w := newWorld(t, true)
	o := w.sentOrder(t, 5001300)
	n := w.rail.notification(t)

	for range 2 {
		if err := w.take(t, n); err != nil {
			t.Fatal(err)
		}
	}

	w.expectRejected(t, o, "AC04", "SIM"+o.EndToEndID)
}

// expectRejected checks that o was rejected for reason, with the rail's reference railRef, and published, and
// that its hold was released without posting anything: the deposit is whole and nothing is pending.
func (w world) expectRejected(t *testing.T, o payments.Order, reason, railRef string) {
	t.Helper()
	if s := storedOrder(t, o.ID); s.status != "rejected" || !slices.Equal(eventsOf(t, o.ID), events("sent", "rejected")) {
		t.Fatalf("order = %+v, events %v; want it rejected and published", s, eventsOf(t, o.ID))
	}
	if ref, why := railRefAndReason(t, o.ID); why != reason || ref != railRef {
		t.Fatalf("rejected with %q by %q, want %q by %q", why, ref, reason, railRef)
	}
	if status := holdStatus(t, o.HoldID); status != "released" {
		t.Fatalf("hold = %s, want released", status)
	}
	if units, _ := w.posted(t); len(units) != 0 {
		t.Fatalf("posted %v, want nothing", units)
	}
	if b := w.balance(t); b.Posted != -100000000 || b.PendingDebits != 0 {
		t.Fatalf("balance = %+v, want the deposit whole and nothing pending", b)
	}
}

// TestANotificationIsTakenOnce (TH-7): a notification taken again changes nothing, and so does the same
// outcome in another; another outcome for an order that took one is ErrOutcomeConflict, and leaves it as it
// was.
func TestANotificationIsTakenOnce(t *testing.T) {
	w := worldOf(t, "valid-tiered-savings.json", true)
	o := w.sentOrder(t, 30000000)
	n := w.rail.notification(t)
	if err := w.take(t, n); err != nil {
		t.Fatal(err)
	}
	before, _ := w.posted(t)

	if err := w.take(t, n); err != nil {
		t.Fatalf("the same notification again = %v, want it taken", err)
	}
	w.rail.outcome = func(payments.Notification) (payments.RailOutcome, error) {
		return payments.RailOutcome{EndToEndID: o.EndToEndID, RailRef: "SIMOTHER", Status: payments.RailAccepted}, nil
	}
	if err := w.take(t, n); err != nil {
		t.Fatalf("the same outcome in another notification = %v, want it taken", err)
	}
	w.rail.outcome = func(payments.Notification) (payments.RailOutcome, error) {
		return payments.RailOutcome{EndToEndID: o.EndToEndID, Status: payments.RailRejected, Reason: "AC04"}, nil
	}
	err := w.take(t, n)

	if !errors.Is(err, payments.ErrOutcomeConflict) {
		t.Fatalf("another outcome = %v, want ErrOutcomeConflict", err)
	}
	after, transactions := w.posted(t)
	if !maps.Equal(after, before) || transactions != 2 || storedOrder(t, o.ID).status != "settled" ||
		!slices.Equal(eventsOf(t, o.ID), events("sent", "settled")) {
		t.Fatalf("posted %v in %d transactions, want %v once and the order settled", after, transactions, before)
	}
	if ref, _ := railRefAndReason(t, o.ID); ref != "SIM"+o.EndToEndID {
		t.Fatalf("rail reference = %q, want the first one", ref)
	}
}

// TestTwoNotificationsAtOnceSettleOnce (TH-7): a notification that arrives while another of the same order is
// being taken waits for it, and then finds the order settled.
func TestTwoNotificationsAtOnceSettleOnce(t *testing.T) {
	w := newWorld(t, true)
	o := w.sentOrder(t, 30000000)
	n := w.rail.notification(t)
	out, err := w.payments.ReadNotification(n)
	if err != nil {
		t.Fatal(err)
	}
	taking, release, first := make(chan struct{}), make(chan struct{}), make(chan error, 1)
	go func() {
		first <- do(t, func(tx postgres.Tx) error {
			err := w.payments.TakeOutcome(t.Context(), tx, out)
			close(taking)
			<-release
			return err
		})
	}()
	<-taking
	second := make(chan error, 1)
	go func() { second <- w.take(t, n) }()
	time.Sleep(200 * time.Millisecond) // the second one reaches the order's lock
	close(release)

	if err1, err2 := <-first, <-second; err1 != nil || err2 != nil {
		t.Fatalf("the notifications = %v and %v, want both taken", err1, err2)
	}
	if units, transactions := w.posted(t); transactions != 2 || units["customer"] != o.Held.Units() {
		t.Fatalf("posted %v in %d transactions, want the capture and the GMF once", units, transactions)
	}
}

// TestANotificationTheCoreDoesNotTake (TH-7): one the rail did not sign is ErrUnverifiedNotification, one
// for an unknown order ErrOrderNotFound, and one whose outcome is neither, or a rejection without its reason,
// or with a reason too long, ErrInvalidNotification; none of them moves the order.
func TestANotificationTheCoreDoesNotTake(t *testing.T) {
	w := newWorld(t, true)
	o := w.sentOrder(t, 30000000)
	n := w.rail.notification(t)
	forged := payments.Notification{Header: n.Header, Body: []byte(strings.Replace(string(n.Body), "accepted", "rejected", 1))}
	outcomes := map[string]struct {
		out  payments.RailOutcome
		want error
	}{
		"an unknown order":         {payments.RailOutcome{EndToEndID: "ALUNAUNKNOWN", Status: payments.RailAccepted}, payments.ErrOrderNotFound},
		"an unknown status":        {payments.RailOutcome{EndToEndID: o.EndToEndID, Status: "lost"}, payments.ErrInvalidNotification},
		"a rejection without why":  {payments.RailOutcome{EndToEndID: o.EndToEndID, Status: payments.RailRejected}, payments.ErrInvalidNotification},
		"a reason too long":        {payments.RailOutcome{EndToEndID: o.EndToEndID, Status: payments.RailRejected, Reason: strings.Repeat("R", 256)}, payments.ErrInvalidNotification},
		"a reference with a NUL":   {payments.RailOutcome{EndToEndID: o.EndToEndID, Status: payments.RailAccepted, RailRef: "SIM\x00"}, payments.ErrInvalidNotification},
		"a reference too long":     {payments.RailOutcome{EndToEndID: o.EndToEndID, Status: payments.RailAccepted, RailRef: strings.Repeat("S", 256)}, payments.ErrInvalidNotification},
		"a reason that is no text": {payments.RailOutcome{EndToEndID: o.EndToEndID, Status: payments.RailRejected, Reason: "\xff"}, payments.ErrInvalidNotification},
	}

	if err := w.take(t, forged); !errors.Is(err, payments.ErrUnverifiedNotification) {
		t.Fatalf("a forged notification = %v, want ErrUnverifiedNotification", err)
	}
	for name, tc := range outcomes {
		w.rail.outcome = func(payments.Notification) (payments.RailOutcome, error) { return tc.out, nil }
		if err := w.take(t, n); !errors.Is(err, tc.want) {
			t.Fatalf("%s = %v, want %v", name, err, tc.want)
		}
	}

	if s := storedOrder(t, o.ID); s.status != "sent" || holdStatus(t, o.HoldID) != "pending" {
		t.Fatalf("order = %+v, want it sent with its hold pending", s)
	}
}

// TestARailThatRefusesRejectsThePayment (FR-31): a message the rail does not take certainly did not leave:
// the order is rejected as RAIL_REFUSED and its hold released.
func TestARailThatRefusesRejectsThePayment(t *testing.T) {
	w := newWorld(t, true)
	w.rail.answer = func() error { return fmt.Errorf("%w: the creditor's bank is not in Bre-B", payments.ErrRailRefused) }
	o, err := w.initiate(t, w.input(t, 30000000, "@beneficiary"))
	if err != nil {
		t.Fatal(err)
	}

	if err := w.send(t, o.ID); err != nil {
		t.Fatalf("send = %v, want the refusal taken", err)
	}

	w.expectRejected(t, o, "RAIL_REFUSED", "")
}

// TestAPaymentWithoutAnswerStaysSent (FR-31): if the rail does not answer, the outcome is unknown: the order
// stays sent and its hold pending, for reconciliation, and Send is ErrNoAnswer.
func TestAPaymentWithoutAnswerStaysSent(t *testing.T) {
	w := newWorld(t, true)
	w.rail.answer = func() error { return context.DeadlineExceeded }
	o, err := w.initiate(t, w.input(t, 30000000, "@beneficiary"))
	if err != nil {
		t.Fatal(err)
	}

	err = w.send(t, o.ID)

	if !errors.Is(err, payments.ErrNoAnswer) || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("send = %v, want ErrNoAnswer", err)
	}
	if s := storedOrder(t, o.ID); s.status != "sent" || holdStatus(t, o.HoldID) != "pending" || w.balance(t).PendingDebits != o.Held.Units() {
		t.Fatalf("order = %+v, want it sent with its hold pending", s)
	}
}

// TestOnlyAHeldOrderIsSent: a job of an order that is not held, because it was already sent or waits for
// compliance, sends nothing; one of an unknown order is ErrOrderNotFound.
func TestOnlyAHeldOrderIsSent(t *testing.T) {
	w := newWorld(t, true)
	sent := w.sentOrder(t, 30000000)
	pending, err := w.initiate(t, w.input(t, 30000000, "match-sanctioned"))
	if err != nil {
		t.Fatal(err)
	}

	for _, id := range []ids.ID{sent.ID, pending.ID} {
		if err := w.send(t, id); err != nil {
			t.Fatalf("send %s = %v, want nothing to do", id, err)
		}
	}
	if err := w.send(t, ids.New()); !errors.Is(err, payments.ErrOrderNotFound) {
		t.Fatalf("send of an unknown order = %v, want ErrOrderNotFound", err)
	}

	if sends := w.rail.sends.Load(); sends != 1 || storedOrder(t, pending.ID).status != "pending_screening" ||
		!slices.Equal(eventsOf(t, sent.ID), events("sent")) {
		t.Fatalf("the rail got %d messages, want only the held order's", sends)
	}
}

// TestTheWorkerSendsFromThePaymentsQueue: the worker takes payments.send from the payments queue, and cancels,
// without retrying, the job of a payment the rail did not answer.
func TestTheWorkerSendsFromThePaymentsQueue(t *testing.T) {
	w := newWorld(t, true)
	w.rail.answer = func() error { return context.DeadlineExceeded }
	workers := river.NewWorkers()
	w.payments.AddWorkers(workers, postgres.NewUnitOfWork(testPool))
	client, err := river.NewClient(riverpgxv5.New(testPool), &river.Config{
		Schema: outbox.RiverSchema, Workers: workers, Queues: map[string]river.QueueConfig{payments.Queue: {MaxWorkers: 1}},
		FetchPollInterval: 100 * time.Millisecond, TestOnly: true, Logger: slog.New(slog.DiscardHandler),
	})
	if err != nil {
		t.Fatal(err)
	}
	finished, cancel := client.Subscribe(river.EventKindJobCompleted, river.EventKindJobCancelled, river.EventKindJobFailed)
	defer cancel()
	o, err := w.initiate(t, w.input(t, 30000000, "@beneficiary"))
	if err != nil {
		t.Fatal(err)
	}
	if err := client.Start(t.Context()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := client.Stop(context.WithoutCancel(t.Context())); err != nil {
			t.Error(err)
		}
	})

	job := jobOf(t, finished, o.ID)

	if job.State != rivertype.JobStateCancelled || job.Queue != payments.Queue || len(job.Errors) != 1 ||
		!strings.Contains(job.Errors[0].Error, "the rail did not answer") {
		t.Fatalf("job = %s in %s with %+v, want it canceled in payments for the rail's silence", job.State, job.Queue, job.Errors)
	}
	if storedOrder(t, o.ID).status != "sent" {
		t.Fatal("the order the worker sent is not sent")
	}
}

// jobOf waits up to 10 seconds for the payments.send job of order id to finish, and returns it.
func jobOf(t *testing.T, finished <-chan *river.Event, id ids.ID) *rivertype.JobRow {
	t.Helper()
	deadline := time.After(10 * time.Second)
	for {
		select {
		case e := <-finished:
			if e.Job.Kind == "payments.send" && strings.Contains(string(e.Job.EncodedArgs), id.String()) {
				return e.Job
			}
		case <-deadline:
			t.Fatal("the worker did not finish the job in 10 s")
			return nil
		}
	}
}
