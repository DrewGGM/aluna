package app_test

import (
	"context"
	"errors"
	"maps"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"go.opentelemetry.io/otel"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"

	"github.com/DrewGGM/aluna/internal/compliance/internal/adapters/fakescreening"
	"github.com/DrewGGM/aluna/internal/compliance/internal/app"
	"github.com/DrewGGM/aluna/internal/ids"
)

// Prefixes of the fake vendor's subjects.
const (
	onNoList = ""
	match    = fakescreening.MatchPrefix
	failing  = fakescreening.ErrorPrefix
	slow     = fakescreening.SlowPrefix
)

// clock is a time the test moves by hand.
type clock struct {
	mu  sync.Mutex
	now time.Time
}

func newClock() *clock { return &clock{now: time.Date(2026, time.March, 2, 9, 0, 0, 0, time.UTC)} }

func (c *clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *clock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

// counting is the fake vendor, counting its calls.
type counting struct{ calls atomic.Int32 }

func (c *counting) Screen(ctx context.Context, s app.Subject) (app.Result, error) {
	c.calls.Add(1)
	return fakescreening.Vendor{}.Screen(ctx, s)
}

// portFunc is a vendor made of a function.
type portFunc func(context.Context, app.Subject) (app.Result, error)

func (f portFunc) Screen(ctx context.Context, s app.Subject) (app.Result, error) { return f(ctx, s) }

// subject is a party whose external reference starts with prefix.
func subject(prefix string) app.Subject { return named(prefix + ids.New().String()) }

// named is a party whose external reference is ref.
func named(ref string) app.Subject {
	return app.Subject{Kind: app.SubjectParty, ID: ids.New(), ExternalRef: ref}
}

// screener is a Screener of the fake vendor, counting its calls, on a clock of the test.
func screener(t *testing.T) (*app.Screener, *counting, *clock) {
	t.Helper()
	vendor, c := &counting{}, newClock()
	return app.NewScreener(vendor, c.Now), vendor, c
}

// openCircuit fails five calls in a row.
func openCircuit(t *testing.T, s *app.Screener) {
	t.Helper()
	for range 5 {
		outcomeOf(t.Context(), s, subject(failing))
	}
}

// outcomeOf is what s decides about subject, without the vendor's reference.
func outcomeOf(ctx context.Context, s *app.Screener, subject app.Subject) app.Outcome {
	o, _ := s.Screen(ctx, subject)
	return o
}

// within waits for done, or fails the test after 5 seconds instead of hanging it.
func within(t *testing.T, done <-chan struct{}) {
	t.Helper()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("still waiting after 5 s")
	}
}

// outcomeWithin waits for an outcome on outcomes, or fails the test after 5 seconds instead of hanging it.
func outcomeWithin(t *testing.T, outcomes <-chan app.Outcome) app.Outcome {
	t.Helper()
	select {
	case o := <-outcomes:
		return o
	case <-time.After(5 * time.Second):
		t.Fatal("no outcome after 5 s")
		return ""
	}
}

// heldVendor clears "clear", holds "held" until its context ends (or, if deaf, until the test ends) after
// closing entered, and fails anything else.
func heldVendor(t *testing.T, deaf bool) (vendor portFunc, entered <-chan struct{}) {
	t.Helper()
	in, testEnded := make(chan struct{}), make(chan struct{})
	t.Cleanup(func() { close(testEnded) })
	var once sync.Once
	return func(ctx context.Context, s app.Subject) (app.Result, error) {
		switch s.ExternalRef {
		case "clear":
			return app.Result{Status: app.StatusClear}, nil
		case "held":
			once.Do(func() { close(in) })
			if deaf {
				<-testEnded
				return app.Result{Status: app.StatusClear}, nil
			}
			<-ctx.Done()
			return app.Result{}, ctx.Err()
		}
		return app.Result{}, errors.New("down")
	}, in
}

// TestTheVendorDecides (FR-42): a subject the vendor clears is OutcomeClear; one it may match, an error, an
// answer the Screener does not know and a clear answer with an error are OutcomePending.
func TestTheVendorDecides(t *testing.T) {
	s, _, _ := screener(t)
	answering := func(r app.Result, err error) *app.Screener {
		return app.NewScreener(portFunc(func(context.Context, app.Subject) (app.Result, error) { return r, err }), time.Now)
	}

	cases := map[string]struct {
		screener *app.Screener
		subject  app.Subject
		want     app.Outcome
	}{
		"clear":             {s, subject(onNoList), app.OutcomeClear},
		"a possible match":  {s, subject(match), app.OutcomePending},
		"an error":          {s, subject(failing), app.OutcomePending},
		"an unknown answer": {answering(app.Result{Status: "maybe"}, nil), subject(onNoList), app.OutcomePending},
		"clear, with an error": {
			answering(app.Result{Status: app.StatusClear}, errors.New("partial")), subject(onNoList), app.OutcomePending,
		},
	}
	for name, tc := range cases {
		if got := outcomeOf(t.Context(), tc.screener, tc.subject); got != tc.want {
			t.Errorf("%s = %s, want %s", name, got, tc.want)
		}
	}
}

