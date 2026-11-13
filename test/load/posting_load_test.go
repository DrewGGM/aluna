//go:build load

// Package load puts the compiled binary under a constant HTTP load (QS-3, SM-3, SM-4) against a durable PostgreSQL;
// docs/benchmarks/2026-mvp-load.md reports a run.
package load

import (
	"bytes"
	"context"
	crand "crypto/rand"
	"crypto/rsa"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
	"github.com/coreos/go-oidc/v3/oidc/oidctest"
	"github.com/jackc/pgx/v5"

	"github.com/DrewGGM/aluna/internal/ids"
)

// The load of QS-3 over HTTP: postingRate postings a second, each of 4 entries with an Idempotency-Key, between two of
// the customers and the shared async settlement and revenue accounts, and balanceRate reads of a customer's balance a
// second, both at a constant rate whatever the api answers.
const (
	postingRate     = 300
	balanceRate     = 50
	customers       = 1000
	defaultDuration = 10 * time.Minute
)

// The targets: SM-3 and SM-4, and the rate sustained within 1 %.
const (
	postingP99   = 200 * time.Millisecond
	balanceP99   = 100 * time.Millisecond
	minSustained = 0.99
)

const (
	audience   = "aluna-api"
	railSecret = "a secret of the simulated rail, for the load test"
)

// TestPostingLoad runs the load for ALUNA_LOAD_DURATION (10 minutes unless it says otherwise) against aluna api and
// aluna worker, built without -race, and requires every posting to be posted with p99 under 200 ms, every balance read
// with p99 under 100 ms, no 5xx and the rate sustained. It logs what it measured.
func TestPostingLoad(t *testing.T) {
	duration := loadDuration(t)
	server := os.Getenv("ALUNA_TEST_DATABASE_URL")
	if server == "" {
		t.Fatal("the load test needs ALUNA_TEST_DATABASE_URL: a durable PostgreSQL, not one of testcontainers (fsync=off)")
	}
	bin := buildAluna(t)
	dsn := createDatabase(t, server)
	requireDurable(t, dsn)
	if out, err := alunaCmd(t, bin, []string{"ALUNA_DATABASE_URL=" + dsn}, "migrate", "up").CombinedOutput(); err != nil {
		t.Fatalf("aluna migrate up: %v\n%s", err, out)
	}
	// as the tables' owner, because no API creates entities (Q-136); the worker's jobs then run over it during the load
	entity := ids.New()
	if err := execOn(t.Context(), dsn, `INSERT INTO calendar.legal_entities (id, name, business_date, functional_currency)
		VALUES ($1, 'Load', current_date, 'COP')`, entity); err != nil {
		t.Fatal(err)
	}
	a := startSystem(t, bin, dsn, entity)
	l := createLedger(t, a)

	r := runLoad(t, a, l, duration)

	r.report(t, duration)
	r.check(t, duration)
	expectAllPosted(t, dsn, r.postings.ok)
}

// loadDuration is ALUNA_LOAD_DURATION, for shorter runs on a workstation, or defaultDuration.
func loadDuration(t *testing.T) time.Duration {
	t.Helper()
	v := os.Getenv("ALUNA_LOAD_DURATION")
	if v == "" {
		return defaultDuration
	}
	d, err := time.ParseDuration(v)
	if err != nil || d < time.Second {
		t.Fatalf("ALUNA_LOAD_DURATION=%q: want a duration of at least 1s", v)
	}
	return d
}

// buildAluna builds cmd/aluna without -race, as it is deployed, into a temporary directory.
func buildAluna(t *testing.T) string {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "aluna")
	if runtime.GOOS == "windows" {
		bin += ".exe"
	}
	if out, err := exec.CommandContext(t.Context(), "go", "build", "-o", bin, "github.com/DrewGGM/aluna/cmd/aluna").CombinedOutput(); err != nil { //nolint:gosec // bin is a path in the test's TempDir
		t.Fatalf("build aluna: %v\n%s", err, out)
	}
	return bin
}

