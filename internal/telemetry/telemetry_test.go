package telemetry

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"go.opentelemetry.io/otel"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/trace"
)

// keepGlobals restores, when the test ends, what Setup changes in OTel's global state.
func keepGlobals(t *testing.T) {
	t.Helper()
	tracer, meter, propagator := otel.GetTracerProvider(), otel.GetMeterProvider(), otel.GetTextMapPropagator()
	handler := otel.GetErrorHandler()
	t.Cleanup(func() {
		otel.SetTracerProvider(tracer)
		otel.SetMeterProvider(meter)
		otel.SetTextMapPropagator(propagator)
		otel.SetErrorHandler(handler)
	})
}

func TestSetupWithoutEndpointLogsJSONWithTrace(t *testing.T) {
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "")
	keepGlobals(t)
	var out bytes.Buffer
	log, shutdown, err := Setup(t.Context(), "aluna", "test", &out)
	if err != nil {
		t.Fatal(err)
	}
	if _, isSDK := otel.GetTracerProvider().(*sdktrace.TracerProvider); isSDK {
		t.Fatal("without OTEL_EXPORTER_OTLP_ENDPOINT, OTel must stay no-op")
	}
	sc := trace.NewSpanContext(trace.SpanContextConfig{
		TraceID: trace.TraceID{1, 2, 3}, SpanID: trace.SpanID{4, 5, 6}, TraceFlags: trace.FlagsSampled,
	})

	log.InfoContext(trace.ContextWithSpanContext(t.Context(), sc), "hello", "k", "v")

	var entry map[string]any
	if err := json.Unmarshal(out.Bytes(), &entry); err != nil {
		t.Fatalf("the log must be one JSON line: %v (%s)", err, out.String())
	}
	if entry["msg"] != "hello" || entry["k"] != "v" || entry["trace_id"] != sc.TraceID().String() || entry["span_id"] != sc.SpanID().String() {
		t.Fatalf("log = %v", entry)
	}
	if err := shutdown(t.Context()); err != nil {
		t.Fatal(err)
	}
}

func TestOTelErrorsGoToTheLog(t *testing.T) {
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "")
	keepGlobals(t)
	var out bytes.Buffer
	if _, _, err := Setup(t.Context(), "aluna", "test", &out); err != nil {
		t.Fatal(err)
	}

	otel.Handle(errors.New("collector down"))

	var entry map[string]any
	if err := json.Unmarshal(out.Bytes(), &entry); err != nil || entry["level"] != "ERROR" || entry["error"] != "collector down" {
		t.Fatalf("the OTel error must come out as a JSON error line: %v (%s)", err, out.String())
	}
}

// TestSetupExportsOverOTLP: with an endpoint, traces and metrics go out over OTLP/HTTP on shutdown.
func TestSetupExportsOverOTLP(t *testing.T) {
	var mu sync.Mutex
	received := map[string]bool{}
	collector := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		received[r.URL.Path] = true
	}))
	t.Cleanup(collector.Close)
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", collector.URL)
	keepGlobals(t)

	_, shutdown, err := Setup(t.Context(), "aluna", "test", &bytes.Buffer{})
	if err != nil {
		t.Fatal(err)
	}
	_, span := otel.Tracer("test").Start(t.Context(), "operation")
	span.End()
	counter, err := otel.Meter("test").Int64Counter("test.counter")
	if err != nil {
		t.Fatal(err)
	}
	counter.Add(t.Context(), 1)
	if err := shutdown(t.Context()); err != nil {
		t.Fatal(err)
	}

	mu.Lock()
	defer mu.Unlock()
	if !received["/v1/traces"] || !received["/v1/metrics"] {
		t.Fatalf("the collector received %v, want /v1/traces and /v1/metrics", received)
	}
}
