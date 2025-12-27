package telemetry

import (
	"context"
	"fmt"
	"log/slog"
	"os"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.24.0"
)

var (
	tracerProvider *sdktrace.TracerProvider
)

// InitTracer initializes the OpenTelemetry tracer
func InitTracer(ctx context.Context, serviceName string) (func(context.Context) error, error) {
	// Check if tracing is enabled via env var endpoint or similar?
	// The user asked for Tracing, so we enable it if OTLP endpoint is present or just default?
	// Usually we check for OTEL_EXPORTER_OTLP_ENDPOINT.
	// If not set, maybe we shouldn't fail hard, or we default to localhost:4318.
	
	// We'll use the standard env vars for configuration (OTEL_EXPORTER_OTLP_ENDPOINT etc).
	// But to be safe, if no endpoint is explicitly defined, maybe we shouldn't block startup?
	// However, otlptracehttp defaults to localhost:4318.

	if os.Getenv("OTEL_EXPORTER_OTLP_ENDPOINT") == "" {
		slog.Info("OTEL_EXPORTER_OTLP_ENDPOINT not set. Tracing might not report to a collector.")
	}

	exporter, err := otlptracehttp.New(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to create trace exporter: %w", err)
	}

	res, err := resource.New(ctx,
		resource.WithAttributes(
			semconv.ServiceNameKey.String(serviceName),
		),
	)
	if err != nil {
		return nil, fmt.Errorf("failed to create resource: %w", err)
	}

	tracerProvider = sdktrace.NewTracerProvider(
		sdktrace.WithBatcher(exporter),
		sdktrace.WithResource(res),
	)

	otel.SetTracerProvider(tracerProvider)
	otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(propagation.TraceContext{}, propagation.Baggage{}))

	return tracerProvider.Shutdown, nil
}