// createDatabase creates an empty database with a random name (only [a-z2-7]: no injection possible) on server, drops
// it when the test ends and returns its DSN, with the server's user, who will own the tables.
func createDatabase(t *testing.T, server string) string {
	t.Helper()
	u, err := url.Parse(server)
	if err != nil || (u.Scheme != "postgres" && u.Scheme != "postgresql") {
		t.Fatal("ALUNA_TEST_DATABASE_URL must be a postgres:// URL") // the DSN carries the password: it stays out of the log
	}
	name := "aluna_load_" + strings.ToLower(crand.Text())
	if err := execOn(t.Context(), server, "CREATE DATABASE "+name); err != nil {
		t.Fatalf("create database %s: %v", name, err)
	}
	t.Cleanup(func() {
		if err := execOn(context.Background(), server, "DROP DATABASE IF EXISTS "+name+" WITH (FORCE)"); err != nil {
			t.Errorf("drop database %s: %v", name, err)
		}
	})
	u.Path = "/" + name
	return u.String()
}

func execOn(ctx context.Context, dsn, sql string, args ...any) error {
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		return err
	}
	_, err = conn.Exec(ctx, sql, args...)
	return errors.Join(err, conn.Close(ctx))
}

// requireDurable logs the server and refuses to measure without fsync, as the posting benchmark does: a ledger is
// measured with commits that wait for the disk.
func requireDurable(t *testing.T, dsn string) {
	t.Helper()
	conn, err := pgx.Connect(t.Context(), dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(context.Background()) //nolint:errcheck // only read from
	var version, fsync, syncCommit, maxConnections string
	if err := conn.QueryRow(t.Context(), `SELECT version(), current_setting('fsync'), current_setting('synchronous_commit'),
		current_setting('max_connections')`).Scan(&version, &fsync, &syncCommit, &maxConnections); err != nil {
		t.Fatal(err)
	}
	t.Logf("%s; fsync=%s, synchronous_commit=%s, max_connections=%s; client %s/%s with %d CPUs",
		version, fsync, syncCommit, maxConnections, runtime.GOOS, runtime.GOARCH, runtime.NumCPU())
	if fsync != "on" || syncCommit != "on" {
		t.Fatal("the server is not durable: run the load test against one with fsync and synchronous_commit on")
	}
}

// appDSN is dsn connecting with the aluna_app role, like the real processes.
func appDSN(t *testing.T, dsn string) string {
	t.Helper()
	u, err := url.Parse(dsn)
	if err != nil {
		t.Fatal("invalid database DSN")
	}
	query := u.Query()
	query.Set("options", "-c role=aluna_app")
	u.RawQuery = strings.ReplaceAll(query.Encode(), "+", "%20") // pgx does not read "+" as a space
	return u.String()
}

// alunaCmd prepares the binary with the process environment plus env, without exporting telemetry.
func alunaCmd(t *testing.T, bin string, env []string, args ...string) *exec.Cmd {
	t.Helper()
	cmd := exec.CommandContext(t.Context(), bin, args...) //nolint:gosec // bin was built by buildAluna
	cmd.Env = append(os.Environ(), append([]string{"OTEL_EXPORTER_OTLP_ENDPOINT="}, env...)...)
	return cmd
}

// start runs aluna command with env until the test ends; if the test fails, its log is printed.
func start(t *testing.T, bin, command string, env ...string) {
	t.Helper()
	cmd := alunaCmd(t, bin, env, command)
	var logs bytes.Buffer // os/exec does not write to Stdout and Stderr concurrently if they are the same writer
	cmd.Stdout, cmd.Stderr = &logs, &logs
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		cmd.Wait() //nolint:errcheck,gosec // the process dies with the test context
		if t.Failed() {
			t.Logf("aluna %s log:\n%s", command, logs.String())
		}
	})
}

// target is the running api and the tokens of an operator of the entity.
type target struct {
	base   string
	tokens *tokens
	client *http.Client
}

// startSystem starts aluna api, with a test OIDC issuer of entity's operator, and aluna worker on dsn as aluna_app,
// as they run in production, and waits for the api to be ready.
func startSystem(t *testing.T, bin, dsn string, entity ids.ID) target {
	t.Helper()
	app := appDSN(t, dsn)
	issuer, sign := oidcIssuer(t, entity)
	addr := freeAddr(t)
	start(t, bin, "api", "ALUNA_HTTP_ADDR="+addr, "ALUNA_RAIL_ADDR="+freeAddr(t), "ALUNA_DATABASE_URL="+app,
		"ALUNA_OIDC_ISSUER="+issuer, "ALUNA_OIDC_AUDIENCE="+audience, "ALUNA_SIMBREB_SECRET="+railSecret)
	start(t, bin, "worker", "ALUNA_DATABASE_URL="+app, "ALUNA_AUDIT_ANCHOR_DIR="+t.TempDir(), "ALUNA_SIMBREB_SECRET="+railSecret)
	a := target{base: "http://" + addr, tokens: &tokens{sign: sign}, client: &http.Client{
		Timeout:   10 * time.Second,
		Transport: &http.Transport{MaxIdleConns: 1024, MaxIdleConnsPerHost: 1024, IdleConnTimeout: time.Minute},
	}}
	waitReady(t, a)
	return a
}

