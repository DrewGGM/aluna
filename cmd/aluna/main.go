// Command aluna es el binario único del core bancario: cada proceso (API, worker, migraciones)
// es un subcomando.
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

	"github.com/DrewGGM/aluna/internal/audit"
	"github.com/DrewGGM/aluna/internal/calendar"
	"github.com/DrewGGM/aluna/internal/httpapi"
	"github.com/DrewGGM/aluna/internal/idempotency"
	"github.com/DrewGGM/aluna/internal/jobs"
	"github.com/DrewGGM/aluna/internal/ledger"
	"github.com/DrewGGM/aluna/internal/outbox"
	"github.com/DrewGGM/aluna/internal/postgres"
	"github.com/DrewGGM/aluna/internal/telemetry"
)

// version se inyecta al compilar: go build -ldflags "-X main.version=1.2.3" ./cmd/aluna
var version = "dev"

// stderr es una variable para que las pruebas capturen el uso.
var stderr io.Writer = os.Stderr

const usage = `uso: aluna <comando>

comandos:
  api                        sirve la API HTTP en ALUNA_HTTP_ADDR (por defecto :8080); necesita
                             ALUNA_DATABASE_URL, ALUNA_OIDC_ISSUER (el emisor de los tokens) y
                             ALUNA_OIDC_AUDIENCE (la audiencia que deben traer); con
                             OTEL_EXPORTER_OTLP_ENDPOINT exporta trazas y métricas por OTLP
  worker                     corre los jobs (outbox, holds, saldos async, purgas, sellado de la
                             auditoría) sobre ALUNA_DATABASE_URL, con el ancla de los bloques en
                             ALUNA_AUDIT_ANCHOR_DIR; varios workers eligen un líder para los periódicos
  audit verify --from N --to M
                             recalcula la cadena de auditoría de los bloques N a M y la compara con
                             sus anclas en ALUNA_AUDIT_ANCHOR_DIR
  migrate up|down|status     aplica, revierte la última o lista las migraciones en ALUNA_DATABASE_URL
  version                    imprime la versión
`

// shutdownTimeout es lo máximo que se espera a que terminen los requests o los jobs en curso al apagar.
const shutdownTimeout = 20 * time.Second

func main() {
	os.Exit(run(os.Args[1:], os.Stdout))
}

// run ejecuta el subcomando indicado y devuelve el código de salida del proceso.
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
	if len(rest) != 0 { // los demás comandos no llevan argumentos
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

// exitCode imprime el error de un subcomando de una sola ejecución y lo convierte en código de salida.
func exitCode(err error) int {
	if err == nil {
		return 0
	}
	fmt.Fprintln(stderr, err) //nolint:errcheck // si stderr falla no hay dónde reportarlo
	return 1
}

func printUsage() int {
	fmt.Fprint(stderr, usage) //nolint:errcheck // si stderr falla no hay dónde reportarlo
	return 2
}

// untilSignal corre fn con un contexto que se cancela con SIGINT o SIGTERM. Tras la primera señal se
// restaura el comportamiento normal, así una segunda Ctrl+C corta sin esperar el apagado.
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

// apiCommand lee la configuración del entorno, prepara la telemetría y corre la api; los logs van en
// JSON a stdout.
func apiCommand(ctx context.Context, stdout io.Writer) int {
	log, shutdown, err := telemetry.Setup(ctx, "aluna", version, stdout)
	if err != nil {
		return exitCode(fmt.Errorf("telemetría: %w", err))
	}
	defer flushTelemetry(ctx, shutdown, log)
	cfg, err := apiConfigFromEnv()
	if err != nil {
		log.Error("configuración de la api", "error", err)
		return 1
	}
	verifier, err := httpapi.NewOIDCVerifier(ctx, cfg.issuer, cfg.audience, log)
	if err != nil {
		log.Error("emisor de los tokens", "error", err)
		return 1
	}
	return runAPI(ctx, cfg, verifier, log)
}

// flushTelemetry envía lo que quede de trazas y métricas antes de salir, sin esperar más de
// telemetryFlushTimeout a un colector que no responde.
func flushTelemetry(ctx context.Context, shutdown telemetry.Shutdown, log *slog.Logger) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), telemetryFlushTimeout)
	defer cancel()
	if err := shutdown(ctx); err != nil {
		log.Error("vaciar la telemetría", "error", err)
	}
}

// apiConfig es la configuración de aluna api. La entidad legal de cada request sale de su token (FR-41),
// no de la configuración.
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
		return apiConfig{}, fmt.Errorf("%w: falta ALUNA_OIDC_ISSUER", errNoOIDC)
	}
	if cfg.audience == "" {
		return apiConfig{}, fmt.Errorf("%w: falta ALUNA_OIDC_AUDIENCE", errNoOIDC)
	}
	return cfg, nil
}

var (
	errNoDatabaseURL = errors.New("falta ALUNA_DATABASE_URL")
	errNoOIDC        = errors.New("configuración OIDC incompleta")
)

// connectTimeout acota la conexión inicial: si la base no responde, el proceso falla en vez de dejar el
// puerto abierto sin servir.
const connectTimeout = 10 * time.Second

// telemetryFlushTimeout acota el envío final de trazas y métricas al apagar.
const telemetryFlushTimeout = 5 * time.Second

// readyTimeout acota el ping de /readyz: un probe no debe colgarse si la base no responde.
const readyTimeout = 2 * time.Second

