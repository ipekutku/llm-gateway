// Package tracing creates the gateway's OpenTelemetry spans for a request:
// a server span for the HTTP request, a span for routing, a span per
// provider the request is sent to, and a client span per upstream attempt.
//
// Spans carry identifiers, models, statuses, token counts, and error
// messages, never prompt or completion content or credentials. Incoming W3C
// trace context (traceparent) is accepted, so a client's trace continues in
// the gateway, but trace context is never sent to providers.
//
// The methods of a nil *Tracer return their argument unchanged, so wiring
// can leave tracing out at no cost.
package tracing

import (
	"context"
	"errors"
	"net/http"
	"strconv"

	"github.com/ipekutku/llm-gateway/internal/httpapi"
	"github.com/ipekutku/llm-gateway/internal/llm"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/trace"
)

// ScopeName is the instrumentation scope of the gateway's spans.
const ScopeName = "github.com/ipekutku/llm-gateway"

// Tracer creates spans with a tracer of one TracerProvider. It is safe for
// concurrent use.
type Tracer struct {
	tracer trace.Tracer
	// propagator reads incoming trace context. It is never used to inject.
	propagator propagation.TextMapPropagator
}

// New returns a Tracer using tp, or nil if tp is nil.
func New(tp trace.TracerProvider) *Tracer {
	if tp == nil {
		return nil
	}
	return &Tracer{tracer: tp.Tracer(ScopeName), propagator: propagation.TraceContext{}}
}

// Handler returns next wrapped in a server span per request. A valid
// incoming traceparent header makes the span its child. The span records
// the response status; a 5xx status marks it as an error. next adds the
// request's own attributes to the span in the request context.
func (t *Tracer) Handler(next http.Handler) http.Handler {
	if t == nil {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx := t.propagator.Extract(r.Context(), propagation.HeaderCarrier(r.Header))
		// Only the gateway's own route names a span, so arbitrary paths sent
		// by clients cannot create span names.
		name, attrs := r.Method, []attribute.KeyValue{attribute.String("http.request.method", r.Method)}
		if r.URL.Path == httpapi.ChatCompletionsPath {
			name += " " + httpapi.ChatCompletionsPath
			attrs = append(attrs, attribute.String("http.route", httpapi.ChatCompletionsPath))
		}
		ctx, span := t.tracer.Start(ctx, name, trace.WithSpanKind(trace.SpanKindServer), trace.WithAttributes(attrs...))
		defer span.End()

		sw := &statusWriter{ResponseWriter: w}
		next.ServeHTTP(sw, r.WithContext(ctx))
		// No status means the client went away before a response was
		// written; next records that on the span.
		if sw.status != 0 {
			span.SetAttributes(attribute.Int("http.response.status_code", sw.status))
			if sw.status >= http.StatusInternalServerError {
				span.SetStatus(codes.Error, "")
			}
		}
	})
}

// statusWriter records the response status. Unwrap lets
// http.ResponseController reach the underlying writer.
type statusWriter struct {
	http.ResponseWriter
	status int
}

func (w *statusWriter) WriteHeader(status int) {
	if w.status == 0 {
		w.status = status
	}
	w.ResponseWriter.WriteHeader(status)
}

func (w *statusWriter) Write(b []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	return w.ResponseWriter.Write(b)
}

func (w *statusWriter) Unwrap() http.ResponseWriter {
	return w.ResponseWriter
}

// Route returns p, normally the router, wrapped in a span named "route" per
// call. The router adds a "fallback" event to it when it falls back.
func (t *Tracer) Route(p llm.Provider) llm.Provider {
	if t == nil {
		return p
	}
	return span{tracer: t.tracer, name: "route", next: p}
}

// Provider returns p wrapped in a span named "provider <name>" per call.
// Wrap the provider's circuit breaker, so the span covers all its attempts
// and the retry layer's "retry" events, and a call the open circuit rejects
// still has one.
func (t *Tracer) Provider(name string, p llm.Provider) llm.Provider {
	if t == nil {
		return p
	}
	return span{tracer: t.tracer, name: "provider " + name, provider: name, next: p}
}

