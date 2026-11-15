// Package jobs wires the worker's jobs (FR-47, ADR-0007): it registers them in River and defines the
// periodic ones. Every job is idempotent: running it too often, or two at once, leaves the same result
// (two at once can collide on a lock and one fails; the next interval repeats it).
package jobs

import (
	"context"
	"errors"
	"time"

	"github.com/riverqueue/river"
	"github.com/riverqueue/river/rivertype"

	"github.com/DrewGGM/aluna/internal/accounting"
	"github.com/DrewGGM/aluna/internal/audit"
	"github.com/DrewGGM/aluna/internal/compliance"
	"github.com/DrewGGM/aluna/internal/deposits"
	"github.com/DrewGGM/aluna/internal/idempotency"
	"github.com/DrewGGM/aluna/internal/ids"
	"github.com/DrewGGM/aluna/internal/ledger"
	"github.com/DrewGGM/aluna/internal/outbox"
	"github.com/DrewGGM/aluna/internal/payments"
	"github.com/DrewGGM/aluna/internal/postgres"
	"github.com/DrewGGM/aluna/internal/reconciliation"
	"github.com/DrewGGM/aluna/internal/webhooks"
)

// QueueLedger is the queue for the jobs that write to the ledger and QueueInterest the one of the accrual,
// which runs one at a time: a catch-up can outlast its interval, and the runs it enqueues meanwhile wait and
// then find nothing to do, instead of sharing the work, colliding on its rows and taking the default queue's
// workers from the other jobs. QueueControls is for the same reason the controls', although a run that waits
// would repeat the whole read, so at most one is ever queued or running (runControls); QueueGL, the GL's, likewise. Payments are sent from
// their own queue (payments.Queue), whose jobs wait up to 15 seconds for the rail and would otherwise hold the
// default queue's workers. The rest go to river.QueueDefault.
const (
	QueueLedger   = "ledger"
	QueueInterest = "interest"
	QueueControls = "controls"
	QueueGL       = "gl"
	QueueWebhooks = "webhooks"
)

// The two ledger jobs and the expiry of pending payments keep taking batches until they catch up; the size
// bounds each transaction. An expired payment emits its event in the batch's transaction, so its batches are
// smaller.
const (
	holdsBatch           = 500
	aggregateBatch       = 1000
	pendingPaymentsBatch = 100
)

// The accrual (FR-25) locks accounts batch by batch, so a batch also bounds how long a movement on one of
// them waits, and it must stay under the pool's lock_timeout (2 s) or that movement fails: on a
// capitalization day (FR-26) each account also posts and emits, and 50 accounts take about a quarter of a
// second.
//
// A run catches up every closed day, which after a long stop can take longer than River's default limit of
// a minute; its batches commit as they go, so a run cut short only leaves work for the next.
const (
	accrualBatch   = 50
	accrualTimeout = 30 * time.Minute
)

// The dormancy (FR-27) also locks accounts batch by batch, for less time per batch: each account only changes
// its status and emits. Its first run after going live, or after importing a portfolio, marks every account already past its
// days, which can take longer than River's default minute.
const (
	dormancyBatch   = 50
	dormancyTimeout = 30 * time.Minute
)

// The controls (FR-33) run every 15 minutes, continuous enough for what they find. Each of the 4 reads for up to 5
// minutes, and the rest of the limit is for recording what they found.
const (
	controlsInterval = 15 * time.Minute
	controlsTimeout  = 25 * time.Minute
)

// The GL (FR-36) projects each closed day once, and again if a transaction reached it after its projection: after
// a rollover the previous day is projected within glInterval. A catch-up after a long stop projects many days, the
// newest first, so it has its own queue and the controls' limit, and like them a run is not inserted while another
// waits or runs.
const (
	glInterval = 15 * time.Minute
	glTimeout  = 25 * time.Minute
)

// The statements (FR-45) of a period are generated once it is closed, so an hour is soon enough. A period of many
// accounts takes many batches, longer than River's default minute; they commit as they go, so a run cut short only
// leaves work for the next. Like the GL a run is not inserted while another waits or runs, so it holds at most one
// of the default queue's workers, as the dormancy does, and needs no queue of its own.
const (
	statementsInterval = time.Hour
	statementsBatch    = 200
	statementsTimeout  = 30 * time.Minute
)

// Webhooks (FR-46) are queued and attempted every few seconds, so an integrator hears of an event soon after it
// commits. An attempt can wait 15 seconds for its endpoint, so a run of a full batch of slow endpoints outlasts River's
// default minute; they go on their own queue, one run at a time, so they hold none of the default queue's workers.
const (
	webhooksInterval = 10 * time.Second
	webhooksBatch    = 50
	webhooksTimeout  = 15 * time.Minute
)