// runAPI abre el puerto primero (un puerto ocupado falla sin tocar la base), conecta la base, cablea
// el ledger a mano y sirve hasta que ctx se cancela.
func runAPI(ctx context.Context, cfg apiConfig, verifier httpapi.TokenVerifier, log *slog.Logger) int {
	ln, err := new(net.ListenConfig).Listen(ctx, "tcp", cfg.addr)
	if err != nil {
		log.Error("no se pudo abrir el puerto", "addr", cfg.addr, "error", err)
		return 1
	}
	openCtx, cancel := context.WithTimeout(ctx, connectTimeout)
	defer cancel()
	pool, err := postgres.Open(openCtx, cfg.dsn)
	if err != nil {
		log.Error("no se pudo conectar a la base", "error", err)
		if closeErr := ln.Close(); closeErr != nil {
			log.Error("cerrar el puerto", "error", closeErr)
		}
		return 1
	}
	defer pool.Close()
	events, err := outbox.New(pool)
	if err != nil {
		log.Error("configurar el outbox", "error", err)
		return 1
	}
	uow := postgres.NewUnitOfWork(pool)
	api := httpapi.API{
		LedgerHandlers: httpapi.NewLedgerHandlers(ledger.New(calendar.New(), events), uow, idempotency.NewStore(pool), pool),
	}
	ready := func(ctx context.Context) error {
		ctx, cancel := context.WithTimeout(ctx, readyTimeout)
		defer cancel()
		return pool.Ping(ctx)
	}
	serverCfg := httpapi.ServerConfig{Ready: ready, Audit: &httpapi.CommandAudit{Records: audit.New(), UoW: uow}, Verifier: verifier}
	return serveAPI(ctx, ln, httpapi.NewServer(serverCfg, httpapi.Routes(api, log), log), log)
}

// serveAPI sirve en ln hasta que ctx se cancela y luego espera hasta shutdownTimeout a que terminen
// los requests en curso.
func serveAPI(ctx context.Context, ln net.Listener, srv *http.Server, log *slog.Logger) int {
	served := make(chan error, 1)
	go func() { served <- srv.Serve(ln) }()
	log.Info("api escuchando", "addr", ln.Addr().String())

	select {
	case err := <-served:
		log.Error("el servidor se detuvo", "error", err)
		return 1
	case <-ctx.Done():
	}
	shutdownCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), shutdownTimeout)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		log.Error("apagado incompleto", "error", err)
		return 1
	}
	if err := <-served; !errors.Is(err, http.ErrServerClosed) {
		log.Error("el servidor se detuvo", "error", err)
		return 1
	}
	log.Info("api detenida")
	return 0
}

// workerCommand prepara la telemetría y corre el worker sobre ALUNA_DATABASE_URL; los logs van en JSON a
// stdout.
func workerCommand(ctx context.Context, stdout io.Writer) int {
	log, shutdown, err := telemetry.Setup(ctx, "aluna", version, stdout)
	if err != nil {
		return exitCode(fmt.Errorf("telemetría: %w", err))
	}
	defer flushTelemetry(ctx, shutdown, log)
	dsn := os.Getenv("ALUNA_DATABASE_URL")
	if dsn == "" {
		log.Error("configuración del worker", "error", errNoDatabaseURL)
		return 1
	}
	records, err := auditFromEnv()
	if err != nil {
		log.Error("configuración del worker", "error", err)
		return 1
	}
	return runWorker(ctx, dsn, records, log)
}

var errNoAnchorDir = errors.New("falta ALUNA_AUDIT_ANCHOR_DIR")

// auditFromEnv es el servicio de auditoría que sella y verifica, con el ancla de los bloques en
// ALUNA_AUDIT_ANCHOR_DIR.
func auditFromEnv() (*audit.Service, error) {
	dir := os.Getenv("ALUNA_AUDIT_ANCHOR_DIR")
	if dir == "" {
		return nil, errNoAnchorDir
	}
	return audit.WithFileAnchor(dir)
}

// runWorker conecta la base, cablea los jobs a mano y corre River hasta que ctx se cancela. Entonces deja
// de tomar jobs y espera hasta shutdownTimeout a los que están corriendo antes de cancelarlos.
func runWorker(ctx context.Context, dsn string, records *audit.Service, log *slog.Logger) int {
	openCtx, cancel := context.WithTimeout(ctx, connectTimeout)
	defer cancel()
	pool, err := postgres.Open(openCtx, dsn)
	if err != nil {
		log.Error("no se pudo conectar a la base", "error", err)
		return 1
	}
	defer pool.Close()
	client, err := newWorker(pool, records, log)
	if err != nil {
		log.Error("configurar el worker", "error", err)
		return 1
	}
	if err := client.Start(ctx); err != nil {
		log.Error("arrancar el worker", "error", err)
		return 1
	}
	log.Info("worker corriendo")
	<-client.Stopped()
	log.Info("worker detenido")
	return 0
}

// newWorker arma el cliente River del worker con la configuración de jobs, cableada a mano.
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
		return nil, fmt.Errorf("cliente River: %w", err)
	}
	return client, nil
}

// auditCommand corre audit verify --from N --to M.
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

// runVerify recalcula la cadena de auditoría de los bloques from a to contra sus anclas.
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
	_, err = fmt.Fprintf(out, "cadena de auditoría íntegra: bloques %d a %d (o hasta el último sellado)\n", from, to)
	return err
}

// runMigrate aplica direction sobre la base de dsn con la conexión propia de las migraciones.
func runMigrate(ctx context.Context, dsn, direction string, out io.Writer) error {
	if dsn == "" {
		return errNoDatabaseURL
	}
	db, err := postgres.OpenForMigrations(dsn)
	if err != nil {
		return err
	}
	defer db.Close() //nolint:errcheck // el proceso termina enseguida
	return postgres.Migrate(ctx, db, direction, out)
}
