package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/propagation"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	semconv "go.opentelemetry.io/otel/semconv/v1.43.0"
	"go.opentelemetry.io/otel/trace"

	"github.com/DrewGGM/aluna/internal/telemetry"
)

func newTestServer(t *testing.T, cfg ServerConfig, routes func(*http.ServeMux)) (*http.Server, *bytes.Buffer) {
	t.Helper()
	var logs bytes.Buffer
	return NewServer(cfg, routes, slog.New(slog.NewJSONHandler(&logs, nil))), &logs
}

func serve(srv *http.Server, req *http.Request) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	srv.Handler.ServeHTTP(rec, req)
	return rec
}

func decodeProblem(t *testing.T, rec *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	if got := rec.Header().Get("Content-Type"); got != "application/problem+json" {
		t.Fatalf("Content-Type = %q, se esperaba application/problem+json", got)
	}
	var body map[string]any
	if err := json.NewDecoder(rec.Body).Decode(&body); err != nil {
		t.Fatalf("cuerpo problem+json inválido: %v", err)
	}
	return body
}

func TestHealthz(t *testing.T) {
	srv, _ := newTestServer(t, ServerConfig{}, nil)

	rec := serve(srv, httptest.NewRequest(http.MethodGet, "/healthz", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, se esperaba 200", rec.Code)
	}
	if got := strings.TrimSpace(rec.Body.String()); got != `{"status":"ok"}` {
		t.Fatalf("cuerpo = %s", got)
	}
}

func TestReadyz(t *testing.T) {
	failing := func(context.Context) error { return errors.New("base caída") }
	cases := map[string]struct {
		ready func(context.Context) error
		want  int
	}{
		"sin checker":         {nil, http.StatusOK},
		"checker responde":    {func(context.Context) error { return nil }, http.StatusOK},
		"checker no responde": {failing, http.StatusServiceUnavailable},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			srv, _ := newTestServer(t, ServerConfig{Ready: tc.ready}, nil)

			rec := serve(srv, httptest.NewRequest(http.MethodGet, "/readyz", nil))

			if rec.Code != tc.want {
				t.Fatalf("status = %d, se esperaba %d", rec.Code, tc.want)
			}
			if tc.want == http.StatusServiceUnavailable {
				body := decodeProblem(t, rec)
				if body["code"] != "not_ready" || strings.Contains(rec.Body.String(), "base caída") {
					t.Fatalf("problem = %v: debe tener code not_ready y no filtrar el error interno", body)
				}
			}
		})
	}
}

func TestRequestID(t *testing.T) {
	cases := map[string]struct {
		incoming string
		keep     bool
	}{
		"falta, se genera":            {"", false},
		"válido, se conserva":         {"abc-123_X.y", true},
		"128 caracteres, se conserva": {strings.Repeat("a", 128), true},
		"con salto de línea":          {"abc\ninyectado", false},
		"demasiado largo":             {strings.Repeat("a", 129), false},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			var seen string
			srv, _ := newTestServer(t, ServerConfig{}, func(mux *http.ServeMux) {
				mux.HandleFunc("GET /eco", func(_ http.ResponseWriter, r *http.Request) {
					seen = requestIDFrom(r.Context())
				})
			})
			req := httptest.NewRequest(http.MethodGet, "/eco", nil)
			if tc.incoming != "" {
				req.Header.Set(headerRequestID, tc.incoming)
			}

			rec := serve(srv, req)

			got := rec.Header().Get(headerRequestID)
			if got == "" || got != seen {
				t.Fatalf("X-Request-Id = %q, contexto = %q: deben coincidir y no estar vacíos", got, seen)
			}
			if kept := got == tc.incoming; kept != tc.keep {
				t.Fatalf("X-Request-Id = %q con entrada %q: conservado = %v, se esperaba %v", got, tc.incoming, kept, tc.keep)
			}
		})
	}
}

func TestPanicBecomesProblemWithoutStack(t *testing.T) {
	srv, logs := newTestServer(t, ServerConfig{}, func(mux *http.ServeMux) {
		mux.HandleFunc("GET /falla", func(http.ResponseWriter, *http.Request) {
			panic("secreto interno")
		})
	})

	rec := serve(srv, httptest.NewRequest(http.MethodGet, "/falla", nil))

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, se esperaba 500", rec.Code)
	}
	body := decodeProblem(t, rec)
	if body["code"] != "internal_error" || strings.Contains(rec.Body.String(), "secreto") ||
		strings.Contains(rec.Body.String(), "goroutine") {
		t.Fatalf("problem = %v: debe ser internal_error sin el mensaje ni el stack", body)
	}
	if !strings.Contains(logs.String(), "secreto interno") {
		t.Fatalf("el panic debe quedar en el log: %s", logs.String())
	}
}

