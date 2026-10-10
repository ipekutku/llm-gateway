package tracing

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/ipekutku/llm-gateway/internal/llm"
	"go.opentelemetry.io/otel/codes"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"
)

type providerFunc func(context.Context, llm.ChatRequest) (llm.ChatResponse, error)

func (f providerFunc) Chat(ctx context.Context, req llm.ChatRequest) (llm.ChatResponse, error) {
	return f(ctx, req)
}

func newTracer(t *testing.T) (*Tracer, *tracetest.SpanRecorder) {
	t.Helper()
	rec := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(rec))
	t.Cleanup(func() { _ = tp.Shutdown(context.Background()) })
	return New(tp), rec
}

func attrs(s sdktrace.ReadOnlySpan) map[string]string {
	m := make(map[string]string)
	for _, kv := range s.Attributes() {
		m[string(kv.Key)] = kv.Value.Emit()
	}
	return m
}

func onlySpan(t *testing.T, rec *tracetest.SpanRecorder) sdktrace.ReadOnlySpan {
	t.Helper()
	spans := rec.Ended()
	if len(spans) != 1 {
		t.Fatalf("got %d spans, want 1", len(spans))
	}
	return spans[0]
}

func TestNilTracerReturnsArguments(t *testing.T) {
	var tr *Tracer
	if New(nil) != nil {
		t.Fatal("New(nil) is not nil")
	}
	p := providerFunc(func(context.Context, llm.ChatRequest) (llm.ChatResponse, error) { return llm.ChatResponse{}, nil })
	for name, got := range map[string]llm.Provider{"Route": tr.Route(p), "Provider": tr.Provider("openai", p), "Attempt": tr.Attempt("openai", p)} {
		if _, ok := got.(providerFunc); !ok {
			t.Errorf("%s of a nil Tracer returned %T, want the provider itself", name, got)
		}
	}
	if got := tr.Handler(http.NotFoundHandler()); fmt.Sprintf("%T", got) != "http.HandlerFunc" {
		t.Errorf("Handler of a nil Tracer returned %T, want the handler itself", got)
	}
}

func TestHandlerServerSpan(t *testing.T) {
	tests := []struct {
		name   string
		method string
		path   string
		status int // 0 writes nothing
		span   string
		attrs  map[string]string
		error  bool
	}{
		{"success", http.MethodPost, "/v1/chat/completions", 200, "POST /v1/chat/completions",
			map[string]string{"http.request.method": "POST", "http.route": "/v1/chat/completions", "http.response.status_code": "200"}, false},
		{"client error", http.MethodPost, "/v1/chat/completions", 429, "POST /v1/chat/completions",
			map[string]string{"http.response.status_code": "429"}, false},
		{"server error", http.MethodPost, "/v1/chat/completions", 502, "POST /v1/chat/completions",
			map[string]string{"http.response.status_code": "502"}, true},
		{"other path", http.MethodGet, "/client/chosen/path", 404, "GET",
			map[string]string{"http.request.method": "GET", "http.route": "", "http.response.status_code": "404"}, false},
		{"nothing written", http.MethodPost, "/v1/chat/completions", 0, "POST /v1/chat/completions",
			map[string]string{"http.response.status_code": ""}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tr, rec := newTracer(t)
			h := tr.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if !trace.SpanFromContext(r.Context()).SpanContext().IsValid() {
					t.Error("request context has no span")
				}
				if tt.status != 0 {
					w.WriteHeader(tt.status)
				}
			}))
			h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(tt.method, tt.path, nil))

			s := onlySpan(t, rec)
			if s.Name() != tt.span || s.SpanKind() != trace.SpanKindServer {
				t.Errorf("span = %q (%v), want server span %q", s.Name(), s.SpanKind(), tt.span)
			}
			got := attrs(s)
			for k, v := range tt.attrs {
				if got[k] != v {
					t.Errorf("%s = %q, want %q", k, got[k], v)
				}
			}
			if (s.Status().Code == codes.Error) != tt.error {
				t.Errorf("status = %v, want error %v", s.Status(), tt.error)
			}
		})
	}
}

func TestHandlerAcceptsIncomingTraceContext(t *testing.T) {
	tr, rec := newTracer(t)
	h := tr.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	for _, traceparent := range []string{"00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01", "malformed"} {
		r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
		r.Header.Set("traceparent", traceparent)
		h.ServeHTTP(httptest.NewRecorder(), r)
	}
	spans := rec.Ended()
	if got := spans[0].Parent(); got.TraceID().String() != "4bf92f3577b34da6a3ce929d0e0e4736" || !got.IsRemote() {
		t.Errorf("parent = %v, want the incoming span", got)
	}
	// A malformed header starts a new trace.
	if spans[1].Parent().IsValid() || !spans[1].SpanContext().IsValid() {
		t.Errorf("span with malformed traceparent: parent %v", spans[1].Parent())
	}
}

func TestHandlerKeepsResponseController(t *testing.T) {
	// The gateway sets read deadlines through http.ResponseController, which
	// must reach the connection through the status-recording writer.
	tr, _ := newTracer(t)
	errs := make(chan error, 1)
	srv := httptest.NewServer(tr.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		errs <- http.NewResponseController(w).SetReadDeadline(time.Now().Add(time.Minute))
	})))
	t.Cleanup(srv.Close)
	resp, err := http.Get(srv.URL)
	if err != nil {
		t.Fatalf("GET error = %v", err)
	}
	resp.Body.Close()
	if err := <-errs; err != nil {
		t.Errorf("SetReadDeadline() error = %v", err)
	}
}

