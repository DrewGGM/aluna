// Package jobs wires the worker's jobs (FR-47, ADR-0007): it registers them in River and defines the
// periodic ones. Every job is idempotent: running it too often, or two at once, leaves the same result
// (two at once can collide on a lock and one fails; the next interval repeats it).
package jobs

import (
	"context"
	"time"

	"github.com/riverqueue/river"

	"github.com/DrewGGM/aluna/internal/audit"
	"github.com/DrewGGM/aluna/internal/deposits"
	"github.com/DrewGGM/aluna/internal/idempotency"
	"github.com/DrewGGM/aluna/internal/ledger"
	"github.com/DrewGGM/aluna/internal/outbox"
	"github.com/DrewGGM/aluna/internal/postgres"
)

// QueueLedger is the queue for the jobs that write to the ledger and QueueInterest the one of the accrual,
// which runs one at a time: a catch-up can outlast its interval, and the runs it enqueues meanwhile wait and
// then find nothing to do, instead of sharing the work, colliding on its rows and taking the default queue's
// workers from the other jobs. The rest go to river.QueueDefault.
const (
	QueueLedger   = "ledger"
	QueueInterest = "interest"
)

// The two ledger jobs keep taking batches until they catch up; the size bounds each transaction.
const (
	holdsBatch     = 500
	aggregateBatch = 1000
)

// The accrual (FR-25) locks accounts batch by batch, so a batch also bounds how long a movement on one of
// them waits. A run catches up every closed day, which after a long stop can take longer than River's
// default limit of a minute; its batches commit as they go, so a run cut short only leaves work for the next.
const (
	accrualBatch   = 500
	accrualTimeout = 30 * time.Minute
)

// Sealing (ADR-0012) closes blocks of at most auditBlockSize records, sealRounds per run so it finishes
// before the job's time limit; whatever is left goes to the next run.
const (
	auditBlockSize = 10_000
	sealRounds     = 5
)

const (
	idempotencyRetention = 7 * 24 * time.Hour  // architecture §9
	outboxRetention      = 30 * 24 * time.Hour // the Events API retains 30 days (FR-46)
)

// Deps are the services the jobs use.
type Deps struct {
	UnitOfWork  *postgres.UnitOfWork
	Ledger      *ledger.Service
	Idempotency *idempotency.Store
	Outbox      *outbox.Outbox
	Audit       *audit.Service
	Deposits    *deposits.Service
}

// Config is the worker's River configuration: the queues, the workers from Register, the periodic jobs
// and the schema. The caller adds the logger and how it shuts down.
func Config(deps Deps) *river.Config {
	workers := river.NewWorkers()
	Register(workers, deps)
	return &river.Config{
		Schema: outbox.RiverSchema, Workers: workers, PeriodicJobs: Periodic(),
		Queues: map[string]river.QueueConfig{
			river.QueueDefault: {MaxWorkers: 4}, QueueLedger: {MaxWorkers: 2}, QueueInterest: {MaxWorkers: 1},
		},
		// REINDEX requires owning the index and the worker runs as aluna_app (migration 0009).
		ReindexerIndexNames: []string{},
	}
}

// Register registers in w the workers of the periodic jobs and the one that publishes the outbox.
func Register(w *river.Workers, deps Deps) {
	river.AddWorker(w, &task[expireHolds]{run: func(ctx context.Context) error {
		_, err := deps.Ledger.ExpireDueHolds(ctx, deps.UnitOfWork, time.Now(), holdsBatch)
		return err
	}})
	river.AddWorker(w, &task[aggregateAsync]{run: func(ctx context.Context) error {
		_, err := deps.Ledger.AggregateAsyncBalances(ctx, deps.UnitOfWork, aggregateBatch)
		return err
	}})
	river.AddWorker(w, &task[purgeIdempotency]{run: func(ctx context.Context) error {
		_, err := deps.Idempotency.Purge(ctx, time.Now().Add(-idempotencyRetention))
		return err
	}})
	river.AddWorker(w, &task[purgeOutbox]{run: func(ctx context.Context) error {
		_, err := deps.Outbox.Purge(ctx, time.Now().Add(-outboxRetention))
		return err
	}})
	river.AddWorker(w, &task[sealAudit]{run: func(ctx context.Context) error {
		for range sealRounds {
			id, err := deps.Audit.Seal(ctx, deps.UnitOfWork, auditBlockSize)
			if err != nil || id == 0 {
				return err
			}
		}
		return nil
	}})
	river.AddWorker(w, &task[accrueInterest]{timeout: accrualTimeout, run: func(ctx context.Context) error {
		_, err := deps.Deposits.Accrue(ctx, deps.UnitOfWork, accrualBatch)
		return err
	}})
	deps.Outbox.AddWorkers(w)
}

// Periodic are the periodic jobs. They also run at startup (RunOnStart): River's scheduler lives in the
// leader's memory, and without it a worker that restarts more often than the interval would never run
// them.
func Periodic() []*river.PeriodicJob {
	return []*river.PeriodicJob{
		every(30*time.Second, expireHolds{}),
		every(10*time.Second, aggregateAsync{}),
		every(time.Hour, purgeIdempotency{}),
		every(24*time.Hour, purgeOutbox{}),
		every(time.Minute, sealAudit{}),
		every(5*time.Minute, accrueInterest{}),
	}
}

func every(interval time.Duration, args river.JobArgs) *river.PeriodicJob {
	return river.NewPeriodicJob(river.PeriodicInterval(interval),
		func() (river.JobArgs, *river.InsertOpts) { return args, nil },
		&river.PeriodicJobOpts{RunOnStart: true})
}

// task is the worker of a periodic job: it has no arguments, only something to run, within timeout (River's
// default if zero).
type task[A river.JobArgs] struct {
	river.WorkerDefaults[A]
	run     func(ctx context.Context) error
	timeout time.Duration
}

func (t *task[A]) Work(ctx context.Context, _ *river.Job[A]) error { return t.run(ctx) }

func (t *task[A]) Timeout(*river.Job[A]) time.Duration { return t.timeout }

// insertOpts puts the job in queue without retries: a failing periodic job runs again on the next
// interval, instead of piling up retries of the same run.
func insertOpts(queue string) river.InsertOpts { return river.InsertOpts{Queue: queue, MaxAttempts: 1} }

type expireHolds struct{}

func (expireHolds) Kind() string                 { return "ledger.holds.expire" }
func (expireHolds) InsertOpts() river.InsertOpts { return insertOpts(QueueLedger) }

type aggregateAsync struct{}

func (aggregateAsync) Kind() string                 { return "ledger.balances.aggregate" }
func (aggregateAsync) InsertOpts() river.InsertOpts { return insertOpts(QueueLedger) }

type purgeIdempotency struct{}

func (purgeIdempotency) Kind() string                 { return "idempotency.purge" }
func (purgeIdempotency) InsertOpts() river.InsertOpts { return insertOpts(river.QueueDefault) }

type purgeOutbox struct{}

func (purgeOutbox) Kind() string                 { return "outbox.purge" }
func (purgeOutbox) InsertOpts() river.InsertOpts { return insertOpts(river.QueueDefault) }

type sealAudit struct{}

func (sealAudit) Kind() string                 { return "audit.seal" }
func (sealAudit) InsertOpts() river.InsertOpts { return insertOpts(river.QueueDefault) }

// accrueInterest accrues the closed days, once each: running it often only shortens the wait after a rollover.
type accrueInterest struct{}

func (accrueInterest) Kind() string                 { return "deposits.interest.accrue" }
func (accrueInterest) InsertOpts() river.InsertOpts { return insertOpts(QueueInterest) }
