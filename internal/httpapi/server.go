// Package httpapi es el adaptador de entrada HTTP: servidor, middleware, errores RFC 9457 y los
// handlers que traducen la API a las fachadas de los módulos.
package httpapi

//go:generate go tool oapi-codegen -config ../../api/oapi-codegen.yaml ../../api/openapi.yaml

import (
	"context"
	"log/slog"
	"net/http"
	"time"

	"github.com/DrewGGM/aluna/internal/httpapi/gen"
)

// API reúne los handlers de todos los módulos: el contrato genera una sola interfaz con todas sus
// operaciones y cada módulo aporta las suyas por embedding.
type API struct {
	*LedgerHandlers
}

var _ gen.StrictServerInterface = API{}

// Routes registra todas las operaciones del contrato en mux, una sola vez, con los errores en
// problem+json.
func Routes(api gen.StrictServerInterface, log *slog.Logger) func(*http.ServeMux) {
	return func(mux *http.ServeMux) {
		strict := gen.NewStrictHandlerWithOptions(api, nil, gen.StrictHTTPServerOptions{
			RequestErrorHandlerFunc:  requestError,
			ResponseErrorHandlerFunc: responseError(log),
		})
		gen.HandlerWithOptions(strict, gen.StdHTTPServerOptions{BaseRouter: mux, ErrorHandlerFunc: requestError})
	}
}

// Rutas de los probes del orquestador.
const (
	pathHealthz = "/healthz"
	pathReadyz  = "/readyz"
)

// ServerConfig configura el servidor HTTP.
type ServerConfig struct {
	// Ready dice si el proceso puede atender tráfico (base de datos, dependencias). Nil es "listo".
	Ready func(ctx context.Context) error
	// Audit registra los comandos (POST); nil solo en un servidor sin rutas de comandos.
	Audit *CommandAudit
}

// NewServer arma el servidor con /healthz, /readyz, las rutas que registra routes (puede ser nil) y
// el middleware común. Quien lo usa le da el listener con Serve.
func NewServer(cfg ServerConfig, routes func(*http.ServeMux), log *slog.Logger) *http.Server {
	mux := http.NewServeMux()
	mux.HandleFunc("GET "+pathHealthz, healthz)
	mux.HandleFunc("GET "+pathReadyz, readyz(cfg.Ready, log))
	if routes != nil {
		routes(mux)
	}
	return &http.Server{
		Handler:           middleware(log, mux, cfg.Audit),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      15 * time.Second,
		IdleTimeout:       60 * time.Second,
		ErrorLog:          slog.NewLogLogger(log.Handler(), slog.LevelError),
	}
}

func healthz(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.Write([]byte(`{"status":"ok"}` + "\n")) //nolint:errcheck,gosec // si el cliente cortó, no hay a quién responder
}

func readyz(ready func(context.Context) error, log *slog.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if ready != nil {
			if err := ready(r.Context()); err != nil {
				log.LogAttrs(r.Context(), slog.LevelWarn, "no listo", slog.String("error", err.Error()))
				WriteProblem(w, r, Problem{
					Title: "Servicio no disponible", Code: "not_ready", Status: http.StatusServiceUnavailable,
				})
				return
			}
		}
		healthz(w, r)
	}
}
