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
		t.Fatalf("Content-Type = %q, want application/problem+json", got)
	}
	var body map[string]any
	if err := json.NewDecoder(rec.Body).Decode(&body); err != nil {
		t.Fatalf("invalid problem+json body: %v", err)
	}
	return body
}

func TestHealthz(t *testing.T) {
	srv, _ := newTestServer(t, ServerConfig{}, nil)

	rec := serve(srv, httptest.NewRequest(http.MethodGet, "/healthz", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if got := strings.TrimSpace(rec.Body.String()); got != `{"status":"ok"}` {
		t.Fatalf("body = %s", got)
	}
}

func TestReadyz(t *testing.T) {
	failing := func(context.Context) error { return errors.New("database down") }
	cases := map[string]struct {
		ready func(context.Context) error
		want  int
	}{
		"no checker":              {nil, http.StatusOK},
		"checker answers":         {func(context.Context) error { return nil }, http.StatusOK},
		"checker does not answer": {failing, http.StatusServiceUnavailable},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			srv, _ := newTestServer(t, ServerConfig{Ready: tc.ready}, nil)

			rec := serve(srv, httptest.NewRequest(http.MethodGet, "/readyz", nil))

			if rec.Code != tc.want {
				t.Fatalf("status = %d, want %d", rec.Code, tc.want)
			}
			if tc.want == http.StatusServiceUnavailable {
				body := decodeProblem(t, rec)
				if body["code"] != "not_ready" || strings.Contains(rec.Body.String(), "database down") {
					t.Fatalf("problem = %v: must have code not_ready and not leak the internal error", body)
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
		"missing, generated":   {"", false},
		"valid, kept":          {"abc-123_X.y", true},
		"128 characters, kept": {strings.Repeat("a", 128), true},
		"with a line break":    {"abc\ninjected", false},
		"too long":             {strings.Repeat("a", 129), false},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			var seen string
			srv, _ := newTestServer(t, ServerConfig{}, func(mux *http.ServeMux) {
				mux.HandleFunc("GET /echo", func(_ http.ResponseWriter, r *http.Request) {
					seen = requestIDFrom(r.Context())
				})
			})
			req := httptest.NewRequest(http.MethodGet, "/echo", nil)
			if tc.incoming != "" {
				req.Header.Set(headerRequestID, tc.incoming)
			}

			rec := serve(srv, req)

			got := rec.Header().Get(headerRequestID)
			if got == "" || got != seen {
				t.Fatalf("X-Request-Id = %q, context = %q: they must match and not be empty", got, seen)
			}
			if kept := got == tc.incoming; kept != tc.keep {
				t.Fatalf("X-Request-Id = %q with input %q: kept = %v, want %v", got, tc.incoming, kept, tc.keep)
			}
		})
	}
}

func TestPanicBecomesProblemWithoutStack(t *testing.T) {
	srv, logs := newTestServer(t, ServerConfig{}, func(mux *http.ServeMux) {
		mux.HandleFunc("GET /fail", func(http.ResponseWriter, *http.Request) {
			panic("internal secret")
		})
	})

	rec := serve(srv, httptest.NewRequest(http.MethodGet, "/fail", nil))

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", rec.Code)
	}
	body := decodeProblem(t, rec)
	if body["code"] != "internal_error" || strings.Contains(rec.Body.String(), "secret") ||
		strings.Contains(rec.Body.String(), "goroutine") {
		t.Fatalf("problem = %v: must be internal_error without the message or the stack", body)
	}
	if !strings.Contains(logs.String(), "internal secret") {
		t.Fatalf("the panic must be in the log: %s", logs.String())
	}
}

func TestAccessLog(t *testing.T) {
	srv, logs := newTestServer(t, ServerConfig{}, func(mux *http.ServeMux) {
		mux.HandleFunc("GET /resource/{id}", func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusTeapot)
		})
	})

	rec := serve(srv, httptest.NewRequest(http.MethodGet, "/resource/7?name=private", nil))

	var entry map[string]any
	if err := json.Unmarshal(logs.Bytes(), &entry); err != nil {
		t.Fatalf("one JSON line per request: %v (%s)", err, logs.String())
	}
	want := map[string]any{
		"msg": "request", "method": "GET", "path": "/resource/7", "status": float64(http.StatusTeapot),
		"request_id": rec.Header().Get(headerRequestID),
	}
	for key, value := range want {
		if entry[key] != value {
			t.Errorf("log[%q] = %v, want %v", key, entry[key], value)
		}
	}
	if _, ok := entry["duration_ms"].(float64); !ok {
		t.Errorf("log without duration_ms: %v", entry)
	}
	if strings.Contains(logs.String(), "private") {
		t.Errorf("the log must not include the query string: %s", logs.String())
	}
}