func TestProviderSpans(t *testing.T) {
	resp := llm.ChatResponse{
		Provider: "openai", Model: "gpt-4o-2024-08-06", FinishReason: llm.FinishReasonStop,
		Message: llm.Message{Role: llm.RoleAssistant, Content: "secret completion"},
		Usage:   llm.Usage{InputTokens: 12, CacheReadInputTokens: 4, CacheWriteInputTokens: 2, OutputTokens: 3},
	}
	ok := providerFunc(func(context.Context, llm.ChatRequest) (llm.ChatResponse, error) { return resp, nil })
	req := llm.ChatRequest{Model: "gpt-4o", MaxTokens: 100, Messages: []llm.Message{{Role: llm.RoleUser, Content: "secret prompt"}}}

	tests := []struct {
		name  string
		wrap  func(*Tracer, llm.Provider) llm.Provider
		span  string
		kind  trace.SpanKind
		attrs map[string]string
	}{
		{"route", (*Tracer).Route, "route", trace.SpanKindInternal,
			map[string]string{"gen_ai.request.model": "gpt-4o", "gen_ai.provider.name": "openai"}},
		{"provider", func(t *Tracer, p llm.Provider) llm.Provider { return t.Provider("openai", p) }, "provider openai", trace.SpanKindInternal,
			map[string]string{"gen_ai.request.model": "gpt-4o", "gen_ai.provider.name": "openai"}},
		{"attempt", func(t *Tracer, p llm.Provider) llm.Provider { return t.Attempt("openai", p) }, "chat gpt-4o", trace.SpanKindClient,
			map[string]string{
				"gen_ai.operation.name": "chat", "gen_ai.provider.name": "openai", "gen_ai.request.model": "gpt-4o",
				"gen_ai.request.max_tokens": "100", "gen_ai.response.model": "gpt-4o-2024-08-06",
				"gen_ai.response.finish_reasons": `["stop"]`, "gen_ai.usage.input_tokens": "12", "gen_ai.usage.output_tokens": "3",
				"gateway.usage.cache_read_input_tokens": "4", "gateway.usage.cache_write_input_tokens": "2",
			}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tr, rec := newTracer(t)
			got, err := tt.wrap(tr, ok).Chat(context.Background(), req)
			if err != nil || got.Message.Content != resp.Message.Content {
				t.Fatalf("Chat() = %v, %v; want the wrapped provider's response", got, err)
			}
			s := onlySpan(t, rec)
			if s.Name() != tt.span || s.SpanKind() != tt.kind {
				t.Errorf("span = %q (%v), want %q (%v)", s.Name(), s.SpanKind(), tt.span, tt.kind)
			}
			a := attrs(s)
			if len(a) != len(tt.attrs) {
				t.Errorf("attributes = %v, want %v", a, tt.attrs)
			}
			for k, v := range tt.attrs {
				if a[k] != v {
					t.Errorf("%s = %q, want %q", k, a[k], v)
				}
			}
			if s.Status().Code == codes.Error {
				t.Errorf("status = %v, want unset", s.Status())
			}
		})
	}
}

func TestProviderSpanErrors(t *testing.T) {
	transport := &llm.ProviderError{Provider: "openai", Err: errors.New("connection reset")}
	tests := []struct {
		err       error
		errorType string
		status    string // http.response.status_code on the attempt span
	}{
		{&llm.ProviderError{Provider: "openai", StatusCode: 503}, "503", "503"},
		{transport, "transport", ""},
		{fmt.Errorf("after 3 attempts: %w", context.DeadlineExceeded), "timeout", ""},
		{context.Canceled, "canceled", ""},
		{fmt.Errorf("openai: %w", llm.ErrCircuitOpen), "circuit_open", ""},
		{fmt.Errorf("%w: %q", llm.ErrUnknownModel, "gpt-5"), "unknown_model", ""},
		{errors.New("bug"), "_OTHER", ""},
	}
	for _, tt := range tests {
		t.Run(tt.errorType, func(t *testing.T) {
			tr, rec := newTracer(t)
			p := tr.Attempt("openai", providerFunc(func(context.Context, llm.ChatRequest) (llm.ChatResponse, error) {
				return llm.ChatResponse{}, tt.err
			}))
			if _, err := p.Chat(context.Background(), llm.ChatRequest{Model: "gpt-4o"}); err != tt.err {
				t.Fatalf("Chat() error = %v, want the wrapped provider's error unchanged", err)
			}
			s := onlySpan(t, rec)
			a := attrs(s)
			if a["error.type"] != tt.errorType || a["http.response.status_code"] != tt.status {
				t.Errorf("error.type = %q, status = %q; want %q, %q", a["error.type"], a["http.response.status_code"], tt.errorType, tt.status)
			}
			if s.Status().Code != codes.Error || s.Status().Description != tt.err.Error() {
				t.Errorf("status = %v, want error %q", s.Status(), tt.err)
			}
			if _, ok := a["gen_ai.usage.input_tokens"]; ok {
				t.Error("failed attempt reports usage")
			}
		})
	}
}

func TestSpansNest(t *testing.T) {
	tr, rec := newTracer(t)
	attempt := tr.Attempt("openai", providerFunc(func(context.Context, llm.ChatRequest) (llm.ChatResponse, error) {
		return llm.ChatResponse{Provider: "openai"}, nil
	}))
	p := tr.Route(tr.Provider("openai", attempt))
	if _, err := p.Chat(context.Background(), llm.ChatRequest{Model: "gpt-4o"}); err != nil {
		t.Fatalf("Chat() error = %v", err)
	}
	spans := rec.Ended() // innermost first
	if len(spans) != 3 {
		t.Fatalf("got %d spans, want 3", len(spans))
	}
	for i := range 2 {
		if spans[i].Parent().SpanID() != spans[i+1].SpanContext().SpanID() {
			t.Errorf("%s is not a child of %s", spans[i].Name(), spans[i+1].Name())
		}
	}
}