// The restrictive lists (ADR-0018) are downloaded every six hours, so that screening, which refuses a list older
// than its maximum age (48 hours by default), keeps working through a day of failed downloads. A run reads some
// 25 MB from three owners.
const (
	listsInterval = 6 * time.Hour
	listsTimeout  = 15 * time.Minute
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
	UnitOfWork     *postgres.UnitOfWork
	Ledger         *ledger.Service
	Idempotency    *idempotency.Store
	Outbox         *outbox.Outbox
	Audit          *audit.Service
	Deposits       *deposits.Service
	Payments       *payments.Service
	Reconciliation *reconciliation.Service
	Accounting     *accounting.Service
	Webhooks       *webhooks.Service
	// WebhookSender is the client webhooks are sent with (webhooks.NewSender).
	WebhookSender webhooks.Sender
	// ListSources are where the restrictive lists are refreshed from, when screening reads them (ADR-0018); nil
	// with the fake screening vendor, which reads none.
	ListSources *compliance.ListSources
}

// Config is the worker's River configuration: the queues, the workers from Register, the periodic jobs
// and the schema. The caller adds the logger and how it shuts down.
func Config(deps Deps) *river.Config {
	workers := river.NewWorkers()
	Register(workers, deps)
	periodic := addRefreshingLists(workers, deps, Periodic())
	return &river.Config{
		Schema: outbox.RiverSchema, Workers: workers, PeriodicJobs: periodic,
		Queues: map[string]river.QueueConfig{
			river.QueueDefault: {MaxWorkers: 4}, QueueLedger: {MaxWorkers: 2}, QueueInterest: {MaxWorkers: 1}, QueueControls: {MaxWorkers: 1},
			QueueGL: {MaxWorkers: 1}, QueueWebhooks: {MaxWorkers: 1}, payments.Queue: {MaxWorkers: 4},
		},
		// REINDEX requires owning the index and the worker runs as aluna_app (migration 0009).
		ReindexerIndexNames: []string{},
	}
}

// Register registers in w the workers of the periodic jobs, the one that publishes the outbox and the one that
// sends payments, and subscribes the consumer of the cash totals to the outbox.
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
	river.AddWorker(w, &task[markDormant]{timeout: dormancyTimeout, run: func(ctx context.Context) error {
		_, err := deps.Deposits.MarkDormant(ctx, deps.UnitOfWork, dormancyBatch)
		return err
	}})
	river.AddWorker(w, &task[runControls]{timeout: controlsTimeout, run: func(ctx context.Context) error {
		return deps.Reconciliation.RunControls(ctx, deps.UnitOfWork)
	}})
	river.AddWorker(w, &task[projectGL]{timeout: glTimeout, run: func(ctx context.Context) error {
		return deps.Accounting.ProjectGL(ctx, deps.UnitOfWork)
	}})
	river.AddWorker(w, deliveringWebhooks(deps))
	river.AddWorker(w, expiringPendingPayments(deps))
	river.AddWorker(w, &task[generateStatements]{timeout: statementsTimeout, run: func(ctx context.Context) error {
		_, err := deps.Deposits.GenerateStatements(ctx, deps.UnitOfWork, statementsBatch)
		return err
	}})
	compliance.FollowCash(deps.Outbox, holderOf(deps.Deposits))
	deps.Outbox.AddWorkers(w)
	deps.Payments.AddWorkers(w, deps.UnitOfWork)
}

// deliveringWebhooks is the worker of deliverWebhooks.
func deliveringWebhooks(deps Deps) *task[deliverWebhooks] {
	return &task[deliverWebhooks]{timeout: webhooksTimeout, run: func(ctx context.Context) error {
		return deps.Webhooks.Deliver(ctx, deps.UnitOfWork, deps.WebhookSender, time.Now, webhooksBatch)
	}}
}

// addRefreshingLists is periodic with the refresh of the lists, whose worker it registers in w, if screening reads
// them (deps.ListSources); periodic as it is otherwise.
func addRefreshingLists(w *river.Workers, deps Deps, periodic []*river.PeriodicJob) []*river.PeriodicJob {
	if deps.ListSources == nil {
		return periodic
	}
	river.AddWorker(w, &task[refreshLists]{timeout: listsTimeout, run: func(ctx context.Context) error {
		return compliance.RefreshLists(ctx, deps.UnitOfWork, *deps.ListSources)
	}})
	return append(periodic, every(listsInterval, refreshLists{}))
}