func TestBodyLimit(t *testing.T) {
	var readErr error
	srv, _ := newTestServer(t, ServerConfig{}, func(mux *http.ServeMux) {
		mux.HandleFunc("POST /upload", func(_ http.ResponseWriter, r *http.Request) {
			_, readErr = io.Copy(io.Discard, r.Body)
		})
	})
	tooBig := bytes.Repeat([]byte("x"), maxBodyBytes+1)

	t.Run("exactly 1 MiB passes", func(t *testing.T) {
		rec := serve(srv, httptest.NewRequest(http.MethodPost, "/upload", bytes.NewReader(tooBig[:maxBodyBytes])))

		if rec.Code != http.StatusOK || readErr != nil {
			t.Fatalf("status = %d, read error = %v", rec.Code, readErr)
		}
	})
	t.Run("declared Content-Length", func(t *testing.T) {
		rec := serve(srv, httptest.NewRequest(http.MethodPost, "/upload", bytes.NewReader(tooBig)))

		if rec.Code != http.StatusRequestEntityTooLarge {
			t.Fatalf("status = %d, want 413", rec.Code)
		}
		if decodeProblem(t, rec)["code"] != "body_too_large" {
			t.Fatal("want code body_too_large")
		}
	})
	t.Run("without Content-Length", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodPost, "/upload", io.NopCloser(bytes.NewReader(tooBig)))
		req.ContentLength = -1

		serve(srv, req)

		var maxErr *http.MaxBytesError
		if !errors.As(readErr, &maxErr) {
			t.Fatalf("reading more than 1 MiB must fail with MaxBytesError, got %v", readErr)
		}
	})
}

func TestServerTimeouts(t *testing.T) {
	srv, _ := newTestServer(t, ServerConfig{}, nil)

	got := [4]time.Duration{srv.ReadHeaderTimeout, srv.ReadTimeout, srv.WriteTimeout, srv.IdleTimeout}
	if want := [4]time.Duration{5 * time.Second, 10 * time.Second, 15 * time.Second, 60 * time.Second}; got != want {
		t.Errorf("timeouts (ReadHeader, Read, Write, Idle) = %v, want %v", got, want)
	}
}

func TestPanicDropsHandlerHeaders(t *testing.T) {
	srv, _ := newTestServer(t, ServerConfig{}, func(mux *http.ServeMux) {
		mux.HandleFunc("GET /fail", func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Set-Cookie", "session=1")
			w.Header().Set("Content-Length", "5")
			panic("before writing")
		})
	})

	rec := serve(srv, httptest.NewRequest(http.MethodGet, "/fail", nil))

	if rec.Header().Get("Set-Cookie") != "" || rec.Header().Get("Content-Length") != "" {
		t.Fatalf("handler headers in the 500: %v", rec.Header())
	}
	if rec.Header().Get(headerRequestID) == "" {
		t.Fatal("the 500 must keep X-Request-Id")
	}
	decodeProblem(t, rec)
}

// TestPanicAfterWriteAbortsConnection checks that a panic with the response already started cuts the
// connection: the client cannot take a truncated 200 as complete.
func TestPanicAfterWriteAbortsConnection(t *testing.T) {
	srv, logs := newTestServer(t, ServerConfig{}, func(mux *http.ServeMux) {
		mux.HandleFunc("GET /partial", func(w http.ResponseWriter, _ *http.Request) {
			if _, err := w.Write([]byte(`{"balance":`)); err != nil {
				t.Error(err)
			}
			w.(http.Flusher).Flush() //nolint:forcetypeassert // statusRecorder implements Flusher; if not, the test must break
			panic("halfway through the response")
		})
	})
	ts := httptest.NewServer(srv.Handler)
	t.Cleanup(ts.Close)

	resp, err := http.Get(ts.URL + "/partial")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	_, readErr := io.ReadAll(resp.Body)
	if err := resp.Body.Close(); err != nil {
		t.Fatal(err)
	}
	if readErr == nil {
		t.Fatal("reading the truncated response should have failed")
	}
	ts.Close() // waits for the handler to finish writing its logs
	if !strings.Contains(logs.String(), "halfway through the response") {
		t.Fatalf("the panic must be in the log: %s", logs.String())
	}
}

// TestHandlerAbortPassesThrough checks that a handler that aborts on purpose (http.ErrAbortHandler) cuts
// the connection instead of becoming a 500.
func TestHandlerAbortPassesThrough(t *testing.T) {
	srv, _ := newTestServer(t, ServerConfig{}, func(mux *http.ServeMux) {
		mux.HandleFunc("GET /abort", func(http.ResponseWriter, *http.Request) {
			panic(http.ErrAbortHandler)
		})
	})
	ts := httptest.NewServer(srv.Handler)
	t.Cleanup(ts.Close)

	resp, err := http.Get(ts.URL + "/abort")
	if err == nil {
		resp.Body.Close() //nolint:errcheck,gosec // the test already fails on the status
		t.Fatalf("want a connection error, got status %d", resp.StatusCode)
	}
}

