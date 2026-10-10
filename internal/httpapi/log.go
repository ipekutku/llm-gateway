package httpapi

import (
	"context"
	"log/slog"

	"github.com/ipekutku/llm-gateway/internal/auth"
	"go.opentelemetry.io/otel/trace"
)

// logHandler adds the request ID, authenticated client ID, and trace ID
// carried in a record's context to the record.
type logHandler struct {
	next slog.Handler
}

// NewLogHandler returns a handler that adds request_id and client_id to
// every record logged with the context of a request this package handles,
// and trace_id to every record logged with the context of a trace, then
// passes it to next. Records logged with another context are passed on
// unchanged. Below a group set by WithGroup, the attributes are in that
// group.
//
// The gateway wraps its root handler with it, so the logs of every layer a
// request passes through, such as retries and the circuit breaker, identify
// the request without those layers knowing about it.
func NewLogHandler(next slog.Handler) slog.Handler {
	return logHandler{next: next}
}

func (h logHandler) Enabled(ctx context.Context, level slog.Level) bool {
	return h.next.Enabled(ctx, level)
}

func (h logHandler) Handle(ctx context.Context, r slog.Record) error {
	if ctx != nil {
		if id, ok := ctx.Value(requestIDKey{}).(string); ok {
			r.AddAttrs(slog.String("request_id", id))
		}
		if id, ok := auth.FromContext(ctx); ok {
			r.AddAttrs(slog.String("client_id", id.ClientID))
		}
		if sc := trace.SpanContextFromContext(ctx); sc.HasTraceID() {
			r.AddAttrs(slog.String("trace_id", sc.TraceID().String()))
		}
	}
	return h.next.Handle(ctx, r)
}

func (h logHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return logHandler{next: h.next.WithAttrs(attrs)}
}

func (h logHandler) WithGroup(name string) slog.Handler {
	return logHandler{next: h.next.WithGroup(name)}
}
