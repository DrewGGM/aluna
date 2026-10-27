//go:build integration

package ledger_test

import (
	"context"
	crand "crypto/rand"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"math/rand/v2"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/DrewGGM/aluna/internal/idempotency"
	"github.com/DrewGGM/aluna/internal/ids"
	"github.com/DrewGGM/aluna/internal/ledger"
	"github.com/DrewGGM/aluna/internal/money"
	"github.com/DrewGGM/aluna/internal/postgres"
)

// Carga del benchmark de posting (QS-3); la describe docs/benchmarks/2026-posting.md.
const (
	benchCustomers = 10_000
	benchWorkers   = 64
	benchScope     = "benchmark POST /v1/transactions"

	benchAggregateBatch = 1000 // el agregador sigue con otro lote hasta ponerse al día
)

// benchLedger son las cuentas del benchmark: se crean una vez y las reusan las corridas con b.N creciente.
type benchLedger struct {
	cur                 money.Currency
	customers           []ids.ID
	settlement, revenue ids.ID
}

var benchSetup = sync.OnceValues(func() (benchLedger, error) { return setUpBenchLedger(context.Background()) })

// BenchmarkPost reporta throughput, p50/p95/p99 y la tasa de reintentos de UnitOfWork de postings de 4
// entries con Idempotency-Key, 64 a la vez, con el agregador async corriendo.
func BenchmarkPost(b *testing.B) {
	requireDurableServer(b)
	l, err := benchSetup()
	if err != nil {
		b.Fatal(err)
	}
	stopAggregator := aggregateEverySecond(b.Context())
	r := runPostings(b, l)
	if err := stopAggregator(); err != nil {
		b.Fatal(err)
	}
	if r.failed > 0 {
		b.Fatalf("%d de %d postings fallaron; el primero: %v", r.failed, b.N, r.firstErr)
	}
	checkAggregated(b, l)
	r.report(b)
}

// requireDurableServer registra el servidor y se niega a medir sin fsync: un ledger se mide con commits
// que esperan el disco. El PostgreSQL de testcontainers arranca con fsync=off; el benchmark corre contra
// uno durable con ALUNA_TEST_DATABASE_URL.
func requireDurableServer(b *testing.B) {
	b.Helper()
	var version, fsync, syncCommit string
	err := testPool.QueryRow(b.Context(), `SELECT version(), current_setting('fsync'), current_setting('synchronous_commit')`).
		Scan(&version, &fsync, &syncCommit)
	if err != nil {
		b.Fatal(err)
	}
	b.Logf("%s; fsync=%s, synchronous_commit=%s; pool de %d conexiones", version, fsync, syncCommit, testPool.Config().MaxConns)
	if fsync != "on" || syncCommit != "on" {
		b.Fatal("el servidor no es durable: corre el benchmark con ALUNA_TEST_DATABASE_URL apuntando a uno con fsync y synchronous_commit en on")
	}
}

// checkAggregated termina de agregar lo que quedó al parar el agregador, lo registra (es su atraso) y
// exige que el settlement quede con la suma de sus entries.
func checkAggregated(b *testing.B, l benchLedger) {
	b.Helper()
	backlog, err := svc().AggregateAsyncBalances(b.Context(), postgres.NewUnitOfWork(testPool), benchAggregateBatch)
	if err != nil {
		b.Fatal(err)
	}
	var posted, entries int64
	err = testPool.QueryRow(b.Context(), `SELECT b.posted, (SELECT coalesce(sum(amount), 0)::bigint FROM ledger.entries WHERE account_id = $1)
		FROM ledger.balances b WHERE b.account_id = $1`, l.settlement).Scan(&posted, &entries)
	if err != nil {
		b.Fatal(err)
	}
	b.Logf("al parar, el agregador tenía %d entries por agregar", backlog)
	if posted != entries {
		b.Fatalf("settlement: saldo %d, Σ entries %d", posted, entries)
	}
}

// setUpBenchLedger crea, en una transacción, los clientes (pasivos sync con sobregiro amplio: se valida
// el límite, pero no hay que fondearlos) y las dos cuentas async compartidas.
func setUpBenchLedger(ctx context.Context) (benchLedger, error) {
	cur, err := money.CurrencyOf("COP")
	if err != nil {
		return benchLedger{}, err
	}
	l := benchLedger{cur: cur}
	account := func(tx postgres.Tx, kind ledger.Kind, mode ledger.BalanceMode, limit int64) (ids.ID, error) {
		a, err := svc().CreateAccount(ctx, tx, ledger.CreateAccountInput{
			LegalEntityID: entity, Code: "bench-" + ids.New().String(), Kind: kind, Currency: cur, Mode: mode, LowerLimit: limit,
		})
		return a.ID, err
	}
	err = postgres.NewUnitOfWork(testPool).Do(ctx, func(tx postgres.Tx) error {
		l.customers = make([]ids.ID, benchCustomers)
		var err error
		for i := range l.customers {
			if l.customers[i], err = account(tx, ledger.KindLiability, ledger.BalanceModeSync, -1_000_000_000_000); err != nil {
				return err
			}
		}
		if l.settlement, err = account(tx, ledger.KindAsset, ledger.BalanceModeAsync, 0); err != nil {
			return err
		}
		l.revenue, err = account(tx, ledger.KindIncome, ledger.BalanceModeAsync, 0)
		return err
	})
	if err != nil {
		return benchLedger{}, fmt.Errorf("crear las cuentas del benchmark: %w", err)
	}
	return l, nil
}