func TestAccessLog(t *testing.T) {
	srv, logs := newTestServer(t, ServerConfig{}, func(mux *http.ServeMux) {
		mux.HandleFunc("GET /recurso/{id}", func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusTeapot)
		})
	})

	rec := serve(srv, httptest.NewRequest(http.MethodGet, "/recurso/7?nombre=privado", nil))

	var entry map[string]any
	if err := json.Unmarshal(logs.Bytes(), &entry); err != nil {
		t.Fatalf("una línea JSON por request: %v (%s)", err, logs.String())
	}
	want := map[string]any{
		"msg": "request", "method": "GET", "path": "/recurso/7", "status": float64(http.StatusTeapot),
		"request_id": rec.Header().Get(headerRequestID),
	}
	for key, value := range want {
		if entry[key] != value {
			t.Errorf("log[%q] = %v, se esperaba %v", key, entry[key], value)
		}
	}
	if _, ok := entry["duration_ms"].(float64); !ok {
		t.Errorf("log sin duration_ms: %v", entry)
	}
	if strings.Contains(logs.String(), "privado") {
		t.Errorf("el log no debe incluir el query string: %s", logs.String())
	}
}

func TestBodyLimit(t *testing.T) {
	var readErr error
	srv, _ := newTestServer(t, ServerConfig{}, func(mux *http.ServeMux) {
		mux.HandleFunc("POST /carga", func(_ http.ResponseWriter, r *http.Request) {
			_, readErr = io.Copy(io.Discard, r.Body)
		})
	})
	tooBig := bytes.Repeat([]byte("x"), maxBodyBytes+1)

	t.Run("exactamente 1 MiB pasa", func(t *testing.T) {
		rec := serve(srv, httptest.NewRequest(http.MethodPost, "/carga", bytes.NewReader(tooBig[:maxBodyBytes])))

		if rec.Code != http.StatusOK || readErr != nil {
			t.Fatalf("status = %d, error de lectura = %v", rec.Code, readErr)
		}
	})
	t.Run("Content-Length declarado", func(t *testing.T) {
		rec := serve(srv, httptest.NewRequest(http.MethodPost, "/carga", bytes.NewReader(tooBig)))

		if rec.Code != http.StatusRequestEntityTooLarge {
			t.Fatalf("status = %d, se esperaba 413", rec.Code)
		}
		if decodeProblem(t, rec)["code"] != "body_too_large" {
			t.Fatal("se esperaba code body_too_large")
		}
	})
	t.Run("sin Content-Length", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodPost, "/carga", io.NopCloser(bytes.NewReader(tooBig)))
		req.ContentLength = -1

		serve(srv, req)

		var maxErr *http.MaxBytesError
		if !errors.As(readErr, &maxErr) {
			t.Fatalf("leer más de 1 MiB debe fallar con MaxBytesError, fue %v", readErr)
		}
	})
}

func TestServerTimeouts(t *testing.T) {
	srv, _ := newTestServer(t, ServerConfig{}, nil)

	got := [4]time.Duration{srv.ReadHeaderTimeout, srv.ReadTimeout, srv.WriteTimeout, srv.IdleTimeout}
	if want := [4]time.Duration{5 * time.Second, 10 * time.Second, 15 * time.Second, 60 * time.Second}; got != want {
		t.Errorf("timeouts (ReadHeader, Read, Write, Idle) = %v, se esperaba %v", got, want)
	}
}

func TestPanicDropsHandlerHeaders(t *testing.T) {
	srv, _ := newTestServer(t, ServerConfig{}, func(mux *http.ServeMux) {
		mux.HandleFunc("GET /falla", func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Set-Cookie", "sesion=1")
			w.Header().Set("Content-Length", "5")
			panic("antes de escribir")
		})
	})

	rec := serve(srv, httptest.NewRequest(http.MethodGet, "/falla", nil))

	if rec.Header().Get("Set-Cookie") != "" || rec.Header().Get("Content-Length") != "" {
		t.Fatalf("cabeceras del handler en el 500: %v", rec.Header())
	}
	if rec.Header().Get(headerRequestID) == "" {
		t.Fatal("el 500 debe conservar X-Request-Id")
	}
	decodeProblem(t, rec)
}

