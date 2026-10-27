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

// middleware envuelve mux con la cadena común, del más externo al más interno. Un middleware nuevo se
// agrega en esta lista con la forma func(http.Handler) http.Handler. La auditoría va antes de la
// recuperación, para ver como 500 un comando que hizo panic; sin a, el servidor no tiene comandos que
// auditar (solo los probes).
func middleware(log *slog.Logger, mux *http.ServeMux, a *CommandAudit) http.Handler {
	chain := []func(http.Handler) http.Handler{withRequestID, withObservability(log)}
	if a != nil {
		chain = append(chain, withAudit(a, mux, log))
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

// withRequestID conserva el X-Request-Id del cliente solo si es seguro de escribir en un log; si no,
// genera uno.
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

// statusRecorder recuerda el status de la respuesta; 0 significa que todavía no se escribió nada.
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

// Flush mantiene http.Flusher y marca la respuesta como enviada: tras un flush ya no cabe un 500.
func (s *statusRecorder) Flush() {
	if s.status == 0 {
		s.status = http.StatusOK
	}
	http.NewResponseController(s.ResponseWriter).Flush() //nolint:errcheck,gosec // Flusher no tiene cómo devolver el error; el siguiente Write lo verá
}

// code es el status que vio el cliente: un handler que no escribe nada responde 200.
func (s *statusRecorder) code() int {
	if s.status == 0 {
		return http.StatusOK
	}
	return s.status
}

// instrumentation nombra el origen de las trazas y métricas HTTP.
const instrumentation = "github.com/DrewGGM/aluna/internal/httpapi"

// durationBuckets son los límites (en segundos) que recomiendan las convenciones semánticas para
// http.server.request.duration.
var durationBuckets = []float64{0.005, 0.01, 0.025, 0.05, 0.075, 0.1, 0.25, 0.5, 0.75, 1, 2.5, 5, 7.5, 10}

// observability mide cada request una sola vez, también si termina en un panic que aborta la
// conexión: un span de servidor que continúa el traceparent del cliente, el histograma
// http.server.request.duration y la línea del log de acceso, con el mismo status y la misma duración.
type observability struct {
	log      *slog.Logger
	tracer   trace.Tracer
	duration metric.Float64Histogram
}

func withObservability(log *slog.Logger) func(http.Handler) http.Handler {
	duration, err := otel.Meter(instrumentation).Float64Histogram("http.server.request.duration",
		metric.WithUnit("s"), metric.WithDescription("Duración de los requests HTTP."),
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

// finish cierra el span, registra la duración y escribe el log de acceso. route es el patrón del mux
// sin el método, no la URL, para que los ids no multipliquen las series; vacía si el mux no encontró
// ruta (404, 405). El log registra la ruta sin el query string, que puede traer datos personales, y
// los probes van en Debug porque el orquestador los llama cada pocos segundos.
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

// routeKey guarda en el contexto dónde withRoute deja el patrón que eligió el mux.
type routeKey struct{}

// withRoute va justo antes del mux: el mux escribe r.Pattern en el request que recibe, y los
// middlewares de afuera pueden haber reemplazado el suyo con r.WithContext.
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

// methodOther agrupa los métodos que no están en el estándar HTTP.
const methodOther = "_OTHER"

// methodOf acota el método a los conocidos: cualquier otro es _OTHER, para que un cliente no cree
// series nuevas con métodos inventados.
func methodOf(r *http.Request) string {
	switch r.Method {
	case http.MethodGet, http.MethodHead, http.MethodPost, http.MethodPut, http.MethodPatch,
		http.MethodDelete, http.MethodOptions, http.MethodConnect, http.MethodTrace:
		return r.Method
	default:
		return methodOther
	}
}

// spanName es "{método} {ruta}"; un método desconocido se nombra HTTP, como piden las convenciones.
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

// routeOf quita el método del patrón del mux: "GET /v1/transactions/{id}" es "/v1/transactions/{id}".
func routeOf(pattern string) string {
	if _, route, ok := strings.Cut(pattern, " "); ok {
		return route
	}
	return pattern
}

// withRecovery convierte un panic en un 500 problem+json. El stack va al log, nunca a la respuesta.
func withRecovery(log *slog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			rec := &statusRecorder{ResponseWriter: w}
			defer recoverPanic(log, rec, r)
			next.ServeHTTP(rec, r)
		})
	}
}

// recoverPanic se difiere directamente para que recover() atrape el panic del handler.
func recoverPanic(log *slog.Logger, rec *statusRecorder, r *http.Request) {
	v := recover()
	if v == nil {
		return
	}
	if err, ok := v.(error); ok && errors.Is(err, http.ErrAbortHandler) {
		panic(v) //nolint:forbidigo // net/http usa ErrAbortHandler para cortar la conexión a propósito
	}
	log.LogAttrs(r.Context(), slog.LevelError, "panic",
		slog.String("value", fmt.Sprint(v)),
		slog.String("stack", string(debug.Stack())),
		slog.String("request_id", requestIDFrom(r.Context())),
	)
	if rec.status != 0 {
		// Las cabeceras ya salieron: se corta la conexión para que el cliente no tome como completa
		// una respuesta truncada.
		panic(http.ErrAbortHandler) //nolint:forbidigo // así net/http aborta la respuesta en curso
	}
	// Las cabeceras que dejó el handler (Set-Cookie, Content-Length, Location) no aplican al 500.
	header := rec.Header()
	requestID := header.Get(headerRequestID)
	clear(header)
	header.Set(headerRequestID, requestID)
	WriteProblem(rec, r, Problem{Title: "Error interno", Code: "internal_error", Status: http.StatusInternalServerError})
}

// withBodyLimit rechaza con 413 un cuerpo declarado de más de maxBodyBytes. Un cuerpo sin
// Content-Length se corta en ese mismo límite y el handler recibe *http.MaxBytesError, que
// requestError convierte en 413.
func withBodyLimit(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.ContentLength > maxBodyBytes {
			WriteProblem(w, r, Problem{
				Title: "Cuerpo demasiado grande", Code: "body_too_large", Status: http.StatusRequestEntityTooLarge,
				Detail: fmt.Sprintf("El cuerpo supera %d bytes.", maxBodyBytes),
			})
			return
		}
		r.Body = http.MaxBytesReader(w, r.Body, maxBodyBytes)
		next.ServeHTTP(w, r)
	})
}

// withProblemForUnrouted convierte en problem+json los 404 y 405 en texto plano que responde
// http.ServeMux cuando la ruta o el método no existen. Los handlers propios ya responden problem+json,
// así que no se tocan.
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
	p := Problem{Title: "Ruta no encontrada", Code: "not_found", Status: code}
	if code == http.StatusMethodNotAllowed {
		p.Title, p.Code = "Método no permitido", "method_not_allowed"
	}
	WriteProblem(u.ResponseWriter, u.r, p)
}

func (u *unroutedWriter) Write(b []byte) (int, error) {
	if u.replaced { // el cuerpo de texto de ServeMux se descarta: ya se escribió el problem
		return len(b), nil
	}
	return u.ResponseWriter.Write(b)
}

func (u *unroutedWriter) Unwrap() http.ResponseWriter { return u.ResponseWriter }

// Flush mantiene http.Flusher para los handlers que escriben por partes.
func (u *unroutedWriter) Flush() {
	http.NewResponseController(u.ResponseWriter).Flush() //nolint:errcheck,gosec // Flusher no tiene cómo devolver el error; el siguiente Write lo verá
}
