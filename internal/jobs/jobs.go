// Package jobs cablea los jobs del worker (FR-47, ADR-0007): los registra en River y define los
// periódicos. Cada job es idempotente: correrlo de más, o dos a la vez, deja el mismo resultado (dos a la
// vez pueden chocar en un lock y fallar uno; lo repite el próximo intervalo).
package jobs

import (
	"context"
	"time"

	"github.com/riverqueue/river"

	"github.com/DrewGGM/aluna/internal/audit"
	"github.com/DrewGGM/aluna/internal/idempotency"
	"github.com/DrewGGM/aluna/internal/ledger"
	"github.com/DrewGGM/aluna/internal/outbox"
	"github.com/DrewGGM/aluna/internal/postgres"
)

// QueueLedger es la cola de los jobs que escriben en el ledger; el resto va a river.QueueDefault.
const QueueLedger = "ledger"

// Los dos trabajos del ledger siguen con otro lote hasta ponerse al día; el tamaño acota cada transacción.
const (
	holdsBatch     = 500
	aggregateBatch = 1000
)

// El sellado (ADR-0012) cierra bloques de a lo más auditBlockSize registros, sealRounds por corrida para
// terminar antes del límite de tiempo del job; lo que quede lo toma la corrida siguiente.
const (
	auditBlockSize = 10_000
	sealRounds     = 5
)

const (
	idempotencyRetention = 7 * 24 * time.Hour  // architecture §9
	outboxRetention      = 30 * 24 * time.Hour // la Events API retiene 30 días (FR-46)
)

// Deps son los servicios que usan los jobs.
type Deps struct {
	UnitOfWork  *postgres.UnitOfWork
	Ledger      *ledger.Service
	Idempotency *idempotency.Store
	Outbox      *outbox.Outbox
	Audit       *audit.Service
}

// Config es la configuración de River del worker: las colas, los workers de Register, los periódicos y el
// esquema. Quien la usa agrega el logger y cómo se apaga.
func Config(deps Deps) *river.Config {
	workers := river.NewWorkers()
	Register(workers, deps)
	return &river.Config{
		Schema: outbox.RiverSchema, Workers: workers, PeriodicJobs: Periodic(),
		Queues: map[string]river.QueueConfig{river.QueueDefault: {MaxWorkers: 4}, QueueLedger: {MaxWorkers: 2}},
		// REINDEX exige ser dueño del índice y el worker corre como aluna_app (migración 0009).
		ReindexerIndexNames: []string{},
	}
}

// Register registra en w los workers de los jobs periódicos y el que publica el outbox.
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
	deps.Outbox.AddWorkers(w)
}

// Periodic son los jobs periódicos. Corren también al arrancar (RunOnStart): el scheduler de River vive en
// memoria del líder, y sin eso un worker que reinicia más seguido que el intervalo nunca los correría.
func Periodic() []*river.PeriodicJob {
	return []*river.PeriodicJob{
		every(30*time.Second, expireHolds{}),
		every(10*time.Second, aggregateAsync{}),
		every(time.Hour, purgeIdempotency{}),
		every(24*time.Hour, purgeOutbox{}),
		every(time.Minute, sealAudit{}),
	}
}

func every(interval time.Duration, args river.JobArgs) *river.PeriodicJob {
	return river.NewPeriodicJob(river.PeriodicInterval(interval),
		func() (river.JobArgs, *river.InsertOpts) { return args, nil },
		&river.PeriodicJobOpts{RunOnStart: true})
}

// task es el worker de un job periódico: no tiene argumentos, solo algo que correr.
type task[A river.JobArgs] struct {
	river.WorkerDefaults[A]
	run func(ctx context.Context) error
}

func (t *task[A]) Work(ctx context.Context, _ *river.Job[A]) error { return t.run(ctx) }

// insertOpts pone el job en queue sin reintentos: un periódico que falla lo vuelve a correr el próximo
// intervalo, en vez de acumular reintentos de la misma corrida.
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