func TestUnroutedRequestsGetProblems(t *testing.T) {
	srv, _ := newTestServer(t, ServerConfig{}, func(mux *http.ServeMux) {
		mux.HandleFunc("GET /v1/things", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	})
	cases := map[string]struct {
		method, path string
		status       int
		code         string
	}{
		"nonexistent route":  {http.MethodGet, "/v1/nothing", http.StatusNotFound, "not_found"},
		"method not allowed": {http.MethodDelete, "/v1/things", http.StatusMethodNotAllowed, "method_not_allowed"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			rec := serve(srv, httptest.NewRequest(tc.method, tc.path, nil))

			if rec.Code != tc.status || decodeProblem(t, rec)["code"] != tc.code {
				t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
			}
			if tc.status == http.StatusMethodNotAllowed && rec.Header().Get("Allow") == "" {
				t.Fatal("the 405 must keep the Allow header")
			}
		})
	}
}

// recordTelemetry keeps traces and metrics in memory during the test.
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

// durationPoints returns the points of http.server.request.duration.
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
	t.Fatalf("http.server.request.duration in seconds was not recorded: %+v", rm)
	return nil
}

func TestTelemetryPerRequest(t *testing.T) {
	spans, reader := recordTelemetry(t)
	srv, _ := newTestServer(t, ServerConfig{}, func(mux *http.ServeMux) {
		mux.HandleFunc("GET /resource/{id}", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusTeapot) })
		mux.HandleFunc("GET /fail", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusBadGateway) })
	})
	serve(srv, httptest.NewRequest(http.MethodGet, "/resource/7", nil))
	serve(srv, httptest.NewRequest(http.MethodGet, "/resource/8", nil))
	serve(srv, httptest.NewRequest(http.MethodGet, "/fail", nil))
	serve(srv, httptest.NewRequest("MADEUP", "/nothing", nil))

	ended := spans.Ended()
	names := make([]string, len(ended))
	for i, s := range ended {
		names[i] = s.Name()
	}
	if want := []string{"GET /resource/{id}", "GET /resource/{id}", "GET /fail", "HTTP"}; !slices.Equal(names, want) {
		t.Errorf("span names = %q, want %q", names, want)
	}
	if ended[0].SpanKind() != trace.SpanKindServer || ended[2].Status().Code != codes.Error || ended[0].Status().Code == codes.Error {
		t.Errorf("span kind or status: %v %v %v", ended[0].SpanKind(), ended[0].Status(), ended[2].Status())
	}
	got := map[string]uint64{}
	for _, p := range durationPoints(t, reader) {
		route, _ := p.Attributes.Value(semconv.HTTPRouteKey)
		method, _ := p.Attributes.Value(semconv.HTTPRequestMethodKey)
		status, _ := p.Attributes.Value(semconv.HTTPResponseStatusCodeKey)
		got[fmt.Sprintf("%s %s %d", method.AsString(), route.AsString(), status.AsInt64())] = p.Count
	}
	want := map[string]uint64{"GET /resource/{id} 418": 2, "GET /fail 502": 1, "_OTHER  404": 1}
	if !maps.Equal(got, want) {
		t.Errorf("histogram series = %v, want %v", got, want)
	}
}

// TestTraceContinuesAndReachesLogsAndProblems: the span continues the client's traceparent and its trace_id
// reaches the access log and the problem.
func TestTraceContinuesAndReachesLogsAndProblems(t *testing.T) {
	recordTelemetry(t)
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "") // Setup only builds the logger: it does not touch the test providers
	var logs bytes.Buffer
	log, _, err := telemetry.Setup(t.Context(), "aluna", "test", &logs)
	if err != nil {
		t.Fatal(err)
	}
	srv := NewServer(ServerConfig{}, nil, log)
	const traceID = "4bf92f3577b34da6a3ce929d0e0e4736"
	req := httptest.NewRequest(http.MethodGet, "/does-not-exist", nil)
	req.Header.Set("traceparent", "00-"+traceID+"-00f067aa0ba902b7-01")

	rec := serve(srv, req)

	if p := decodeProblem(t, rec); p["trace_id"] != traceID {
		t.Errorf("problem trace_id = %v, want %s", p["trace_id"], traceID)
	}
	if !strings.Contains(logs.String(), `"trace_id":"`+traceID+`"`) {
		t.Errorf("the access log does not carry the trace_id: %s", logs.String())
	}
}

func TestProbesLogAtDebug(t *testing.T) {
	var logs bytes.Buffer
	srv := NewServer(ServerConfig{}, nil, slog.New(slog.NewJSONHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug})))

	serve(srv, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	serve(srv, httptest.NewRequest(http.MethodGet, "/readyz", nil))

	if got := strings.Count(logs.String(), `"level":"DEBUG","msg":"request"`); got != 2 || strings.Contains(logs.String(), `"INFO"`) {
		t.Errorf("both probes must log at Debug and nothing at Info: %s", logs.String())
	}
}
