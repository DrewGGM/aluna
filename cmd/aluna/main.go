// Command aluna is the single binary of the core banking system: each process (API, worker,
// migrations) is a subcommand.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/riverdriver/riverpgxv5"

	contract "github.com/DrewGGM/aluna/api"
	"github.com/DrewGGM/aluna/config"
	"github.com/DrewGGM/aluna/internal/access"
	"github.com/DrewGGM/aluna/internal/approvals"
	"github.com/DrewGGM/aluna/internal/audit"
	"github.com/DrewGGM/aluna/internal/calendar"
	"github.com/DrewGGM/aluna/internal/httpapi"
	"github.com/DrewGGM/aluna/internal/idempotency"
	"github.com/DrewGGM/aluna/internal/jobs"
	"github.com/DrewGGM/aluna/internal/ledger"
	"github.com/DrewGGM/aluna/internal/outbox"
	"github.com/DrewGGM/aluna/internal/party"
	"github.com/DrewGGM/aluna/internal/postgres"
	"github.com/DrewGGM/aluna/internal/telemetry"
)

// version is injected at build time: go build -ldflags "-X main.version=1.2.3" ./cmd/aluna
var version = "dev"

// stderr is a variable so tests can capture the usage text.
var stderr io.Writer = os.Stderr

const usage = `usage: aluna <command>

commands:
  api                        serves the HTTP API on ALUNA_HTTP_ADDR (default :8080); requires
                             ALUNA_DATABASE_URL, ALUNA_OIDC_ISSUER (the token issuer) and
                             ALUNA_OIDC_AUDIENCE (the audience tokens must carry); with
                             OTEL_EXPORTER_OTLP_ENDPOINT it exports traces and metrics over OTLP
  worker                     runs the jobs (outbox, holds, async balances, purges, audit sealing)
                             on ALUNA_DATABASE_URL, anchoring blocks in ALUNA_AUDIT_ANCHOR_DIR;
                             multiple workers elect a leader for the periodic jobs
  audit verify --from N --to M
                             recomputes the audit chain of blocks N to M and checks it against
                             their anchors in ALUNA_AUDIT_ANCHOR_DIR
  migrate up|down|status     applies, rolls back the latest or lists the migrations on
                             ALUNA_DATABASE_URL
  version                    prints the version
`

// shutdownTimeout is the longest shutdown waits for in-flight requests or jobs to finish.
const shutdownTimeout = 20 * time.Second

func main() {
	os.Exit(run(os.Args[1:], os.Stdout))
}

// run executes the given subcommand and returns the process exit code.
func run(args []string, stdout io.Writer) int {
	if len(args) == 0 {
		return printUsage()
	}
	command, rest := args[0], args[1:]
	switch command {
	case "migrate":
		return migrateCommand(rest, stdout)
	case "audit":
		return auditCommand(rest, stdout)
	}
	if len(rest) != 0 { // the other commands take no arguments
		return printUsage()
	}
	switch command {
	case "api":
		return untilSignal(func(ctx context.Context) int { return apiCommand(ctx, stdout) })
	case "worker":
		return untilSignal(func(ctx context.Context) int { return workerCommand(ctx, stdout) })
	case "version":
		return printVersion(stdout)
	default:
		return printUsage()
	}
}

func migrateCommand(args []string, stdout io.Writer) int {
	if len(args) != 1 || !postgres.ValidDirection(args[0]) {
		return printUsage()
	}
	return untilSignal(func(ctx context.Context) int {
		return exitCode(runMigrate(ctx, os.Getenv("ALUNA_DATABASE_URL"), args[0], stdout))
	})
}

func printVersion(stdout io.Writer) int {
	if _, err := fmt.Fprintf(stdout, "aluna %s\n", version); err != nil {
		return 1
	}
	return 0
}

// exitCode prints the error of a one-shot subcommand and turns it into an exit code.
func exitCode(err error) int {
	if err == nil {
		return 0
	}
	fmt.Fprintln(stderr, err) //nolint:errcheck // if stderr fails there is nowhere to report it
	return 1
}