// expiringPendingPayments is the worker of expirePendingPayments.
func expiringPendingPayments(deps Deps) *task[expirePendingPayments] {
	return &task[expirePendingPayments]{run: func(ctx context.Context) error {
		_, err := deps.Payments.ExpirePending(ctx, deps.UnitOfWork, time.Now(), pendingPaymentsBatch)
		return err
	}}
}

// holderOf answers compliance who holds a ledger account from deposits, where the customers' accounts are.
func holderOf(d *deposits.Service) compliance.HolderOf {
	return func(ctx context.Context, tx postgres.Tx, entity, account ids.ID) (compliance.Holder, bool, error) {
		h, err := d.HoldingOf(ctx, tx, entity, account)
		if errors.Is(err, deposits.ErrAccountNotFound) {
			return compliance.Holder{}, false, nil
		}
		if err != nil {
			return compliance.Holder{}, false, err
		}
		return compliance.Holder(h), true, nil
	}
}

// Periodic are the periodic jobs, but for the refresh of the restrictive lists, which only runs when screening
// reads them (addRefreshingLists). They also run at startup (RunOnStart): River's scheduler lives in the leader's
// memory, and without it a worker that restarts more often than the interval would never run them.
func Periodic() []*river.PeriodicJob {
	return []*river.PeriodicJob{
		every(30*time.Second, expireHolds{}),
		every(10*time.Second, aggregateAsync{}),
		every(time.Hour, purgeIdempotency{}),
		every(24*time.Hour, purgeOutbox{}),
		every(time.Minute, sealAudit{}),
		every(5*time.Minute, accrueInterest{}),
		every(time.Hour, markDormant{}),
		every(controlsInterval, runControls{}),
		every(glInterval, projectGL{}),
		every(statementsInterval, generateStatements{}),
		every(webhooksInterval, deliverWebhooks{}),
		every(time.Hour, expirePendingPayments{}),
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

// refreshLists downloads the restrictive lists screening reads (compliance.RefreshLists).
type refreshLists struct{}

func (refreshLists) Kind() string                 { return "compliance.lists.refresh" }
func (refreshLists) InsertOpts() river.InsertOpts { return insertOpts(river.QueueDefault) }

// expirePendingPayments rejects the payments that waited for compliance too long (payments.ExpirePending).
type expirePendingPayments struct{}

func (expirePendingPayments) Kind() string                 { return "payments.pending.expire" }
func (expirePendingPayments) InsertOpts() river.InsertOpts { return insertOpts(payments.Queue) }

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

// markDormant marks dormant the inactive accounts (FR-27). Dormancy counts days back from the business date,
// which changes once a day; running it every hour only shortens the wait after a rollover.
type markDormant struct{}

func (markDormant) Kind() string                 { return "deposits.accounts.mark_dormant" }
func (markDormant) InsertOpts() river.InsertOpts { return insertOpts(river.QueueDefault) }

// runControls runs the continuous controls (FR-33). A run is not inserted while another is waiting or running.
type runControls struct{}

func (runControls) Kind() string { return "reconciliation.controls.run" }

func (runControls) InsertOpts() river.InsertOpts { return oneAtATime(QueueControls) }

// projectGL projects the daily GL of the closed days (FR-36). A run is not inserted while another is waiting or
// running.
type projectGL struct{}

func (projectGL) Kind() string { return "accounting.gl.project" }

func (projectGL) InsertOpts() river.InsertOpts { return oneAtATime(QueueGL) }

// generateStatements generates the monthly statements of the closed periods (FR-45). A run is not inserted while
// another is waiting or running.
type generateStatements struct{}

func (generateStatements) Kind() string { return "deposits.statements.generate" }

func (generateStatements) InsertOpts() river.InsertOpts { return oneAtATime(river.QueueDefault) }

// deliverWebhooks queues the new events for each webhook endpoint and attempts the due deliveries (FR-46). A run is
// not inserted while another is waiting or running.
type deliverWebhooks struct{}

func (deliverWebhooks) Kind() string { return "webhooks.deliver" }

func (deliverWebhooks) InsertOpts() river.InsertOpts { return oneAtATime(QueueWebhooks) }

// oneAtATime puts a job in queue, like insertOpts, and inserts it only if no other of its kind waits or runs.
func oneAtATime(queue string) river.InsertOpts {
	opts := insertOpts(queue)
	opts.UniqueOpts = river.UniqueOpts{ByState: []rivertype.JobState{
		rivertype.JobStateAvailable, rivertype.JobStatePending, rivertype.JobStateRunning, rivertype.JobStateScheduled,
	}}
	return opts
}