// TestTheVendorsReferenceComesBack (FR-42): the vendor's reference to its check comes back with what it
// answered, a clear or a possible match, and there is none without an answer.
func TestTheVendorsReferenceComesBack(t *testing.T) {
	s, _, _ := screener(t)
	cleared, matched, failed := subject(onNoList), subject(match), subject(failing)

	_, clearRef := s.Screen(t.Context(), cleared)
	_, matchRef := s.Screen(t.Context(), matched)
	_, failedRef := s.Screen(t.Context(), failed)

	if clearRef != "fake-"+cleared.ID.String() || matchRef != "fake-"+matched.ID.String() || failedRef != "" {
		t.Fatalf("references = %q, %q and %q, want the vendor's for a clear and a match, and none for a failure",
			clearRef, matchRef, failedRef)
	}
}

// TestAVendorThatDoesNotAnswerIsPending (AD-3, TH-8): a vendor that does not answer in 2 seconds is
// OutcomePending, also one that ignores its context, and the Screener does not wait for it much longer.
func TestAVendorThatDoesNotAnswerIsPending(t *testing.T) {
	deaf, _ := heldVendor(t, true)
	cases := map[string]struct {
		screener *app.Screener
		subject  app.Subject
	}{
		"slow":                {app.NewScreener(fakescreening.Vendor{}, time.Now), subject(slow)},
		"deaf to its context": {app.NewScreener(deaf, time.Now), named("held")},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			start := time.Now()
			got := outcomeOf(t.Context(), tc.screener, tc.subject)
			if elapsed := time.Since(start); got != app.OutcomePending || elapsed < 2*time.Second || elapsed > 2500*time.Millisecond {
				t.Fatalf("= %s after %s, want pending after about 2 s", got, elapsed)
			}
		})
	}
}

// TestACallerThatGaveUpDoesNotCallTheVendor (AD-3): a caller whose context already ended is OutcomePending
// without calling the vendor, not even after the Screener answers.
func TestACallerThatGaveUpDoesNotCallTheVendor(t *testing.T) {
	called := make(chan struct{}, 1)
	s := app.NewScreener(portFunc(func(context.Context, app.Subject) (app.Result, error) {
		called <- struct{}{}
		return app.Result{Status: app.StatusClear}, nil
	}), time.Now)
	gone, cancel := context.WithCancel(t.Context())
	cancel()

	got := outcomeOf(gone, s, subject(onNoList))

	select {
	case <-called:
		t.Fatal("the vendor was called for a caller that gave up")
	case <-time.After(100 * time.Millisecond):
	}
	if got != app.OutcomePending {
		t.Fatalf("a caller that gave up = %s, want pending", got)
	}
}

// screenAndGiveUp screens a subject "held" and gives up once the vendor has it; it is OutcomePending.
func screenAndGiveUp(t *testing.T, s *app.Screener, entered <-chan struct{}) {
	t.Helper()
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan app.Outcome, 1)
	go func() { done <- outcomeOf(ctx, s, named("held")) }()
	within(t, entered)
	cancel()
	if got := outcomeWithin(t, done); got != app.OutcomePending {
		t.Fatalf("a caller that gave up during the call = %s, want pending", got)
	}
}

// TestACallerThatGivesUpIsNotAFailure (AD-3): a caller that gives up while the vendor has its call is
// OutcomePending, but that is not the vendor failing: after 4 failures it does not open the circuit.
func TestACallerThatGivesUpIsNotAFailure(t *testing.T) {
	vendor, entered := heldVendor(t, false)
	s := app.NewScreener(vendor, newClock().Now)
	for range 4 {
		outcomeOf(t.Context(), s, subject(failing))
	}

	screenAndGiveUp(t, s, entered)

	if got := outcomeOf(t.Context(), s, named("clear")); got != app.OutcomeClear {
		t.Fatalf("after 4 failures and a caller that gave up, a clear subject = %s, want clear", got)
	}
}

