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

// Load of the posting benchmark (QS-3); docs/benchmarks/2026-posting.md describes it.
const (
	benchCustomers = 10_000
	benchWorkers   = 64
	benchScope     = "benchmark POST /v1/transactions"

	benchAggregateBatch = 1000 // the aggregator keeps going with another batch until it catches up
)

// benchLedger is the benchmark's accounts: created once and reused by the runs with growing b.N.
type benchLedger struct {
	cur                 money.Currency
	customers           []ids.ID
	settlement, revenue ids.ID
}

var benchSetup = sync.OnceValues(func() (benchLedger, error) { return setUpBenchLedger(context.Background()) })

// BenchmarkPost reports throughput, p50/p95/p99 and the UnitOfWork retry rate of 4-entry postings with an
// Idempotency-Key, 64 at a time, with the async aggregator running.
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
		b.Fatalf("%d of %d postings failed; the first one: %v", r.failed, b.N, r.firstErr)
	}
	checkAggregated(b, l)
	r.report(b)
}

// requireDurableServer logs the server and refuses to measure without fsync: a ledger is measured with
// commits that wait for the disk. The testcontainers PostgreSQL starts with fsync=off; the benchmark runs
// against a durable one with ALUNA_TEST_DATABASE_URL.
func requireDurableServer(b *testing.B) {
	b.Helper()
	var version, fsync, syncCommit string
	err := testPool.QueryRow(b.Context(), `SELECT version(), current_setting('fsync'), current_setting('synchronous_commit')`).
		Scan(&version, &fsync, &syncCommit)
	if err != nil {
		b.Fatal(err)
	}
	b.Logf("%s; fsync=%s, synchronous_commit=%s; pool of %d connections", version, fsync, syncCommit, testPool.Config().MaxConns)
	if fsync != "on" || syncCommit != "on" {
		b.Fatal("the server is not durable: run the benchmark with ALUNA_TEST_DATABASE_URL pointing to one with fsync and synchronous_commit on")
	}
}

// checkAggregated finishes aggregating what was left when the aggregator stopped, logs it (it is its lag)
// and requires the settlement to end with the sum of its entries.
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
	b.Logf("when stopped, the aggregator had %d entries left to aggregate", backlog)
	if posted != entries {
		b.Fatalf("settlement: balance %d, Σ entries %d", posted, entries)
	}
}

// setUpBenchLedger creates, in one transaction, the customers (sync liabilities with a wide overdraft:
// the limit is checked, but they need no funding) and the two shared async accounts.
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
		return benchLedger{}, fmt.Errorf("create benchmark accounts: %w", err)
	}
	return l, nil
}

// aggregateEverySecond runs the async aggregator every second, as the job will (T-031), until the
// function it returns is called.
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
					done <- fmt.Errorf("aggregator: %w", err)
					return
				}
			}
		}
	}()
	return func() error { cancel(); return <-done }
}

// benchResult is what a run of b.N postings measured.
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
			rng := rand.New(rand.NewPCG(uint64(w), uint64(start.UnixNano()))) //nolint:gosec // picking random accounts, not cryptography
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

// posting builds the 4 entries: a customer pays 1,000 to another, who receives 900; 90 go to the
// settlement and 10 to the fee income. The two customers are different: with the same one, Post would net
// its two entries.
func (l benchLedger) posting(rng *rand.Rand) []ledger.EntryInput {
	from := rng.IntN(len(l.customers))
	to := (from + 1 + rng.IntN(len(l.customers)-1)) % len(l.customers)
	entry := func(account ids.ID, units int64) ledger.EntryInput {
		return ledger.EntryInput{AccountID: account, Amount: money.New(units, l.cur)}
	}
	return []ledger.EntryInput{entry(l.customers[from], 1000), entry(l.customers[to], -900), entry(l.settlement, -90), entry(l.revenue, -10)}
}

// postIdempotent posts entries with the statements of commands.idempotent and underSavepoint
// (internal/httpapi/idempotency.go) for a command that succeeds, in their order: key claim, savepoint,
// Post, stored response and audit record. If that path adds a statement, it has to be added here; the
// fingerprint, scope, body and summary are simplified because they do not change the SQL. The key (also
// the external reference) is random, like a client's: a time-ordered key would always insert at the end
// of the indexes. It counts each attempt of the transaction: those beyond one are UnitOfWork retries.
func postIdempotent(ctx context.Context, uow *postgres.UnitOfWork, keys *idempotency.Store, entries []ledger.EntryInput, attempts *atomic.Int64) error {
	key, today := crand.Text(), time.Now().UTC().Truncate(24*time.Hour)
	in := ledger.PostInput{
		LegalEntityID: entity, Code: "payment", ValueDate: today, BusinessDate: today,
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

// report publishes the benchmark metrics. They are times and rates, not amounts: ReportMetric takes
// float64.
//
//nolint:forbidigo // benchmark metrics, not money (ADR-0005)
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
	b.ReportMetric(0, "ns/op") // the percentiles give the time per posting: 64 in parallel
}
