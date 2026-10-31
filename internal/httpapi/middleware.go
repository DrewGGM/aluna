package httpapi

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"runtime/debug"
	"slices"
	"strings"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/propagation"
	semconv "go.opentelemetry.io/otel/semconv/v1.43.0"
	"go.opentelemetry.io/otel/trace"
)

const (
	headerRequestID = "X-Request-Id"
	maxRequestIDLen = 128
	maxBodyBytes    = 1 << 20
	requestIDChars  = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789-_."
)

// middleware wraps mux with the common chain, from outermost to innermost. A new middleware is added to
// this list with the shape func(http.Handler) http.Handler. Auditing goes before authentication, to also
// record a command rejected without a token, and before recovery, to see a command that panicked as a 500.
// Authorization sits right after authentication: one is never mounted without the other. Without cfg.Audit
// or cfg.Verifier, the server only has probes.
func middleware(log *slog.Logger, mux *http.ServeMux, cfg ServerConfig) http.Handler {
	chain := []func(http.Handler) http.Handler{withRequestID, withObservability(log)}
	if cfg.Audit != nil {
		chain = append(chain, withAudit(cfg.Audit, mux, log))
	}
	if cfg.Verifier != nil {
		chain = append(chain, Authenticate(cfg.Verifier), withAuthorization(cfg.Authorizer, mux, log))
	}
	chain = append(chain, withRecovery(log), withBodyLimit, withProblemForUnrouted, withRoute)
	var h http.Handler = mux
	for _, mw := range slices.Backward(chain) {
		h = mw(h)
	}
	return h
}

type requestIDKey struct{}

func requestIDFrom(ctx context.Context) string {
	id, _ := ctx.Value(requestIDKey{}).(string)
	return id
}

// withRequestID keeps the client's X-Request-Id only if it is safe to write to a log; otherwise, it
// generates one.
func withRequestID(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := r.Header.Get(headerRequestID)
		if !validRequestID(id) {
			id = rand.Text()
		}
		w.Header().Set(headerRequestID, id)
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), requestIDKey{}, id)))
	})
}

func validRequestID(id string) bool {
	return id != "" && len(id) <= maxRequestIDLen && strings.Trim(id, requestIDChars) == ""
}

// statusRecorder remembers the response status; 0 means nothing has been written yet.
type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (s *statusRecorder) WriteHeader(code int) {
	if s.status == 0 {
		s.status = code
	}
	s.ResponseWriter.WriteHeader(code)
}

func (s *statusRecorder) Write(b []byte) (int, error) {
	if s.status == 0 {
		s.status = http.StatusOK
	}
	return s.ResponseWriter.Write(b)
}

func (s *statusRecorder) Unwrap() http.ResponseWriter { return s.ResponseWriter }

// Flush keeps http.Flusher and marks the response as sent: after a flush a 500 no longer fits.
func (s *statusRecorder) Flush() {
	if s.status == 0 {
		s.status = http.StatusOK
	}
	http.NewResponseController(s.ResponseWriter).Flush() //nolint:errcheck,gosec // Flusher has no way to return the error; the next Write will see it
}

// code is the status the client saw: a handler that writes nothing answers 200.
func (s *statusRecorder) code() int {
	if s.status == 0 {
		return http.StatusOK
	}
	return s.status
}

// instrumentation names the source of the HTTP traces and metrics.
const instrumentation = "github.com/DrewGGM/aluna/internal/httpapi"

// durationBuckets are the boundaries (in seconds) that the semantic conventions recommend for
// http.server.request.duration.
var durationBuckets = []float64{0.005, 0.01, 0.025, 0.05, 0.075, 0.1, 0.25, 0.5, 0.75, 1, 2.5, 5, 7.5, 10}

// observability measures each request once, also if it ends in a panic that aborts the connection: a
// server span that continues the client's traceparent, the http.server.request.duration histogram and the
// access log line, with the same status and the same duration.
type observability struct {
	log      *slog.Logger
	tracer   trace.Tracer
	duration metric.Float64Histogram
}

