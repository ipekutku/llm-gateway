package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/trace"
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
		return nil, fmt.Errorf("tracing exporter: %w", tracingError{err})
	}
	res, err := resource.New(ctx,
		resource.WithAttributes(attribute.String("service.name", serviceName)),
		resource.WithFromEnv(),
		resource.WithTelemetrySDK(),
	)
	if err != nil {
		_ = exporter.Shutdown(ctx)
		return nil, fmt.Errorf("tracing resource: %w", tracingError{err})
	}
	// Export runs in the background, so its errors reach only OTel's
	// global error handler, which otherwise writes unstructured lines. They
	// can include the endpoint URL and the collector's response, so only
	// the error category is logged.
	otel.SetErrorHandler(otel.ErrorHandlerFunc(func(err error) {
		logger.Warn("tracing error", slog.Any("error", tracingError{err}))
	}))
	return sdktrace.NewTracerProvider(sdktrace.WithBatcher(exporter), sdktrace.WithResource(res)), nil
}

// startTracing returns the tracer provider if tracing is enabled, or nil,
// and a function that shuts it down, flushing pending spans within a
// shutdownTimeout budget of its own.
func startTracing(ctx context.Context, enabled bool, logger *slog.Logger) (trace.TracerProvider, func() error, error) {
	if !enabled {
		return nil, func() error { return nil }, nil
	}
	tp, err := newTracerProvider(ctx, logger)
	if err != nil {
		return nil, nil, err
	}
	return tp, func() error {
		ctx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
		defer cancel()
		if err := tp.Shutdown(ctx); err != nil {
			return fmt.Errorf("tracing shutdown: %w", tracingError{err})
		}
		return nil
	}, nil
}

// Preserve causes for callers while keeping exporter responses, URLs,
// and environment values out of diagnostics.
type tracingError struct{ cause error }

func (e tracingError) Error() string {
	switch {
	case errors.Is(e.cause, context.DeadlineExceeded):
		return "telemetry operation timed out"
	case errors.Is(e.cause, context.Canceled):
		return "telemetry operation canceled"
	default:
		return "telemetry operation failed; check collector availability and OTEL configuration"
	}
}

func (e tracingError) Unwrap() error { return e.cause }