// tokens hands out the operator's token, signed again every few minutes: a token lives 15 minutes at most and a run
// can last longer.
type tokens struct {
	mu       sync.Mutex
	sign     func() string
	token    string
	signedAt time.Time
}

func (k *tokens) get() string {
	k.mu.Lock()
	defer k.mu.Unlock()
	if time.Since(k.signedAt) > 5*time.Minute {
		k.token, k.signedAt = k.sign(), time.Now()
	}
	return k.token
}

// oidcIssuer is a test OIDC issuer and a function that signs the token of an operator of entity.
func oidcIssuer(t *testing.T, entity ids.ID) (issuer string, sign func() string) {
	t.Helper()
	key, err := rsa.GenerateKey(crand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	s := &oidctest.Server{PublicKeys: []oidctest.PublicKey{{PublicKey: key.Public(), KeyID: "k1", Algorithm: oidc.RS256}}}
	ts := httptest.NewUnstartedServer(s)
	s.SetIssuer("http://" + ts.Listener.Addr().String()) // before it serves: afterwards it would be read concurrently
	ts.Start()
	t.Cleanup(ts.Close)
	return ts.URL, func() string {
		now := time.Now()
		claims := fmt.Sprintf(`{"iss":%q,"aud":%q,"sub":"operator-load","iat":%d,"exp":%d,"legal_entity_id":%q,"roles":["operator"]}`,
			ts.URL, audience, now.Unix(), now.Add(15*time.Minute).Unix(), entity)
		return oidctest.SignIDToken(key, "k1", oidc.RS256, claims)
	}
}

// freeAddr reserves a free port and releases it for the process to take.
func freeAddr(t *testing.T) string {
	t.Helper()
	ln, err := new(net.ListenConfig).Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	if err := ln.Close(); err != nil {
		t.Fatal(err)
	}
	return addr
}

// waitReady polls /readyz until it answers 200; while the process starts, the connection fails.
func waitReady(t *testing.T, a target) {
	t.Helper()
	for deadline := time.Now().Add(30 * time.Second); time.Now().Before(deadline); time.Sleep(100 * time.Millisecond) {
		if status, err := a.send(t.Context(), http.MethodGet, "/readyz", "", nil); err == nil && status == http.StatusOK {
			return
		}
	}
	t.Fatal("aluna api did not answer 200 on /readyz within 30 s")
}

// send makes a request with the operator's token, and a new Idempotency-Key if it is a POST, decodes the body into out
// if it is not nil and returns the status. It reads the whole body, so the connection goes back to the pool.
func (a target) send(ctx context.Context, method, path, body string, out any) (int, error) {
	req, err := http.NewRequestWithContext(ctx, method, a.base+path, strings.NewReader(body))
	if err != nil {
		return 0, err
	}
	req.Header.Set("Authorization", "Bearer "+a.tokens.get())
	req.Header.Set("Content-Type", "application/json")
	if method == http.MethodPost {
		req.Header.Set("Idempotency-Key", ids.New().String())
	}
	resp, err := a.client.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close() //nolint:errcheck // read in full below
	if out != nil {
		err = json.NewDecoder(resp.Body).Decode(out)
	}
	_, drained := io.Copy(io.Discard, resp.Body)
	return resp.StatusCode, errors.Join(err, drained)
}

// ledgerAccounts are the customers (sync liabilities with a wide overdraft: the limit is checked, but they need no
// funding) and the shared async settlement and revenue accounts.
type ledgerAccounts struct {
	customers           []string
	settlement, revenue string
}

// createLedger creates the accounts over HTTP, 16 at a time.
func createLedger(t *testing.T, a target) ledgerAccounts {
	t.Helper()
	l := ledgerAccounts{customers: make([]string, customers)}
	next := make(chan int)
	var wg sync.WaitGroup
	for range 16 {
		wg.Go(func() {
			for i := range next {
				l.customers[i] = createAccount(t, a, "liability", "sync", -1_000_000_000_000)
			}
		})
	}
	for i := range customers {
		next <- i
	}
	close(next)
	wg.Wait()
	l.settlement, l.revenue = createAccount(t, a, "asset", "async", 0), createAccount(t, a, "income", "async", 0)
	if t.Failed() {
		t.FailNow()
	}
	return l
}

// createAccount creates a COP account. It runs outside the test's goroutine too, so it reports a failure with t.Error.
func createAccount(t *testing.T, a target, kind, mode string, lowerLimit int64) string {
	t.Helper()
	body := fmt.Sprintf(`{"code":"load-%s","kind":%q,"currency":"COP","balance_mode":%q,"lower_limit":%d}`, ids.New(), kind, mode, lowerLimit)
	var account struct{ ID string }
	if status, err := a.send(t.Context(), http.MethodPost, "/v1/ledger-accounts", body, &account); err != nil || status != http.StatusCreated {
		t.Errorf("create %s account = %d (%v)", kind, status, err)
	}
	return account.ID
}

// posting is the body of a posting of 4 entries on day: a customer pays 1,000 to another, who receives 900; 90 go to
// the settlement and 10 to the revenue. The two customers differ: with the same one, the ledger would net its entries.
func (l ledgerAccounts) posting(rng *rand.Rand, day string) string {
	from := rng.IntN(len(l.customers))
	to := (from + 1 + rng.IntN(len(l.customers)-1)) % len(l.customers)
	return fmt.Sprintf(`{"code":"payment","value_date":%q,"business_date":%q,"source":"load","external_ref":%q,"entries":[`+
		`{"account_id":%q,"amount":{"amount":1000,"currency":"COP"}},{"account_id":%q,"amount":{"amount":-900,"currency":"COP"}},`+
		`{"account_id":%q,"amount":{"amount":-90,"currency":"COP"}},{"account_id":%q,"amount":{"amount":-10,"currency":"COP"}}]}`,
		day, day, crand.Text(), l.customers[from], l.customers[to], l.settlement, l.revenue)
}

// stream is what one kind of request measured. Each latency counts from when the request was due, not from when it
// went out: a client that fell behind adds its delay instead of hiding it.
type stream struct {
	mu           sync.Mutex
	latencies    []time.Duration
	ok           int
	serverErrors int // 5xx
	otherStatus  int
	failed       int // no response
	firstProblem string
	lastDone     time.Time
}

func (s *stream) record(due time.Time, want, status int, err error) {
	done := time.Now()
	s.mu.Lock()
	defer s.mu.Unlock()
	s.latencies = append(s.latencies, done.Sub(due))
	if done.After(s.lastDone) {
		s.lastDone = done
	}
	switch {
	case err != nil:
		s.failed++
	case status >= http.StatusInternalServerError:
		s.serverErrors++
	case status != want:
		s.otherStatus++
	default:
		s.ok++
		return
	}
	if s.firstProblem == "" {
		s.firstProblem = fmt.Sprintf("status %d, error %v", status, err)
	}
}

// result is what a run measured.
type result struct {
	start           time.Time
	postings, reads stream
}

// runLoad sends the postings and the balance reads at their constant rates for d, waits for the last answers and
// sorts the latencies, as percentile needs them.
func runLoad(t *testing.T, a target, l ledgerAccounts, d time.Duration) *result {
	t.Helper()
	ctx := context.WithoutCancel(t.Context()) // the requests in flight finish even if the test fails meanwhile
	r := &result{start: time.Now()}
	day := r.start.UTC().Format(time.DateOnly)
	var drivers, requests sync.WaitGroup
	drivers.Go(func() {
		rng := rand.New(rand.NewPCG(1, uint64(r.start.UnixNano()))) //nolint:gosec // picking accounts, not cryptography
		drive(r.start, postingRate, d, func(due time.Time) {
			body := l.posting(rng, day)
			requests.Go(func() {
				status, err := a.send(ctx, http.MethodPost, "/v1/transactions", body, nil)
				r.postings.record(due, http.StatusCreated, status, err)
			})
		})
	})
	drivers.Go(func() {
		rng := rand.New(rand.NewPCG(2, uint64(r.start.UnixNano()))) //nolint:gosec // picking accounts, not cryptography
		drive(r.start, balanceRate, d, func(due time.Time) {
			path := "/v1/ledger-accounts/" + l.customers[rng.IntN(len(l.customers))] + "/balance"
			requests.Go(func() {
				status, err := a.send(ctx, http.MethodGet, path, "", nil)
				r.reads.record(due, http.StatusOK, status, err)
			})
		})
	})
	drivers.Wait()
	requests.Wait()
	slices.Sort(r.postings.latencies)
	slices.Sort(r.reads.latencies)
	return r
}

// drive calls fire for each of the rate×d requests of an open load from start, the n-th (from 0) due at
// start + n/rate, whatever the earlier ones are doing: every millisecond it fires those that came due, the n-th as soon
// as n/rate has passed, so n is due once floor(elapsed×rate) ≥ n.
func drive(start time.Time, rate int, d time.Duration, fire func(due time.Time)) {
	total := requestsIn(rate, d)
	tick := time.NewTicker(time.Millisecond)
	defer tick.Stop()
	for sent := 0; sent < total; <-tick.C {
		for due := min(int(time.Since(start)*time.Duration(rate)/time.Second)+1, total); sent < due; sent++ {
			fire(start.Add(time.Duration(sent) * time.Second / time.Duration(rate)))
		}
	}
}

// requestsIn is how many requests a load of rate a second sends in d.
func requestsIn(rate int, d time.Duration) int { return int(d * time.Duration(rate) / time.Second) }

// percentile is the latency below which fraction p of them fall; latencies must be sorted.
func percentile(latencies []time.Duration, p float64) time.Duration {
	if len(latencies) == 0 {
		return 0
	}
	return latencies[int(p*float64(len(latencies)-1))]
}

// rate is how many requests a second s answered as it should, from start to its last answer.
func (s *stream) rate(start time.Time) float64 {
	return float64(s.ok) / s.lastDone.Sub(start).Seconds()
}

// summary logs what s measured, in the report's terms.
func (s *stream) summary(t *testing.T, name string, start time.Time) {
	t.Helper()
	ms := func(p float64) float64 { return float64(percentile(s.latencies, p).Microseconds()) / 1000 }
	t.Logf("%s: %d sent, %d ok, %d 5xx, %d other status, %d without response, %.1f/s answered ok; "+
		"p50 %.1f ms, p95 %.1f ms, p99 %.1f ms, max %.1f ms; first problem: %q",
		name, len(s.latencies), s.ok, s.serverErrors, s.otherStatus, s.failed, s.rate(start),
		ms(0.50), ms(0.95), ms(0.99), ms(1), s.firstProblem)
}

// report logs the run.
func (r *result) report(t *testing.T, d time.Duration) {
	t.Helper()
	t.Logf("load of %s: %d postings/s and %d balance reads/s over %d customers", d, postingRate, balanceRate, customers)
	r.postings.summary(t, "postings", r.start)
	r.reads.summary(t, "balance reads", r.start)
}

// check requires the targets: every posting sent and answered 201 at the rate, every read answered
// 200, nothing 5xx, and the p99 of each under its target.
func (r *result) check(t *testing.T, d time.Duration) {
	t.Helper()
	if sent, want := len(r.postings.latencies), requestsIn(postingRate, d); sent != want || r.postings.rate(r.start) < minSustained*postingRate {
		t.Errorf("%d postings sent of %d, %.1f/s answered ok; want %d/s", sent, want, r.postings.rate(r.start), postingRate)
	}
	for _, s := range []struct {
		name   string
		stream *stream
		target time.Duration
	}{{"posting", &r.postings, postingP99}, {"balance read", &r.reads, balanceP99}} {
		if s.stream.ok != len(s.stream.latencies) {
			t.Errorf("%s: %d of %d not ok, %d of them 5xx; the first: %s", s.name, len(s.stream.latencies)-s.stream.ok,
				len(s.stream.latencies), s.stream.serverErrors, s.stream.firstProblem)
		}
		if p99 := percentile(s.stream.latencies, 0.99); p99 >= s.target {
			t.Errorf("%s p99 = %s, want under %s", s.name, p99, s.target)
		}
	}
}

// expectAllPosted requires the ledger to hold exactly the postings the api answered 201: every one is posted, once.
func expectAllPosted(t *testing.T, dsn string, ok int) {
	t.Helper()
	conn, err := pgx.Connect(t.Context(), dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(context.Background()) //nolint:errcheck // only read from
	var posted int
	if err := conn.QueryRow(t.Context(), `SELECT count(*) FROM ledger.transactions WHERE source = 'load'`).Scan(&posted); err != nil {
		t.Fatal(err)
	}
	if posted != ok {
		t.Errorf("the ledger holds %d postings, the api answered %d with 201", posted, ok)
	}
}