func printUsage() int {
	fmt.Fprint(stderr, usage) //nolint:errcheck // if stderr fails there is nowhere to report it
	return 2
}

// untilSignal runs fn with a context that SIGINT or SIGTERM cancels. After the first signal the default
// behavior is restored, so a second Ctrl+C exits without waiting for the shutdown.
func untilSignal(fn func(ctx context.Context) int) int {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	context.AfterFunc(ctx, stop)
	return fn(ctx)
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

// apiCommand reads the configuration from the environment, sets up telemetry and runs the api; logs go
// to stdout as JSON.
func apiCommand(ctx context.Context, stdout io.Writer) int {
	log, shutdown, err := telemetry.Setup(ctx, "aluna", version, stdout)
	if err != nil {
		return exitCode(fmt.Errorf("telemetry: %w", err))
	}
	defer flushTelemetry(ctx, shutdown, log)
	cfg, err := apiConfigFromEnv()
	if err != nil {
		log.Error("api configuration", "error", err)
		return 1
	}
	verifier, err := httpapi.NewOIDCVerifier(ctx, cfg.issuer, cfg.audience, log)
	if err != nil {
		log.Error("token issuer", "error", err)
		return 1
	}
	authz, err := newAuthorization()
	if err != nil {
		log.Error("authorization", "error", err)
		return 1
	}
	return runAPI(ctx, cfg, verifier, authz, log)
}

// authorization holds the role policy, which also decides who approves each request type, and the
// authorizer of the contract operations.
type authorization struct {
	policy     *access.Policy
	authorizer *httpapi.Authorizer
}

// newAuthorization builds the authorization from the role policy and the contract embedded in the binary.
func newAuthorization() (authorization, error) {
	policy, err := access.ParsePolicy(config.Roles)
	if err != nil {
		return authorization{}, err
	}
	authorizer, err := httpapi.NewAuthorizer(contract.OpenAPI, policy)
	if err != nil {
		return authorization{}, err
	}
	return authorization{policy: policy, authorizer: authorizer}, nil
}

// flushTelemetry sends any remaining traces and metrics before exiting, waiting no longer than
// telemetryFlushTimeout for an unresponsive collector.
func flushTelemetry(ctx context.Context, shutdown telemetry.Shutdown, log *slog.Logger) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), telemetryFlushTimeout)
	defer cancel()
	if err := shutdown(ctx); err != nil {
		log.Error("flush telemetry", "error", err)
	}
}

// apiConfig is the configuration of aluna api. The legal entity of each request comes from its token
// (FR-41), not from the configuration.
type apiConfig struct {
	addr, dsn        string
	issuer, audience string
}

func apiConfigFromEnv() (apiConfig, error) {
	cfg := apiConfig{addr: envOr("ALUNA_HTTP_ADDR", ":8080"), dsn: os.Getenv("ALUNA_DATABASE_URL")}
	if cfg.dsn == "" {
		return apiConfig{}, errNoDatabaseURL
	}
	cfg.issuer, cfg.audience = os.Getenv("ALUNA_OIDC_ISSUER"), os.Getenv("ALUNA_OIDC_AUDIENCE")
	if cfg.issuer == "" {
		return apiConfig{}, fmt.Errorf("%w: missing ALUNA_OIDC_ISSUER", errNoOIDC)
	}
	if cfg.audience == "" {
		return apiConfig{}, fmt.Errorf("%w: missing ALUNA_OIDC_AUDIENCE", errNoOIDC)
	}
	return cfg, nil
}

var (
	errNoDatabaseURL = errors.New("missing ALUNA_DATABASE_URL")
	errNoOIDC        = errors.New("incomplete OIDC configuration")
)

// connectTimeout bounds the initial connection: if the database does not respond, the process fails
// instead of leaving the port open without serving.
const connectTimeout = 10 * time.Second

// telemetryFlushTimeout bounds the final send of traces and metrics on shutdown.
const telemetryFlushTimeout = 5 * time.Second

