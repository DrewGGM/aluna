package telemetry

import (
	"context"
	"errors"
	"fmt"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
)

// State is what the operations metrics report of the database (quality-reliability §Observabilidad), read at each
// collection: the transactions posted, the River jobs waiting by queue (every known queue, 0 when it is empty), the
// holds pending, the reconciliation breaks open, the maker-checker requests a checker ever approved, and when the
// oldest audit record still waiting for its seal was recorded (zero if none waits).
type State struct {
	Postings         int64
	QueueDepth       map[string]int64
	PendingHolds     int64
	OpenBreaks       int64
	ApprovedRequests int64
	OldestUnsealed   time.Time
}

// Source reads the State.
type Source interface {
	Read(ctx context.Context) (State, error)
}

// PoolState is how full the process's pool of database connections is.
type PoolState struct {
	Acquired, Idle, Max int64
}

// Register registers in meter the operations metrics (FF-6): those of the database, read from source at each
// collection, with the age of the audit record waiting for its seal counted at now; and the saturation of the
// process's pool, read from pool. A source that fails is reported to OTel's error handler and leaves only its own
// metrics out of that collection: the rest, the pool's and those of the requests among them, are exported anyway,
// when they matter most. The golden signals of each endpoint come from http.server.request.duration. The returned
// function unregisters them; it is called before the pool closes.
func Register(meter metric.Meter, source Source, pool func() PoolState, now func() time.Time) (func() error, error) {
	db, err := databaseInstruments(meter)
	if err != nil {
		return nil, err
	}
	poolConnections, err := meter.Int64ObservableGauge("aluna.db.pool.connections", metric.WithUnit("{connection}"),
		metric.WithDescription("Database connections of the process's pool, by state: acquired or idle."))
	if err != nil {
		return nil, fmt.Errorf("pool metric: %w", err)
	}
	poolMax, err := meter.Int64ObservableGauge("aluna.db.pool.max", metric.WithUnit("{connection}"),
		metric.WithDescription("Most connections the process's pool opens."))
	if err != nil {
		return nil, fmt.Errorf("pool metric: %w", err)
	}
	dbRegistration, err := meter.RegisterCallback(func(ctx context.Context, o metric.Observer) error {
		s, err := source.Read(ctx)
		if err != nil {
			otel.Handle(fmt.Errorf("read the operations metrics: %w", err))
			return nil
		}
		db.observe(o, s, now())
		return nil
	}, db.all()...)
	if err != nil {
		return nil, fmt.Errorf("register the operations metrics: %w", err)
	}
	poolRegistration, err := meter.RegisterCallback(func(_ context.Context, o metric.Observer) error {
		p := pool()
		o.ObserveInt64(poolConnections, p.Acquired, metric.WithAttributes(attribute.String("state", "acquired")))
		o.ObserveInt64(poolConnections, p.Idle, metric.WithAttributes(attribute.String("state", "idle")))
		o.ObserveInt64(poolMax, p.Max)
		return nil
	}, poolConnections, poolMax)
	if err != nil {
		return nil, errors.Join(fmt.Errorf("register the pool metrics: %w", err), dbRegistration.Unregister())
	}
	return func() error { return errors.Join(dbRegistration.Unregister(), poolRegistration.Unregister()) }, nil
}

// instruments are the operations metrics of the database.
type instruments struct {
	postings, approved                   metric.Int64ObservableCounter
	queueDepth, pendingHolds, openBreaks metric.Int64ObservableGauge
	unsealedAge                          metric.Float64ObservableGauge
}

func databaseInstruments(meter metric.Meter) (instruments, error) {
	var in instruments
	var errs []error
	add := func(err error) { errs = append(errs, err) }
	var err error
	in.postings, err = meter.Int64ObservableCounter("aluna.ledger.postings", metric.WithUnit("{transaction}"),
		metric.WithDescription("Rows inserted into ledger.transactions, rolled-back attempts included, as the table's statistics count them; resets with them."))
	add(err)
	in.approved, err = meter.Int64ObservableCounter("aluna.approvals.approved", metric.WithUnit("{request}"),
		metric.WithDescription("Maker-checker requests a checker approved, ever, whether their command then ran or failed."))
	add(err)
	in.queueDepth, err = meter.Int64ObservableGauge("aluna.jobs.queue.depth", metric.WithUnit("{job}"),
		metric.WithDescription("River jobs waiting to run, by queue."))
	add(err)
	in.pendingHolds, err = meter.Int64ObservableGauge("aluna.ledger.holds.pending", metric.WithUnit("{hold}"),
		metric.WithDescription("Holds pending capture or release."))
	add(err)
	in.openBreaks, err = meter.Int64ObservableGauge("aluna.recon.breaks.open", metric.WithUnit("{break}"),
		metric.WithDescription("Reconciliation breaks open."))
	add(err)
	in.unsealedAge, err = meter.Float64ObservableGauge("aluna.audit.unsealed.age", metric.WithUnit("s"),
		metric.WithDescription("How long the oldest audit record has waited for its seal; 0 when none waits."))
	add(err)
	if err := errors.Join(errs...); err != nil {
		return instruments{}, fmt.Errorf("operations metrics: %w", err)
	}
	return in, nil
}

func (in instruments) all() []metric.Observable {
	return []metric.Observable{in.postings, in.approved, in.queueDepth, in.pendingHolds, in.openBreaks, in.unsealedAge}
}

func (in instruments) observe(o metric.Observer, s State, now time.Time) {
	o.ObserveInt64(in.postings, s.Postings)
	o.ObserveInt64(in.approved, s.ApprovedRequests)
	for queue, depth := range s.QueueDepth {
		o.ObserveInt64(in.queueDepth, depth, metric.WithAttributes(attribute.String("queue", queue)))
	}
	o.ObserveInt64(in.pendingHolds, s.PendingHolds)
	o.ObserveInt64(in.openBreaks, s.OpenBreaks)
	var age float64
	if !s.OldestUnsealed.IsZero() {
		age = now.Sub(s.OldestUnsealed).Seconds()
	}
	o.ObserveFloat64(in.unsealedAge, age)
}
