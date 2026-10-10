// Package routing selects a provider for a request by exact model name and
// falls back to a second provider when the first fails.
package routing

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/ipekutku/llm-gateway/internal/llm"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
)

// Route is the configuration for one model.
type Route struct {
	// Provider serves the model. It receives requests unchanged.
	Provider llm.Provider
	// Fallback, if set, is tried once when Provider fails.
	Fallback *Fallback
}

// Fallback is a second provider for a route, tried at most once.
type Fallback struct {
	// Model replaces the requested model name in the fallback request,
	// because each provider has its own model names.
	Model string
	// Provider serves the fallback request.
	Provider llm.Provider
	// PrimaryTimeout bounds the route's primary provider, so a primary that
	// hangs leaves the fallback the rest of the request's time.
	PrimaryTimeout time.Duration
	// OnFallback, if not nil, is called just before the fallback provider
	// is called. It must be safe for concurrent use.
	OnFallback func()
}

// Router routes chat requests to providers using a static model table.
// Its routing table is safe for concurrent reads and never modified after New.
// Configured providers must support concurrent calls as required by llm.Provider.
type Router struct {
	routes map[string]Route
	log    *slog.Logger
}

var _ llm.Provider = (*Router)(nil)

// New returns a Router for the given model-to-route table. The table is
// copied, so later changes to routes do not affect the Router. A nil log
// uses slog.Default.
func New(routes map[string]Route, log *slog.Logger) (*Router, error) {
	if len(routes) == 0 {
		return nil, errors.New("routing: no routes configured")
	}

	copied := make(map[string]Route, len(routes))
	for model, r := range routes {
		if strings.TrimSpace(model) == "" {
			return nil, errors.New("routing: blank model name")
		}
		if r.Provider == nil {
			return nil, fmt.Errorf("routing: nil provider for model %q", model)
		}
		if f := r.Fallback; f != nil {
			switch {
			case strings.TrimSpace(f.Model) == "":
				return nil, fmt.Errorf("routing: blank fallback model for model %q", model)
			case f.Provider == nil:
				return nil, fmt.Errorf("routing: nil fallback provider for model %q", model)
			case f.PrimaryTimeout <= 0:
				return nil, fmt.Errorf("routing: non-positive primary timeout for model %q", model)
			}
			fallback := *f
			r.Fallback = &fallback
		}
		copied[model] = r
	}
	if log == nil {
		log = slog.Default()
	}
	return &Router{routes: copied, log: log}, nil
}

// Chat sends req to the provider configured for req.Model. It returns an
// error wrapping llm.ErrUnknownModel if no provider is configured.
//
// Without a fallback, ctx and req are forwarded unchanged and provider
// errors are returned as is.
//
// With a fallback, the primary call is bounded by the fallback's
// PrimaryTimeout. If it fails in a way another provider might not (see
// shouldFallBack), the fallback is called once with the fallback model and
// the remaining time of ctx. The primary is never retried here, so there is
// no retry or fallback loop. If the fallback also fails, its error is
// returned, annotated with the primary failure's message; only the fallback
// error is wrapped, so the response reflects the last provider tried. A
// fallback is recorded in the llm.Stats carried by ctx, if any, and as a
// "fallback" event on the trace span in ctx, if any.
func (r *Router) Chat(ctx context.Context, req llm.ChatRequest) (llm.ChatResponse, error) {
	route, ok := r.routes[req.Model]
	if !ok {
		return llm.ChatResponse{}, fmt.Errorf("%w: %q", llm.ErrUnknownModel, req.Model)
	}
	f := route.Fallback
	if f == nil {
		return route.Provider.Chat(ctx, req)
	}

	primaryCtx, cancel := context.WithTimeout(ctx, f.PrimaryTimeout)
	resp, primaryErr := route.Provider.Chat(primaryCtx, req)
	cancel()
	if primaryErr == nil || !shouldFallBack(ctx, primaryErr) {
		return resp, primaryErr
	}

	r.log.LogAttrs(ctx, slog.LevelWarn, "primary provider failed, falling back",
		append(providerAttrs(primaryErr),
			slog.String("model", req.Model),
			slog.String("fallback_model", f.Model),
			slog.Any("error", primaryErr),
		)...,
	)

	llm.StatsFrom(ctx).SetFallback()
	event := []attribute.KeyValue{attribute.String("gateway.fallback.model", f.Model)}
	if pe, ok := errors.AsType[*llm.ProviderError](primaryErr); ok {
		event = append(event, attribute.String("gateway.fallback.from_provider", pe.Provider))
	}
	trace.SpanFromContext(ctx).AddEvent("fallback", trace.WithAttributes(event...))
	if f.OnFallback != nil {
		f.OnFallback()
	}
	fallbackReq := req
	fallbackReq.Model = f.Model
	resp, err := f.Provider.Chat(ctx, fallbackReq)
	if err != nil {
		return llm.ChatResponse{}, fmt.Errorf("fallback to %q failed: %w (primary failure: %v)", f.Model, err, primaryErr)
	}
	if resp.Model == "" {
		// Never report the requested model for an answer from another one.
		resp.Model = f.Model
	}
	return resp, nil
}

// shouldFallBack reports whether a primary failure should be retried on the
// fallback provider. It does not when the incoming request is done, since
// nobody would receive the answer, and when the upstream rejected the
// request (400), which is the client's mistake. Every other upstream
// failure falls back: rate limiting, server errors, transport failures,
// the primary timeout, unusable responses, authentication and other
// configuration errors, and an open circuit breaker. Errors that come from
// neither an upstream nor a breaker are gateway bugs and do not fall back.
func shouldFallBack(ctx context.Context, err error) bool {
	if ctx.Err() != nil {
		return false
	}
	if errors.Is(err, llm.ErrCircuitOpen) {
		return true
	}
	pe, ok := errors.AsType[*llm.ProviderError](err)
	return ok && pe.StatusCode != http.StatusBadRequest
}

func providerAttrs(err error) []slog.Attr {
	pe, ok := errors.AsType[*llm.ProviderError](err)
	if !ok {
		return nil
	}
	return []slog.Attr{
		slog.String("provider", pe.Provider),
		slog.Int("upstream_status", pe.StatusCode),
	}
}
