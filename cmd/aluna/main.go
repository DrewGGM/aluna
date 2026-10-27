// Command aluna es el binario único del core bancario: cada proceso (API, worker, migraciones)
// es un subcomando.
package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/DrewGGM/aluna/internal/httpapi"
	"github.com/DrewGGM/aluna/internal/ids"
	"github.com/DrewGGM/aluna/internal/ledger"
	"github.com/DrewGGM/aluna/internal/postgres"
)

// version se inyecta al compilar: go build -ldflags "-X main.version=1.2.3" ./cmd/aluna
var version = "dev"

// stderr es una variable para que las pruebas capturen el uso.
var stderr io.Writer = os.Stderr

const usage = `uso: aluna <comando>

comandos:
  api                        sirve la API HTTP en ALUNA_HTTP_ADDR (por defecto :8080); necesita
                             ALUNA_DATABASE_URL y ALUNA_LEGAL_ENTITY_ID
  migrate up|down|status     aplica, revierte la última o lista las migraciones en ALUNA_DATABASE_URL
  version                    imprime la versión
`

// shutdownTimeout es lo máximo que se espera a que terminen los requests en curso al apagar.
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
	case "api":
		if len(rest) != 0 {
			return printUsage()
		}
		return untilSignal(func(ctx context.Context) int { return apiCommand(ctx, stdout) })
	case "migrate":
		if len(rest) != 1 || !postgres.ValidDirection(rest[0]) {
			return printUsage()
		}
		return untilSignal(func(ctx context.Context) int {
			return exitCode(runMigrate(ctx, os.Getenv("ALUNA_DATABASE_URL"), rest[0], stdout))
		})
	case "version":
		if len(rest) != 0 {
			return printUsage()
		}
		if _, err := fmt.Fprintf(stdout, "aluna %s\n", version); err != nil {
			return 1
		}
		return 0
	default:
		return printUsage()
	}
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

// apiCommand lee la configuración del entorno y corre la api; los logs van en JSON a stdout.
func apiCommand(ctx context.Context, stdout io.Writer) int {
	log := slog.New(slog.NewJSONHandler(stdout, nil))
	cfg, err := apiConfigFromEnv()
	if err != nil {
		log.Error("configuración de la api", "error", err)
		return 1
	}
	return runAPI(ctx, cfg, log)
}

// apiConfig es la configuración de aluna api. Un despliegue sirve a una sola entidad regulada
// (ADR-0002): su entidad legal es configuración hasta que llegue del token (T-035).
type apiConfig struct {
	addr, dsn string
	entity    ids.ID
}

func apiConfigFromEnv() (apiConfig, error) {
	cfg := apiConfig{addr: envOr("ALUNA_HTTP_ADDR", ":8080"), dsn: os.Getenv("ALUNA_DATABASE_URL")}
	if cfg.dsn == "" {
		return apiConfig{}, errNoDatabaseURL
	}
	entity, err := ids.Parse(os.Getenv("ALUNA_LEGAL_ENTITY_ID"))
	if err != nil {
		return apiConfig{}, fmt.Errorf("ALUNA_LEGAL_ENTITY_ID: %w", err)
	}
	cfg.entity = entity
	return cfg, nil
}

var errNoDatabaseURL = errors.New("falta ALUNA_DATABASE_URL")

// connectTimeout acota la conexión inicial: si la base no responde, el proceso falla en vez de dejar el
// puerto abierto sin servir.
const connectTimeout = 10 * time.Second

// readyTimeout acota el ping de /readyz: un probe no debe colgarse si la base no responde.
const readyTimeout = 2 * time.Second

// runAPI abre el puerto primero (un puerto ocupado falla sin tocar la base), conecta la base, cablea
// el ledger a mano y sirve hasta que ctx se cancela.
func runAPI(ctx context.Context, cfg apiConfig, log *slog.Logger) int {
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
	api := httpapi.API{
		LedgerHandlers: httpapi.NewLedgerHandlers(ledger.New(), postgres.NewUnitOfWork(pool), pool, cfg.entity),
	}
	ready := func(ctx context.Context) error {
		ctx, cancel := context.WithTimeout(ctx, readyTimeout)
		defer cancel()
		return pool.Ping(ctx)
	}
	return serveAPI(ctx, ln, httpapi.NewServer(httpapi.ServerConfig{Ready: ready}, httpapi.Routes(api, log), log), log)
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
