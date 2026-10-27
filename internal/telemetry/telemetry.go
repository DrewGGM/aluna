// Package telemetry configura las señales del proceso (quality-reliability §Observabilidad): logs JSON
// con el trace_id del request y OpenTelemetry (trazas y métricas) exportado por OTLP cuando hay dónde.
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

// Shutdown vacía y cierra los exportadores; se llama al apagar el proceso.
type Shutdown func(context.Context) error

// Setup deja un logger JSON en out que agrega trace_id y span_id del contexto, y propaga traceparent
// (W3C); los errores internos de OTel (un colector caído) salen por ese logger. Si
// OTEL_EXPORTER_OTLP_ENDPOINT existe, registra los providers globales de trazas y métricas
// con exportadores OTLP/HTTP, configurados por las variables OTEL_* estándar (OTEL_SERVICE_NAME y
// OTEL_RESOURCE_ATTRIBUTES mandan sobre service y version); si no, OTel queda en no-op y nada sale
// del proceso.
func Setup(ctx context.Context, service, version string, out io.Writer) (*slog.Logger, Shutdown, error) {
	log := slog.New(traceHandler{slog.NewJSONHandler(out, nil)})
	otel.SetTextMapPropagator(propagation.TraceContext{})
	otel.SetErrorHandler(otel.ErrorHandlerFunc(func(err error) { log.Error("telemetría", "error", err) }))
	if os.Getenv("OTEL_EXPORTER_OTLP_ENDPOINT") == "" {
		return log, func(context.Context) error { return nil }, nil
	}
	res, err := resource.New(ctx, resource.WithAttributes(semconv.ServiceName(service), semconv.ServiceVersion(version)),
		resource.WithFromEnv(), resource.WithTelemetrySDK())
	if err != nil {
		return nil, nil, fmt.Errorf("recurso de OTel: %w", err)
	}
	traces, err := otlptracehttp.New(ctx)
	if err != nil {
		return nil, nil, fmt.Errorf("exportador de trazas: %w", err)
	}
	metrics, err := otlpmetrichttp.New(ctx)
	if err != nil {
		return nil, nil, errors.Join(fmt.Errorf("exportador de métricas: %w", err), traces.Shutdown(ctx))
	}
	tp := sdktrace.NewTracerProvider(sdktrace.WithBatcher(traces), sdktrace.WithResource(res))
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(sdkmetric.NewPeriodicReader(metrics)), sdkmetric.WithResource(res))
	otel.SetTracerProvider(tp)
	otel.SetMeterProvider(mp)
	return log, func(ctx context.Context) error { return errors.Join(tp.Shutdown(ctx), mp.Shutdown(ctx)) }, nil
}

// traceHandler agrega trace_id y span_id a cada registro que llega con un span en el contexto, para
// unir logs y trazas.
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