// readyTimeout bounds the /readyz ping: a probe must not hang if the database does not respond.
const readyTimeout = 2 * time.Second

// runAPI opens the port first (a busy port fails without touching the database), connects to the
// database, wires the ledger by hand and serves until ctx is canceled.
func runAPI(ctx context.Context, cfg apiConfig, verifier httpapi.TokenVerifier, authz authorization, log *slog.Logger) int {
	ln, err := new(net.ListenConfig).Listen(ctx, "tcp", cfg.addr)
	if err != nil {
		log.Error("could not open the port", "addr", cfg.addr, "error", err)
		return 1
	}
	openCtx, cancel := context.WithTimeout(ctx, connectTimeout)
	defer cancel()
	pool, err := postgres.Open(openCtx, cfg.dsn)
	if err != nil {
		log.Error("could not connect to the database", "error", err)
		if closeErr := ln.Close(); closeErr != nil {
			log.Error("close the port", "error", closeErr)
		}
		return 1
	}
	defer pool.Close()
	events, err := outbox.New(pool)
	if err != nil {
		log.Error("configure the outbox", "error", err)
		return 1
	}
	uow := postgres.NewUnitOfWork(pool)
	keys := idempotency.NewStore(pool)
	ledgerSvc := ledger.New(calendar.New(), events)
	requests := approvals.New(authz.policy)
	api := httpapi.API{
		LedgerHandlers:   httpapi.NewLedgerHandlers(ledgerSvc, requests, uow, keys, pool),
		ApprovalHandlers: httpapi.NewApprovalHandlers(requests, uow, keys, pool),
		PartyHandlers:    httpapi.NewPartyHandlers(party.New(events), uow, pool),
	}
	ready := func(ctx context.Context) error {
		ctx, cancel := context.WithTimeout(ctx, readyTimeout)
		defer cancel()
		return pool.Ping(ctx)
	}
	serverCfg := httpapi.ServerConfig{
		Ready: ready, Audit: &httpapi.CommandAudit{Records: audit.New(), UoW: uow}, Verifier: verifier, Authorizer: authz.authorizer,
	}
	return serveAPI(ctx, ln, httpapi.NewServer(serverCfg, httpapi.Routes(api, log), log), log)
}

// serveAPI serves on ln until ctx is canceled and then waits up to shutdownTimeout for in-flight
// requests to finish.
func serveAPI(ctx context.Context, ln net.Listener, srv *http.Server, log *slog.Logger) int {
	served := make(chan error, 1)
	go func() { served <- srv.Serve(ln) }()
	log.Info("api listening", "addr", ln.Addr().String())

	select {
	case err := <-served:
		log.Error("server stopped", "error", err)
		return 1
	case <-ctx.Done():
	}
	shutdownCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), shutdownTimeout)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		log.Error("incomplete shutdown", "error", err)
		return 1
	}
	if err := <-served; !errors.Is(err, http.ErrServerClosed) {
		log.Error("server stopped", "error", err)
		return 1
	}
	log.Info("api stopped")
	return 0
}

// workerCommand sets up telemetry and runs the worker on ALUNA_DATABASE_URL; logs go to stdout as JSON.
func workerCommand(ctx context.Context, stdout io.Writer) int {
	log, shutdown, err := telemetry.Setup(ctx, "aluna", version, stdout)
	if err != nil {
		return exitCode(fmt.Errorf("telemetry: %w", err))
	}
	defer flushTelemetry(ctx, shutdown, log)
	dsn := os.Getenv("ALUNA_DATABASE_URL")
	if dsn == "" {
		log.Error("worker configuration", "error", errNoDatabaseURL)
		return 1
	}
	records, err := auditFromEnv()
	if err != nil {
		log.Error("worker configuration", "error", err)
		return 1
	}
	return runWorker(ctx, dsn, records, log)
}

var errNoAnchorDir = errors.New("missing ALUNA_AUDIT_ANCHOR_DIR")

