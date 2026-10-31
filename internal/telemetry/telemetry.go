// Package telemetry configures the process's signals (quality-reliability §Observabilidad): JSON logs
// with the request's trace_id and OpenTelemetry (traces and metrics) exported over OTLP when there is
// somewhere to send them.
package telemetry

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetrichttp"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	"go.opentelemetry.io/otel/propagation"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.43.0"
	"go.opentelemetry.io/otel/trace"
)

// Shutdown flushes and closes the exporters; it is called when the process shuts down.
type Shutdown func(context.Context) error

// Setup sets up a JSON logger on out that adds the context's trace_id and span_id, and propagates
// traceparent (W3C); OTel's internal errors (a collector that is down) go out through that logger. If
// OTEL_EXPORTER_OTLP_ENDPOINT is set, it registers the global trace and metric providers with OTLP/HTTP
// exporters, configured by the standard OTEL_* variables (OTEL_SERVICE_NAME and OTEL_RESOURCE_ATTRIBUTES
// take precedence over service and version); otherwise, OTel stays no-op and nothing leaves the
// process.
func Setup(ctx context.Context, service, version string, out io.Writer) (*slog.Logger, Shutdown, error) {
	log := slog.New(traceHandler{slog.NewJSONHandler(out, nil)})
	otel.SetTextMapPropagator(propagation.TraceContext{})
	otel.SetErrorHandler(otel.ErrorHandlerFunc(func(err error) { log.Error("telemetry", "error", err) }))
	if os.Getenv("OTEL_EXPORTER_OTLP_ENDPOINT") == "" {
		return log, func(context.Context) error { return nil }, nil
	}
	res, err := resource.New(ctx, resource.WithAttributes(semconv.ServiceName(service), semconv.ServiceVersion(version)),
		resource.WithFromEnv(), resource.WithTelemetrySDK())
	if err != nil {
		return nil, nil, fmt.Errorf("otel resource: %w", err)
	}
	traces, err := otlptracehttp.New(ctx)
	if err != nil {
		return nil, nil, fmt.Errorf("trace exporter: %w", err)
	}
	metrics, err := otlpmetrichttp.New(ctx)
	if err != nil {
		return nil, nil, errors.Join(fmt.Errorf("metric exporter: %w", err), traces.Shutdown(ctx))
	}
	tp := sdktrace.NewTracerProvider(sdktrace.WithBatcher(traces), sdktrace.WithResource(res))
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(sdkmetric.NewPeriodicReader(metrics)), sdkmetric.WithResource(res))
	otel.SetTracerProvider(tp)
	otel.SetMeterProvider(mp)
	return log, func(ctx context.Context) error { return errors.Join(tp.Shutdown(ctx), mp.Shutdown(ctx)) }, nil
}

// traceHandler adds trace_id and span_id to every record that arrives with a span in its context, to
// join logs and traces.
type traceHandler struct{ slog.Handler }

func (h traceHandler) Handle(ctx context.Context, r slog.Record) error {
	if sc := trace.SpanContextFromContext(ctx); sc.IsValid() {
		r.AddAttrs(slog.String("trace_id", sc.TraceID().String()), slog.String("span_id", sc.SpanID().String()))
	}
	return h.Handler.Handle(ctx, r)
}

func (h traceHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return traceHandler{h.Handler.WithAttrs(attrs)}
}

func (h traceHandler) WithGroup(name string) slog.Handler {
	return traceHandler{h.Handler.WithGroup(name)}
}
