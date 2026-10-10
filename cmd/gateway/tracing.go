package main

import (
	"context"
	"fmt"
	"log/slog"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
)

// serviceName is the default service.name of the gateway's spans;
// OTEL_SERVICE_NAME overrides it.
const serviceName = "llm-gateway"

// newTracerProvider returns a tracer provider that batches spans and
// exports them with OTLP over HTTP. The exporter, sampler, and resource
// read their settings from the standard OTEL_* variables. Export errors
// are logged to logger. Shutting the provider down flushes pending spans.
func newTracerProvider(ctx context.Context, logger *slog.Logger) (*sdktrace.TracerProvider, error) {
	exporter, err := otlptracehttp.New(ctx)
	if err != nil {
		return nil, fmt.Errorf("tracing: %w", err)
	}
	res, err := resource.New(ctx,
		resource.WithAttributes(attribute.String("service.name", serviceName)),
		resource.WithFromEnv(),
		resource.WithTelemetrySDK(),
	)
	if err != nil {
		return nil, fmt.Errorf("tracing: %w", err)
	}
	// Export runs in the background, so its errors reach only OTel's
	// global error handler, which otherwise writes unstructured lines. They
	// can include the endpoint URL and the collector's response, never the
	// exported spans.
	otel.SetErrorHandler(otel.ErrorHandlerFunc(func(err error) {
		logger.Warn("tracing error", slog.Any("error", err))
	}))
	return sdktrace.NewTracerProvider(sdktrace.WithBatcher(exporter), sdktrace.WithResource(res)), nil
}