// TestAProbeWhoseCallerGivesUpLetsAnotherProbe (AD-3): a probe whose caller gives up is OutcomePending, and
// the next call probes the vendor instead.
func TestAProbeWhoseCallerGivesUpLetsAnotherProbe(t *testing.T) {
	vendor, entered := heldVendor(t, false)
	c := newClock()
	s := app.NewScreener(vendor, c.Now)
	openCircuit(t, s)
	c.advance(30 * time.Second)

	screenAndGiveUp(t, s, entered)

	if got := outcomeOf(t.Context(), s, named("clear")); got != app.OutcomeClear {
		t.Fatalf("after an abandoned probe, the next call = %s, want clear", got)
	}
}

// TestFiveFailuresOpenTheCircuit (AD-3): after five failures in a row the vendor is not called for 30
// seconds, and every subject is OutcomePending; a success in between starts the count again.
func TestFiveFailuresOpenTheCircuit(t *testing.T) {
	s, vendor, c := screener(t)
	for range 4 {
		outcomeOf(t.Context(), s, subject(failing))
	}
	outcomeOf(t.Context(), s, subject(onNoList)) // the count starts again
	for range 4 {
		outcomeOf(t.Context(), s, subject(failing))
	}
	if got := outcomeOf(t.Context(), s, subject(onNoList)); got != app.OutcomeClear {
		t.Fatalf("after 4 failures, a success and 4 more failures, a clear subject = %s, want clear", got)
	}
	openCircuit(t, s)
	calls := vendor.calls.Load()

	c.advance(30*time.Second - time.Nanosecond)
	open := outcomeOf(t.Context(), s, subject(onNoList))

	if open != app.OutcomePending || vendor.calls.Load() != calls {
		t.Fatalf("with the circuit open, a clear subject = %s after %d calls of the vendor, want pending without calling it",
			open, vendor.calls.Load()-calls)
	}
}

// TestAfter30SecondsOneCallProbesTheVendor (AD-3): 30 seconds after the circuit opens, one call probes the
// vendor; if the probe fails the circuit opens again for 30 seconds, and if it succeeds it closes, with the
// count of failures started again.
func TestAfter30SecondsOneCallProbesTheVendor(t *testing.T) {
	s, vendor, c := screener(t)
	openCircuit(t, s)
	c.advance(30 * time.Second)

	if got := outcomeOf(t.Context(), s, subject(failing)); got != app.OutcomePending {
		t.Fatalf("a failing probe = %s, want pending", got)
	}
	calls := vendor.calls.Load()
	c.advance(30*time.Second - time.Nanosecond)
	if got := outcomeOf(t.Context(), s, subject(onNoList)); got != app.OutcomePending || vendor.calls.Load() != calls {
		t.Fatalf("29.9 s after a failing probe, a clear subject = %s with %d calls, want pending without calling", got,
			vendor.calls.Load()-calls)
	}
	c.advance(time.Nanosecond)
	if got := outcomeOf(t.Context(), s, subject(onNoList)); got != app.OutcomeClear {
		t.Fatalf("a probe that succeeds = %s, want clear", got)
	}
	outcomeOf(t.Context(), s, subject(failing))
	if got := outcomeOf(t.Context(), s, subject(onNoList)); got != app.OutcomeClear || vendor.calls.Load() != calls+3 {
		t.Fatalf("after a probe that succeeds and one failure, a clear subject = %s with %d calls, want clear with 3",
			got, vendor.calls.Load()-calls)
	}
}

// TestOnlyOneProbeAtATime (AD-3): while the probe is in flight, other calls are OutcomePending without calling
// the vendor.
func TestOnlyOneProbeAtATime(t *testing.T) {
	held, entered := heldVendor(t, false)
	var calls atomic.Int32
	c := newClock()
	s := app.NewScreener(portFunc(func(ctx context.Context, sub app.Subject) (app.Result, error) {
		calls.Add(1)
		return held(ctx, sub)
	}), c.Now)
	openCircuit(t, s)
	c.advance(30 * time.Second)
	probing, endProbe := context.WithCancel(t.Context())
	probed := make(chan app.Outcome, 1)
	go func() { probed <- outcomeOf(probing, s, named("held")) }()
	within(t, entered)

	other := outcomeOf(t.Context(), s, named("clear"))
	endProbe()
	outcomeWithin(t, probed)

	if other != app.OutcomePending || calls.Load() != 6 {
		t.Fatalf("a call during the probe = %s with %d calls, want pending with 6", other, calls.Load())
	}
}