func withObservability(log *slog.Logger) func(http.Handler) http.Handler {
	duration, err := otel.Meter(instrumentation).Float64Histogram("http.server.request.duration",
		metric.WithUnit("s"), metric.WithDescription("Duration of HTTP requests."),
		metric.WithExplicitBucketBoundaries(durationBuckets...))
	if err != nil {
		otel.Handle(err)
	}
	o := observability{log: log, tracer: otel.Tracer(instrumentation), duration: duration}
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { o.serve(next, w, r) })
	}
}

func (o observability) serve(next http.Handler, w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	ctx := otel.GetTextMapPropagator().Extract(r.Context(), propagation.HeaderCarrier(r.Header))
	ctx, span := o.tracer.Start(ctx, spanName(methodOf(r), ""), trace.WithSpanKind(trace.SpanKindServer))
	route := new(string)
	r = r.WithContext(context.WithValue(ctx, routeKey{}, route))
	rec := &statusRecorder{ResponseWriter: w}
	defer func() { o.finish(span, r, *route, rec.code(), time.Since(start)) }()
	next.ServeHTTP(rec, r)
}

// finish ends the span, records the duration and writes the access log. route is the mux pattern without
// the method, not the URL, so that ids do not multiply the series; empty if the mux found no route (404,
// 405). The log records the path without the query string, which can carry personal data, and probes go at
// Debug because the orchestrator calls them every few seconds.
func (o observability) finish(span trace.Span, r *http.Request, route string, status int, elapsed time.Duration) {
	method := methodOf(r)
	attrs := []attribute.KeyValue{
		semconv.HTTPRequestMethodKey.String(method), semconv.HTTPResponseStatusCode(status), semconv.URLScheme(schemeOf(r)),
	}
	if route != "" {
		attrs = append(attrs, semconv.HTTPRoute(route))
		span.SetName(spanName(method, route))
	}
	o.duration.Record(r.Context(), elapsed.Seconds(), metric.WithAttributes(attrs...))
	span.SetAttributes(attrs...)
	if status >= http.StatusInternalServerError {
		span.SetStatus(codes.Error, "")
	}
	span.End()

	level := slog.LevelInfo
	if r.URL.Path == pathHealthz || r.URL.Path == pathReadyz {
		level = slog.LevelDebug
	}
	o.log.LogAttrs(r.Context(), level, "request",
		slog.String("method", r.Method),
		slog.String("path", r.URL.Path),
		slog.Int("status", status),
		slog.Float64("duration_ms", float64(elapsed.Microseconds())/1000),
		slog.String("request_id", requestIDFrom(r.Context())),
	)
}

// routeKey stores in the context where withRoute leaves the pattern the mux chose.
type routeKey struct{}

// withRoute goes right before the mux: the mux writes r.Pattern on the request it receives, and the outer
// middlewares may have replaced theirs with r.WithContext.
func withRoute(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if route, ok := r.Context().Value(routeKey{}).(*string); ok {
				*route = routeOf(r.Pattern)
			}
		}()
		next.ServeHTTP(w, r)
	})
}

// methodOther groups the methods that are not in the HTTP standard.
const methodOther = "_OTHER"

// methodOf limits the method to the known ones: any other is _OTHER, so that a client cannot create new
// series with made-up methods.
func methodOf(r *http.Request) string {
	switch r.Method {
	case http.MethodGet, http.MethodHead, http.MethodPost, http.MethodPut, http.MethodPatch,
		http.MethodDelete, http.MethodOptions, http.MethodConnect, http.MethodTrace:
		return r.Method
	default:
		return methodOther
	}
}

// spanName is "{method} {route}"; an unknown method is named HTTP, as the conventions require.
func spanName(method, route string) string {
	if method == methodOther {
		method = "HTTP"
	}
	if route == "" {
		return method
	}
	return method + " " + route
}

func schemeOf(r *http.Request) string {
	if r.TLS != nil {
		return "https"
	}
	return "http"
}

