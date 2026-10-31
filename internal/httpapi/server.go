// Package httpapi is the inbound HTTP adapter: server, middleware, RFC 9457 errors and the handlers that
// translate the API to the module facades.
package httpapi

//go:generate go tool oapi-codegen -config ../../api/oapi-codegen.yaml ../../api/openapi.yaml

import (
	"context"
	"log/slog"
	"net/http"
	"time"

	"github.com/DrewGGM/aluna/internal/httpapi/gen"
)

// API gathers the handlers of every module: the contract generates a single interface with all its
// operations and each module contributes its own by embedding.
type API struct {
	*LedgerHandlers
	*ApprovalHandlers
	*PartyHandlers
}

var _ gen.StrictServerInterface = API{}

// Routes registers every operation of the contract in mux, once, with errors as problem+json.
func Routes(api gen.StrictServerInterface, log *slog.Logger) func(*http.ServeMux) {
	return func(mux *http.ServeMux) {
		strict := gen.NewStrictHandlerWithOptions(api, nil, gen.StrictHTTPServerOptions{
			RequestErrorHandlerFunc:  requestError,
			ResponseErrorHandlerFunc: responseError(log),
		})
		gen.HandlerWithOptions(strict, gen.StdHTTPServerOptions{BaseRouter: mux, ErrorHandlerFunc: requestError})
	}
}

// Paths of the orchestrator probes.
const (
	pathHealthz = "/healthz"
	pathReadyz  = "/readyz"
)

// ServerConfig configures the HTTP server.
type ServerConfig struct {
	// Ready reports whether the process can serve traffic (database, dependencies). Nil means "ready".
	Ready func(ctx context.Context) error
	// Audit records the commands (POST); nil only in a server without command routes.
	Audit *CommandAudit
	// Verifier verifies the token of every request that is not a probe; nil only in a server without
	// business routes (the handlers do not run without a principal).
	Verifier TokenVerifier
	// Authorizer authorizes every authenticated request; with Verifier and without Authorizer, every route is
	// denied.
	Authorizer *Authorizer
}

// NewServer builds the server with /healthz, /readyz, the routes that routes registers (it may be nil) and
// the common middleware. The caller hands it the listener with Serve.
func NewServer(cfg ServerConfig, routes func(*http.ServeMux), log *slog.Logger) *http.Server {
	mux := http.NewServeMux()
	mux.HandleFunc("GET "+pathHealthz, healthz)
	mux.HandleFunc("GET "+pathReadyz, readyz(cfg.Ready, log))
	if routes != nil {
		routes(mux)
	}
	return &http.Server{
		Handler:           middleware(log, mux, cfg),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      15 * time.Second,
		IdleTimeout:       60 * time.Second,
		ErrorLog:          slog.NewLogLogger(log.Handler(), slog.LevelError),
	}
}

func healthz(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.Write([]byte(`{"status":"ok"}` + "\n")) //nolint:errcheck,gosec // if the client hung up, there is no one to answer
}

func readyz(ready func(context.Context) error, log *slog.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if ready != nil {
			if err := ready(r.Context()); err != nil {
				log.LogAttrs(r.Context(), slog.LevelWarn, "not ready", slog.String("error", err.Error()))
				WriteProblem(w, r, Problem{
					Title: "Service unavailable", Code: "not_ready", Status: http.StatusServiceUnavailable,
				})
				return
			}
		}
		healthz(w, r)
	}
}
