package app

import (
	"context"
	"sync"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
)

// Outcome is what screening decides about an operation (FR-42): OutcomeClear lets it go on, OutcomePending
// holds it, without moving money, until compliance resolves it.
type Outcome string

// The only two outcomes: whatever is not OutcomeClear holds the operation.
const (
	OutcomeClear   Outcome = "clear"
	OutcomePending Outcome = "pending"
)

const (
	instrumentation = "github.com/DrewGGM/aluna/internal/compliance"

	screenTimeout  = 2 * time.Second  // the longest an operation waits for the vendor
	failuresToOpen = 5                // failures in a row that open the circuit
	openFor        = 30 * time.Second // how long the circuit stays open before a probe
)

// circuit is the state of the Screener's circuit breaker.
type circuit string

// Circuit states: closed calls the vendor, open does not, and half_open lets one call probe it.
const (
	circuitClosed   circuit = "closed"
	circuitOpen     circuit = "open"
	circuitHalfOpen circuit = "half_open"
)

// Screener asks the vendor about a subject and fails closed (AD-3): an error, no answer within screenTimeout
// or the circuit open is OutcomePending, never OutcomeClear. Its own circuit breaker stops calling a vendor
// that failed failuresToOpen times in a row for openFor, and then lets one call probe it: if the vendor
// answers the circuit closes, and if not it opens again. Each change counts in
// aluna.compliance.screening.circuit.transitions by the state it moves to.
type Screener struct {
	port        ScreeningPort
	now         func() time.Time
	transitions metric.Int64Counter

	mu       sync.Mutex
	state    circuit
	period   uint64    // how many times the state changed: a call's answer only counts in the period that admitted it
	failures int       // in a row, while closed
	openedAt time.Time // while open
	probing  bool      // while half open
}

// admission is the state and period of the circuit that let a call through.
type admission struct {
	state  circuit
	period uint64
}

// NewScreener is a Screener of port, closed, on the clock now.
func NewScreener(port ScreeningPort, now func() time.Time) *Screener {
	transitions, err := otel.Meter(instrumentation).Int64Counter("aluna.compliance.screening.circuit.transitions",
		metric.WithDescription("Changes of state of the screening circuit breaker, by the state it moves to."))
	if err != nil {
		otel.Handle(err)
	}
	return &Screener{port: port, now: now, transitions: transitions, state: circuitClosed}
}

// Screen is OutcomeClear if the vendor clears subject, and OutcomePending otherwise (FR-42). A caller whose ctx
// ends is OutcomePending too, but that is not the vendor failing: it does not count towards opening the
// circuit, and a caller that has already given up does not call the vendor.
func (s *Screener) Screen(ctx context.Context, subject Subject) Outcome {
	if ctx.Err() != nil {
		return OutcomePending
	}
	a, ok := s.admit(ctx)
	if !ok {
		return OutcomePending
	}
	r, err := s.call(ctx, subject)
	if err != nil && ctx.Err() != nil {
		s.release(a)
		return OutcomePending
	}
	s.record(ctx, a, err == nil)
	if err != nil || r.Status != StatusClear {
		return OutcomePending
	}
	return OutcomeClear
}

// call asks the vendor, for up to screenTimeout even if the vendor ignores its context.
func (s *Screener) call(ctx context.Context, subject Subject) (Result, error) {
	ctx, cancel := context.WithTimeout(ctx, screenTimeout)
	defer cancel()
	type answer struct {
		result Result
		err    error
	}
	answered := make(chan answer, 1) // a vendor that answers late does not block
	go func() {
		r, err := s.port.Screen(ctx, subject)
		answered <- answer{r, err}
	}()
	select {
	case a := <-answered:
		return a.result, a.err
	case <-ctx.Done():
		return Result{}, ctx.Err()
	}
}

// admit says whether a call may go to the vendor, and how: always while closed, never while open until
// openFor passes, and then one at a time as the probe.
func (s *Screener) admit(ctx context.Context) (admission, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.state == circuitOpen && s.now().Sub(s.openedAt) >= openFor {
		s.move(ctx, circuitHalfOpen)
	}
	a := admission{state: s.state, period: s.period}
	switch {
	case s.state == circuitClosed:
		return a, true
	case s.state == circuitHalfOpen && !s.probing:
		s.probing = true
		return a, true
	}
	return a, false
}

// record counts whether the vendor answered a call admitted as a, unless the circuit has changed since: a
// probe closes the circuit if it answered and opens it again if not, and a call while closed opens it at the
// failuresToOpen-th failure in a row.
func (s *Screener) record(ctx context.Context, a admission, answered bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if a.period != s.period {
		return
	}
	switch {
	case a.state == circuitHalfOpen && answered:
		s.probing, s.failures = false, 0
		s.move(ctx, circuitClosed)
	case a.state == circuitHalfOpen:
		s.probing, s.openedAt = false, s.now()
		s.move(ctx, circuitOpen)
	case answered:
		s.failures = 0
	default:
		if s.failures++; s.failures >= failuresToOpen {
			s.openedAt = s.now()
			s.move(ctx, circuitOpen)
		}
	}
}

// release frees the probe, if a call admitted as a took it: its caller gave up without an answer. Nothing
// else changes the circuit while a probe is in flight.
func (s *Screener) release(a admission) {
	if a.state != circuitHalfOpen {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.probing = false
}

// move sets the circuit's state, starts a new period and counts the change; the caller holds mu.
func (s *Screener) move(ctx context.Context, to circuit) {
	s.state = to
	s.period++
	s.transitions.Add(ctx, 1, metric.WithAttributes(attribute.String("state", string(to))))
}