// routeOf strips the method from the mux pattern: "GET /v1/transactions/{id}" is "/v1/transactions/{id}".
func routeOf(pattern string) string {
	if _, route, ok := strings.Cut(pattern, " "); ok {
		return route
	}
	return pattern
}

// withRecovery turns a panic into a 500 problem+json. The stack goes to the log, never to the response.
func withRecovery(log *slog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			rec := &statusRecorder{ResponseWriter: w}
			defer recoverPanic(log, rec, r)
			next.ServeHTTP(rec, r)
		})
	}
}

// recoverPanic is deferred directly so that recover() catches the handler's panic.
func recoverPanic(log *slog.Logger, rec *statusRecorder, r *http.Request) {
	v := recover()
	if v == nil {
		return
	}
	if err, ok := v.(error); ok && errors.Is(err, http.ErrAbortHandler) {
		panic(v) //nolint:forbidigo // net/http uses ErrAbortHandler to cut the connection on purpose
	}
	log.LogAttrs(r.Context(), slog.LevelError, "panic",
		slog.String("value", fmt.Sprint(v)),
		slog.String("stack", string(debug.Stack())),
		slog.String("request_id", requestIDFrom(r.Context())),
	)
	if rec.status != 0 {
		// The headers already went out: the connection is cut so the client does not take a truncated
		// response as complete.
		panic(http.ErrAbortHandler) //nolint:forbidigo // this is how net/http aborts the response in progress
	}
	// The headers the handler left (Set-Cookie, Content-Length, Location) do not apply to the 500.
	header := rec.Header()
	requestID := header.Get(headerRequestID)
	clear(header)
	header.Set(headerRequestID, requestID)
	WriteProblem(rec, r, Problem{Title: "Internal error", Code: "internal_error", Status: http.StatusInternalServerError})
}

// withBodyLimit rejects with 413 a declared body over maxBodyBytes. A body without Content-Length is cut at
// that same limit and the handler receives *http.MaxBytesError, which requestError turns into 413.
func withBodyLimit(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.ContentLength > maxBodyBytes {
			WriteProblem(w, r, Problem{
				Title: "Body too large", Code: "body_too_large", Status: http.StatusRequestEntityTooLarge,
				Detail: fmt.Sprintf("The body exceeds %d bytes.", maxBodyBytes),
			})
			return
		}
		r.Body = http.MaxBytesReader(w, r.Body, maxBodyBytes)
		next.ServeHTTP(w, r)
	})
}

// withProblemForUnrouted turns into problem+json the plain-text 404 and 405 that http.ServeMux answers when
// the path or the method does not exist. Our own handlers already answer problem+json, so they are left
// alone.
func withProblemForUnrouted(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		next.ServeHTTP(&unroutedWriter{ResponseWriter: w, r: r}, r)
	})
}

type unroutedWriter struct {
	http.ResponseWriter
	r        *http.Request
	replaced bool
}

func (u *unroutedWriter) WriteHeader(code int) {
	plain := strings.HasPrefix(u.Header().Get("Content-Type"), "text/plain")
	if !plain || (code != http.StatusNotFound && code != http.StatusMethodNotAllowed) {
		u.ResponseWriter.WriteHeader(code)
		return
	}
	u.replaced = true
	p := Problem{Title: "Route not found", Code: "not_found", Status: code}
	if code == http.StatusMethodNotAllowed {
		p.Title, p.Code = "Method not allowed", "method_not_allowed"
	}
	WriteProblem(u.ResponseWriter, u.r, p)
}

func (u *unroutedWriter) Write(b []byte) (int, error) {
	if u.replaced { // ServeMux's text body is discarded: the problem was already written
		return len(b), nil
	}
	return u.ResponseWriter.Write(b)
}

func (u *unroutedWriter) Unwrap() http.ResponseWriter { return u.ResponseWriter }

// Flush keeps http.Flusher for handlers that write in chunks.
func (u *unroutedWriter) Flush() {
	http.NewResponseController(u.ResponseWriter).Flush() //nolint:errcheck,gosec // Flusher has no way to return the error; the next Write will see it
}