// auditFromEnv returns the audit service that seals and verifies, anchoring blocks in
// ALUNA_AUDIT_ANCHOR_DIR.
func auditFromEnv() (*audit.Service, error) {
	dir := os.Getenv("ALUNA_AUDIT_ANCHOR_DIR")
	if dir == "" {
		return nil, errNoAnchorDir
	}
	return audit.WithFileAnchor(dir)
}

// runWorker connects to the database, wires the jobs by hand and runs River until ctx is canceled. It then
// stops taking jobs and waits up to shutdownTimeout for the running ones before canceling them.
func runWorker(ctx context.Context, dsn string, records *audit.Service, log *slog.Logger) int {
	openCtx, cancel := context.WithTimeout(ctx, connectTimeout)
	defer cancel()
	pool, err := postgres.Open(openCtx, dsn)
	if err != nil {
		log.Error("could not connect to the database", "error", err)
		return 1
	}
	defer pool.Close()
	client, err := newWorker(pool, records, log)
	if err != nil {
		log.Error("configure the worker", "error", err)
		return 1
	}
	if err := client.Start(ctx); err != nil {
		log.Error("start the worker", "error", err)
		return 1
	}
	log.Info("worker running")
	<-client.Stopped()
	log.Info("worker stopped")
	return 0
}

// newWorker builds the worker's River client from the jobs configuration, wired by hand.
func newWorker(pool *pgxpool.Pool, records *audit.Service, log *slog.Logger) (*river.Client[pgx.Tx], error) {
	events, err := outbox.New(pool)
	if err != nil {
		return nil, err
	}
	cfg := jobs.Config(jobs.Deps{
		UnitOfWork: postgres.NewUnitOfWork(pool), Ledger: ledger.New(calendar.New(), events), Idempotency: idempotency.NewStore(pool), Outbox: events,
		Audit: records,
	})
	cfg.SoftStopTimeout, cfg.Logger = shutdownTimeout, log
	client, err := river.NewClient(riverpgxv5.New(pool), cfg)
	if err != nil {
		return nil, fmt.Errorf("river client: %w", err)
	}
	return client, nil
}

// auditCommand runs audit verify --from N --to M.
func auditCommand(args []string, stdout io.Writer) int {
	if len(args) == 0 || args[0] != "verify" {
		return printUsage()
	}
	flags := flag.NewFlagSet("audit verify", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	from, to := flags.Int64("from", 0, ""), flags.Int64("to", 0, "")
	if err := flags.Parse(args[1:]); err != nil || flags.NArg() != 0 || *from < 1 || *to < *from {
		return printUsage()
	}
	return untilSignal(func(ctx context.Context) int {
		return exitCode(runVerify(ctx, os.Getenv("ALUNA_DATABASE_URL"), *from, *to, stdout))
	})
}

// runVerify recomputes the audit chain of blocks from through to and checks it against their anchors.
func runVerify(ctx context.Context, dsn string, from, to int64, out io.Writer) error {
	if dsn == "" {
		return errNoDatabaseURL
	}
	records, err := auditFromEnv()
	if err != nil {
		return err
	}
	openCtx, cancel := context.WithTimeout(ctx, connectTimeout)
	defer cancel()
	pool, err := postgres.Open(openCtx, dsn)
	if err != nil {
		return err
	}
	defer pool.Close()
	if err := records.Verify(ctx, pool, from, to); err != nil {
		return err
	}
	_, err = fmt.Fprintf(out, "audit chain intact: blocks %d to %d (or up to the last sealed one)\n", from, to)
	return err
}

// runMigrate applies direction to the dsn database over the migrations' own connection.
func runMigrate(ctx context.Context, dsn, direction string, out io.Writer) error {
	if dsn == "" {
		return errNoDatabaseURL
	}
	db, err := postgres.OpenForMigrations(dsn)
	if err != nil {
		return err
	}
	defer db.Close() //nolint:errcheck // the process exits right after
	return postgres.Migrate(ctx, db, direction, out)
}