// TestACallFromBeforeTheProbeDoesNotFreeIt (AD-3): a call let through while the circuit was closed whose
// caller gives up during the probe does not free the probe: the next call is still OutcomePending.
func TestACallFromBeforeTheProbeDoesNotFreeIt(t *testing.T) {
	entered := map[string]chan struct{}{"before": make(chan struct{}), "probe": make(chan struct{})}
	c := newClock()
	s := app.NewScreener(portFunc(func(ctx context.Context, sub app.Subject) (app.Result, error) {
		if in, held := entered[sub.ExternalRef]; held {
			close(in)
			<-ctx.Done()
			return app.Result{}, ctx.Err()
		}
		if sub.ExternalRef == "clear" {
			return app.Result{Status: app.StatusClear}, nil
		}
		return app.Result{}, errors.New("down")
	}), c.Now)
	before, giveUp := context.WithCancel(t.Context())
	probing, endProbe := context.WithCancel(t.Context())
	gaveUp, probed := make(chan struct{}), make(chan app.Outcome, 1)
	go func() { outcomeOf(before, s, named("before")); close(gaveUp) }()
	within(t, entered["before"])
	openCircuit(t, s)
	c.advance(30 * time.Second)
	go func() { probed <- outcomeOf(probing, s, named("probe")) }()
	within(t, entered["probe"])
	giveUp()
	within(t, gaveUp)

	got := outcomeOf(t.Context(), s, named("clear"))
	endProbe()
	outcomeWithin(t, probed)

	if got != app.OutcomePending {
		t.Fatalf("a call while the probe is in flight = %s, want pending", got)
	}
}

// TestLateFailuresDoNotReopenTheCircuit (AD-3): calls let through while the circuit was closed that fail
// after it opened change nothing: the probe still comes 30 seconds after it opened.
func TestLateFailuresDoNotReopenTheCircuit(t *testing.T) {
	var held sync.WaitGroup
	c := newClock()
	s := app.NewScreener(portFunc(func(ctx context.Context, sub app.Subject) (app.Result, error) {
		switch sub.ExternalRef {
		case "held":
			held.Done()
			<-ctx.Done() // the Screener's timeout fails it
			return app.Result{}, ctx.Err()
		case "clear":
			return app.Result{Status: app.StatusClear}, nil
		}
		return app.Result{}, errors.New("down")
	}), c.Now)
	late := make(chan app.Outcome, 5)
	held.Add(5)
	for range 5 {
		go func() { late <- outcomeOf(t.Context(), s, named("held")) }()
	}
	allHeld := make(chan struct{})
	go func() { held.Wait(); close(allHeld) }()
	within(t, allHeld)
	openCircuit(t, s)
	c.advance(20 * time.Second)
	for range 5 {
		outcomeWithin(t, late) // wait for their timeouts, with the circuit open
	}
	c.advance(10 * time.Second)

	if got := outcomeOf(t.Context(), s, named("clear")); got != app.OutcomeClear {
		t.Fatalf("30 s after the circuit opened, with late failures in between, the probe = %s, want clear", got)
	}
}

// TestEveryChangeOfTheCircuitIsMeasured: the circuit's changes count in
// aluna.compliance.screening.circuit.transitions by the state it moves to.
func TestEveryChangeOfTheCircuitIsMeasured(t *testing.T) {
	reader := sdkmetric.NewManualReader()
	previous := otel.GetMeterProvider()
	otel.SetMeterProvider(sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader)))
	t.Cleanup(func() { otel.SetMeterProvider(previous) })
	s, _, c := screener(t)

	openCircuit(t, s) // closed → open
	c.advance(30 * time.Second)
	outcomeOf(t.Context(), s, subject(failing)) // → half_open → open
	c.advance(30 * time.Second)
	outcomeOf(t.Context(), s, subject(onNoList)) // → half_open → closed

	if got, want := transitions(t, reader), map[string]int64{"open": 2, "half_open": 2, "closed": 1}; !maps.Equal(got, want) {
		t.Fatalf("transitions = %v, want %v", got, want)
	}
}

// transitions are the points of aluna.compliance.screening.circuit.transitions by state.
func transitions(t *testing.T, reader *sdkmetric.ManualReader) map[string]int64 {
	t.Helper()
	var rm metricdata.ResourceMetrics
	if err := reader.Collect(t.Context(), &rm); err != nil {
		t.Fatal(err)
	}
	got := make(map[string]int64)
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			sum, ok := m.Data.(metricdata.Sum[int64])
			if !ok || m.Name != "aluna.compliance.screening.circuit.transitions" {
				continue
			}
			for _, p := range sum.DataPoints {
				state, _ := p.Attributes.Value("state")
				got[state.AsString()] += p.Value
			}
		}
	}
	return got
}