// TestPanicAfterWriteAbortsConnection prueba que un panic con la respuesta ya empezada corta la
// conexión: el cliente no puede tomar un 200 truncado como completo.
func TestPanicAfterWriteAbortsConnection(t *testing.T) {
	srv, logs := newTestServer(t, ServerConfig{}, func(mux *http.ServeMux) {
		mux.HandleFunc("GET /parcial", func(w http.ResponseWriter, _ *http.Request) {
			if _, err := w.Write([]byte(`{"balance":`)); err != nil {
				t.Error(err)
			}
			w.(http.Flusher).Flush() //nolint:forcetypeassert // statusRecorder implementa Flusher; si no, la prueba debe romperse
			panic("a mitad de la respuesta")
		})
	})
	ts := httptest.NewServer(srv.Handler)
	t.Cleanup(ts.Close)

	resp, err := http.Get(ts.URL + "/parcial")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	_, readErr := io.ReadAll(resp.Body)
	if err := resp.Body.Close(); err != nil {
		t.Fatal(err)
	}
	if readErr == nil {
		t.Fatal("leer la respuesta truncada debió fallar")
	}
	ts.Close() // espera a que el handler termine de escribir sus logs
	if !strings.Contains(logs.String(), "a mitad de la respuesta") {
		t.Fatalf("el panic debe quedar en el log: %s", logs.String())
	}
}

// TestHandlerAbortPassesThrough prueba que un handler que aborta a propósito (http.ErrAbortHandler)
// corta la conexión en vez de convertirse en un 500.
func TestHandlerAbortPassesThrough(t *testing.T) {
	srv, _ := newTestServer(t, ServerConfig{}, func(mux *http.ServeMux) {
		mux.HandleFunc("GET /aborta", func(http.ResponseWriter, *http.Request) {
			panic(http.ErrAbortHandler)
		})
	})
	ts := httptest.NewServer(srv.Handler)
	t.Cleanup(ts.Close)

	resp, err := http.Get(ts.URL + "/aborta")
	if err == nil {
		resp.Body.Close() //nolint:errcheck,gosec // la prueba ya falla por el status
		t.Fatalf("se esperaba un error de conexión, llegó status %d", resp.StatusCode)
	}
}

func TestUnroutedRequestsGetProblems(t *testing.T) {
	srv, _ := newTestServer(t, ServerConfig{}, func(mux *http.ServeMux) {
		mux.HandleFunc("GET /v1/cosas", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	})
	cases := map[string]struct {
		method, path string
		status       int
		code         string
	}{
		"ruta inexistente":    {http.MethodGet, "/v1/nada", http.StatusNotFound, "not_found"},
		"método no permitido": {http.MethodDelete, "/v1/cosas", http.StatusMethodNotAllowed, "method_not_allowed"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			rec := serve(srv, httptest.NewRequest(tc.method, tc.path, nil))

			if rec.Code != tc.status || decodeProblem(t, rec)["code"] != tc.code {
				t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
			}
			if tc.status == http.StatusMethodNotAllowed && rec.Header().Get("Allow") == "" {
				t.Fatal("el 405 debe conservar el encabezado Allow")
			}
		})
	}
}

// recordTelemetry deja trazas y métricas en memoria durante la prueba.
func recordTelemetry(t *testing.T) (*tracetest.SpanRecorder, *sdkmetric.ManualReader) {
	t.Helper()
	spans, reader := tracetest.NewSpanRecorder(), sdkmetric.NewManualReader()
	tracer, meter, propagator, handler := otel.GetTracerProvider(), otel.GetMeterProvider(), otel.GetTextMapPropagator(), otel.GetErrorHandler()
	otel.SetTracerProvider(sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(spans)))
	otel.SetMeterProvider(sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader)))
	otel.SetTextMapPropagator(propagation.TraceContext{})
	t.Cleanup(func() {
		otel.SetTracerProvider(tracer)
		otel.SetMeterProvider(meter)
		otel.SetTextMapPropagator(propagator)
		otel.SetErrorHandler(handler)
	})
	return spans, reader
}