// aggregateEverySecond corre el agregador async cada segundo, como lo hará el job (T-031), hasta que se
// llama la función que devuelve.
func aggregateEverySecond(ctx context.Context) func() error {
	ctx, cancel := context.WithCancel(ctx)
	done := make(chan error, 1)
	go func() {
		uow := postgres.NewUnitOfWork(testPool)
		tick := time.NewTicker(time.Second)
		defer tick.Stop()
		for {
			select {
			case <-ctx.Done():
				done <- nil
				return
			case <-tick.C:
				if _, err := svc().AggregateAsyncBalances(ctx, uow, benchAggregateBatch); err != nil && ctx.Err() == nil {
					done <- fmt.Errorf("agregador: %w", err)
					return
				}
			}
		}
	}()
	return func() error { cancel(); return <-done }
}

// benchResult es lo que midió una corrida de b.N postings.
type benchResult struct {
	failed    int
	firstErr  error
	attempts  int64
	elapsed   time.Duration
	latencies []time.Duration
}

func runPostings(b *testing.B, l benchLedger) benchResult {
	b.Helper()
	r := benchResult{latencies: make([]time.Duration, b.N)}
	uow, keys := postgres.NewUnitOfWork(testPool), idempotency.NewStore(testPool)
	var next, attempts, failed atomic.Int64
	var firstErr sync.Once
	b.ResetTimer()
	start := time.Now()
	var wg sync.WaitGroup
	for w := range benchWorkers {
		wg.Go(func() {
			rng := rand.New(rand.NewPCG(uint64(w), uint64(start.UnixNano()))) //nolint:gosec // elegir cuentas al azar, no es criptografía
			for {
				i := next.Add(1) - 1
				if i >= int64(b.N) {
					return
				}
				began := time.Now()
				if err := postIdempotent(b.Context(), uow, keys, l.posting(rng), &attempts); err != nil {
					failed.Add(1)
					firstErr.Do(func() { r.firstErr = err })
				}
				r.latencies[i] = time.Since(began)
			}
		})
	}
	wg.Wait()
	r.elapsed = time.Since(start)
	b.StopTimer()
	r.attempts, r.failed = attempts.Load(), int(failed.Load())
	return r
}

// posting arma los 4 entries: un cliente paga 1.000 a otro, que recibe 900; 90 van al settlement y 10
// al ingreso de la comisión. Los dos clientes son distintos: con el mismo, Post netearía sus dos entries.
func (l benchLedger) posting(rng *rand.Rand) []ledger.EntryInput {
	from := rng.IntN(len(l.customers))
	to := (from + 1 + rng.IntN(len(l.customers)-1)) % len(l.customers)
	entry := func(account ids.ID, units int64) ledger.EntryInput {
		return ledger.EntryInput{AccountID: account, Amount: money.New(units, l.cur)}
	}
	return []ledger.EntryInput{entry(l.customers[from], 1000), entry(l.customers[to], -900), entry(l.settlement, -90), entry(l.revenue, -10)}
}

// postIdempotent asienta entries con las sentencias de commands.idempotent y underSavepoint
// (internal/httpapi/idempotency.go) para un comando que pasa, en su orden: reclamo de la clave, savepoint,
// Post, respuesta guardada y registro de auditoría. Si ese camino suma una sentencia, hay que sumarla
// aquí; la huella, el scope, el cuerpo y el resumen se simplifican porque no cambian el SQL. La clave
// (también la referencia externa) es al azar, como la de un cliente: una clave ordenada en el tiempo
// insertaría siempre al final de los índices. Cuenta cada intento de la transacción: los que pasan de uno
// son reintentos de UnitOfWork.
func postIdempotent(ctx context.Context, uow *postgres.UnitOfWork, keys *idempotency.Store, entries []ledger.EntryInput, attempts *atomic.Int64) error {
	key, today := crand.Text(), time.Now().UTC().Truncate(24*time.Hour)
	in := ledger.PostInput{
		LegalEntityID: entity, Code: "pago", ValueDate: today, BusinessDate: today,
		Source: "benchmark", ExternalRef: key, Actor: "bench", Channel: "api", RuleVersion: "manual.v1", Entries: entries,
	}
	hash := sha256.Sum256([]byte(key))
	return uow.Do(ctx, func(tx postgres.Tx) error {
		attempts.Add(1)
		if _, err := keys.Claim(ctx, tx, benchScope, key, hash); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, "SAVEPOINT command_effect"); err != nil {
			return err
		}
		posted, err := svc().Post(ctx, tx, in)
		if err != nil {
			return err
		}
		body, err := json.Marshal(posted)
		if err != nil {
			return err
		}
		if err := keys.Complete(ctx, tx, benchScope, key, 201, body); err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `INSERT INTO audit.records (actor, role, channel, ip, correlation_id, command, targets, result, after)
			VALUES ('api', 'api', 'api', '127.0.0.1', $1, 'POST /v1/transactions', $2, 'ok', $3)`, key, []string{posted.ID.String()}, body)
		return err
	})
}

// report publica las métricas del benchmark. Son tiempos y tasas, no montos: ReportMetric pide float64.
//
//nolint:forbidigo // métricas del benchmark, no dinero (ADR-0005)
func (r benchResult) report(b *testing.B) {
	b.Helper()
	n := len(r.latencies)
	slices.Sort(r.latencies)
	at := func(p float64) float64 {
		return float64(r.latencies[int(p*float64(n-1))].Microseconds()) / 1000
	}
	b.ReportMetric(float64(n)/r.elapsed.Seconds(), "postings/s")
	b.ReportMetric(at(0.50), "p50_ms")
	b.ReportMetric(at(0.95), "p95_ms")
	b.ReportMetric(at(0.99), "p99_ms")
	b.ReportMetric(100*float64(r.attempts-int64(n))/float64(n), "retries_%")
	b.ReportMetric(0, "ns/op") // el tiempo por posting lo dicen los percentiles: 64 en paralelo
}