// Attempt returns p wrapped in a client span per call, one upstream
// attempt of provider. Wrap each adapter directly, below retries, so every
// attempt has a span.
func (t *Tracer) Attempt(provider string, p llm.Provider) llm.Provider {
	if t == nil {
		return p
	}
	return attempt{tracer: t.tracer, provider: provider, next: p}
}

// span is an internal span around one call to next.
type span struct {
	tracer   trace.Tracer
	name     string
	provider string // empty for the route span
	next     llm.Provider
}

func (s span) Chat(ctx context.Context, req llm.ChatRequest) (llm.ChatResponse, error) {
	attrs := []attribute.KeyValue{attribute.String("gen_ai.request.model", req.Model)}
	if s.provider != "" {
		attrs = append(attrs, attribute.String("gen_ai.provider.name", s.provider))
	}
	ctx, sp := s.tracer.Start(ctx, s.name, trace.WithAttributes(attrs...))
	defer sp.End()
	resp, err := s.next.Chat(ctx, req)
	if err != nil {
		setError(sp, err)
	} else if s.provider == "" {
		sp.SetAttributes(attribute.String("gen_ai.provider.name", resp.Provider))
	}
	return resp, err
}

// attempt is the client span of one upstream attempt.
type attempt struct {
	tracer   trace.Tracer
	provider string
	next     llm.Provider
}

func (a attempt) Chat(ctx context.Context, req llm.ChatRequest) (llm.ChatResponse, error) {
	ctx, sp := a.tracer.Start(ctx, "chat "+req.Model,
		trace.WithSpanKind(trace.SpanKindClient),
		trace.WithAttributes(
			attribute.String("gen_ai.operation.name", "chat"),
			attribute.String("gen_ai.provider.name", a.provider),
			attribute.String("gen_ai.request.model", req.Model),
			attribute.Int("gen_ai.request.max_tokens", req.MaxTokens),
		))
	defer sp.End()
	resp, err := a.next.Chat(ctx, req)
	if err != nil {
		if pe, ok := errors.AsType[*llm.ProviderError](err); ok && pe.StatusCode != 0 {
			sp.SetAttributes(attribute.Int("http.response.status_code", pe.StatusCode))
		}
		setError(sp, err)
		return resp, err
	}
	sp.SetAttributes(
		attribute.String("gen_ai.response.model", resp.Model),
		attribute.StringSlice("gen_ai.response.finish_reasons", []string{resp.FinishReason}),
		attribute.Int("gen_ai.usage.input_tokens", resp.Usage.InputTokens),
		attribute.Int("gen_ai.usage.output_tokens", resp.Usage.OutputTokens),
		attribute.Int("gateway.usage.cache_read_input_tokens", resp.Usage.CacheReadInputTokens),
		attribute.Int("gateway.usage.cache_write_input_tokens", resp.Usage.CacheWriteInputTokens),
	)
	return resp, nil
}

// setError marks sp as failed with err's message and its error.type. Error
// messages carry no credentials, content, or upstream bodies; see
// llm.ProviderError.
func setError(sp trace.Span, err error) {
	sp.SetAttributes(attribute.String("error.type", errorType(err)))
	sp.SetStatus(codes.Error, err.Error())
}

// errorType classifies err for the error.type attribute: "timeout",
// "canceled", "circuit_open", "unknown_model", the upstream HTTP status for
// an upstream error response, "transport" for an upstream failure without
// a usable response, and "_OTHER" otherwise. Context errors are checked
// first.
func errorType(err error) string {
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		return "timeout"
	case errors.Is(err, context.Canceled):
		return "canceled"
	case errors.Is(err, llm.ErrCircuitOpen):
		return "circuit_open"
	case errors.Is(err, llm.ErrUnknownModel):
		return "unknown_model"
	}
	if pe, ok := errors.AsType[*llm.ProviderError](err); ok {
		if pe.StatusCode != 0 {
			return strconv.Itoa(pe.StatusCode)
		}
		return "transport"
	}
	return "_OTHER"
}