// durationPoints devuelve los puntos de http.server.request.duration.
func durationPoints(t *testing.T, reader *sdkmetric.ManualReader) []metricdata.HistogramDataPoint[float64] {
	t.Helper()
	var rm metricdata.ResourceMetrics
	if err := reader.Collect(t.Context(), &rm); err != nil {
		t.Fatal(err)
	}
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			if h, ok := m.Data.(metricdata.Histogram[float64]); ok && m.Name == "http.server.request.duration" && m.Unit == "s" {
				return h.DataPoints
			}
		}
	}
	t.Fatalf("no se registró http.server.request.duration en segundos: %+v", rm)
	return nil
}

func TestTelemetryPerRequest(t *testing.T) {
	spans, reader := recordTelemetry(t)
	srv, _ := newTestServer(t, ServerConfig{}, func(mux *http.ServeMux) {
		mux.HandleFunc("GET /recurso/{id}", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusTeapot) })
		mux.HandleFunc("GET /falla", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusBadGateway) })
	})
	serve(srv, httptest.NewRequest(http.MethodGet, "/recurso/7", nil))
	serve(srv, httptest.NewRequest(http.MethodGet, "/recurso/8", nil))
	serve(srv, httptest.NewRequest(http.MethodGet, "/falla", nil))
	serve(srv, httptest.NewRequest("INVENTADO", "/nada", nil))

	ended := spans.Ended()
	names := make([]string, len(ended))
	for i, s := range ended {
		names[i] = s.Name()
	}
	if want := []string{"GET /recurso/{id}", "GET /recurso/{id}", "GET /falla", "HTTP"}; !slices.Equal(names, want) {
		t.Errorf("nombres de los spans = %q, se esperaba %q", names, want)
	}
	if ended[0].SpanKind() != trace.SpanKindServer || ended[2].Status().Code != codes.Error || ended[0].Status().Code == codes.Error {
		t.Errorf("tipo o estado de los spans: %v %v %v", ended[0].SpanKind(), ended[0].Status(), ended[2].Status())
	}
	got := map[string]uint64{}
	for _, p := range durationPoints(t, reader) {
		route, _ := p.Attributes.Value(semconv.HTTPRouteKey)
		method, _ := p.Attributes.Value(semconv.HTTPRequestMethodKey)
		status, _ := p.Attributes.Value(semconv.HTTPResponseStatusCodeKey)
		got[fmt.Sprintf("%s %s %d", method.AsString(), route.AsString(), status.AsInt64())] = p.Count
	}
	want := map[string]uint64{"GET /recurso/{id} 418": 2, "GET /falla 502": 1, "_OTHER  404": 1}
	if !maps.Equal(got, want) {
		t.Errorf("series del histograma = %v, se esperaba %v", got, want)
	}
}

// TestTraceContinuesAndReachesLogsAndProblems: el span continúa el traceparent del cliente y su
// trace_id llega al log de acceso y al problem.
func TestTraceContinuesAndReachesLogsAndProblems(t *testing.T) {
	recordTelemetry(t)
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "") // Setup solo arma el logger: no toca los providers de la prueba
	var logs bytes.Buffer
	log, _, err := telemetry.Setup(t.Context(), "aluna", "test", &logs)
	if err != nil {
		t.Fatal(err)
	}
	srv := NewServer(ServerConfig{}, nil, log)
	const traceID = "4bf92f3577b34da6a3ce929d0e0e4736"
	req := httptest.NewRequest(http.MethodGet, "/no-existe", nil)
	req.Header.Set("traceparent", "00-"+traceID+"-00f067aa0ba902b7-01")

	rec := serve(srv, req)

	if p := decodeProblem(t, rec); p["trace_id"] != traceID {
		t.Errorf("trace_id del problem = %v, se esperaba %s", p["trace_id"], traceID)
	}
	if !strings.Contains(logs.String(), `"trace_id":"`+traceID+`"`) {
		t.Errorf("el log de acceso no lleva el trace_id: %s", logs.String())
	}
}

func TestProbesLogAtDebug(t *testing.T) {
	var logs bytes.Buffer
	srv := NewServer(ServerConfig{}, nil, slog.New(slog.NewJSONHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug})))

	serve(srv, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	serve(srv, httptest.NewRequest(http.MethodGet, "/readyz", nil))

	if got := strings.Count(logs.String(), `"level":"DEBUG","msg":"request"`); got != 2 || strings.Contains(logs.String(), `"INFO"`) {
		t.Errorf("los dos probes deben salir en Debug y nada en Info: %s", logs.String())
	}
}
