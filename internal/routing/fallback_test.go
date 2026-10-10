package routing

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ipekutku/llm-gateway/internal/llm"
)

// guard bounds how long a test waits for something that should happen
// promptly. It is a failure guard, not a synchronization mechanism.
const guard = 5 * time.Second

var fallbackRequest = llm.ChatRequest{
	Model: "model-a",
	Messages: []llm.Message{
		{Role: llm.RoleSystem, Content: "Be brief."},
		{Role: llm.RoleUser, Content: "Explain TCP."},
	},
	MaxTokens: 100,
}

func statusErr(provider string, status int) error {
	return &llm.ProviderError{Provider: provider, StatusCode: status, Err: errors.New("unexpected status")}
}

// recorder is a provider that records its calls and answers with err, or
// with a response naming model if err is nil.
type recorder struct {
	mu    sync.Mutex
	model string
	err   error
	reqs  []llm.ChatRequest
	ctxs  []context.Context
}

func (p *recorder) Chat(ctx context.Context, req llm.ChatRequest) (llm.ChatResponse, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.reqs = append(p.reqs, req)
	p.ctxs = append(p.ctxs, ctx)
	if p.err != nil {
		return llm.ChatResponse{}, p.err
	}
	return llm.ChatResponse{
		Model:        p.model,
		Message:      llm.Message{Role: llm.RoleAssistant, Content: "from " + p.model},
		FinishReason: llm.FinishReasonStop,
	}, nil
}

func (p *recorder) Calls() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.reqs)
}

// newFallbackRouter routes model-a to primary, falling back to fallback
// with model-b.
func newFallbackRouter(t *testing.T, primary, fallback llm.Provider, primaryTimeout time.Duration) (*Router, *strings.Builder) {
	t.Helper()
	var logs strings.Builder
	r, err := New(map[string]Route{
		"model-a": {
			Provider: primary,
			Fallback: &Fallback{Model: "model-b", Provider: fallback, PrimaryTimeout: primaryTimeout},
		},
	}, slog.New(slog.NewTextHandler(&logs, nil)))
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	return r, &logs
}

func TestFallbackNotUsedWhenPrimarySucceeds(t *testing.T) {
	primary := &recorder{model: "model-a-2026"}
	fallback := &recorder{model: "model-b-2026"}
	r, logs := newFallbackRouter(t, primary, fallback, time.Minute)

	resp, err := r.Chat(context.Background(), fallbackRequest)

	if err != nil || resp.Model != "model-a-2026" {
		t.Fatalf("Chat() = %+v, %v; want the primary's answer", resp, err)
	}
	if fallback.Calls() != 0 {
		t.Errorf("fallback called %d times, want 0", fallback.Calls())
	}
	if !reflect.DeepEqual(primary.reqs[0], fallbackRequest) {
		t.Errorf("primary received %+v, want the request unchanged", primary.reqs[0])
	}
	deadline, ok := primary.ctxs[0].Deadline()
	if !ok || time.Until(deadline) > time.Minute {
		t.Errorf("primary deadline = %v (set %v), want within the primary timeout", deadline, ok)
	}
	if logs.Len() != 0 {
		t.Errorf("logged on success:\n%s", logs)
	}
}

func TestFallbackAnswersWhenPrimaryFails(t *testing.T) {
	primary := &recorder{err: statusErr("openai", http.StatusServiceUnavailable)}
	fallback := &recorder{model: "model-b-2026"}
	r, logs := newFallbackRouter(t, primary, fallback, time.Minute)
	ctx, cancel := context.WithTimeout(context.Background(), time.Hour)
	defer cancel()

	resp, err := r.Chat(ctx, fallbackRequest)

	if err != nil || resp.Model != "model-b-2026" || resp.Message.Content != "from model-b-2026" {
		t.Fatalf("Chat() = %+v, %v; want the fallback's answer", resp, err)
	}
	if primary.Calls() != 1 || fallback.Calls() != 1 {
		t.Errorf("calls = primary %d, fallback %d; want 1 each", primary.Calls(), fallback.Calls())
	}
	// The fallback gets the fallback model and the request's own deadline,
	// not the primary's.
	want := fallbackRequest
	want.Model = "model-b"
	if !reflect.DeepEqual(fallback.reqs[0], want) {
		t.Errorf("fallback received %+v, want %+v", fallback.reqs[0], want)
	}
	if got, _ := fallback.ctxs[0].Deadline(); !got.Equal(mustDeadline(t, ctx)) {
		t.Errorf("fallback deadline = %v, want the request deadline %v", got, mustDeadline(t, ctx))
	}
	for _, want := range []string{`msg="primary provider failed, falling back"`, "provider=openai", "upstream_status=503", "model=model-a", "fallback_model=model-b"} {
		if !strings.Contains(logs.String(), want) {
			t.Errorf("log does not contain %q:\n%s", want, logs)
		}
	}
}

func mustDeadline(t *testing.T, ctx context.Context) time.Time {
	t.Helper()
	d, ok := ctx.Deadline()
	if !ok {
		t.Fatal("context has no deadline")
	}
	return d
}

func TestFallbackReportsFallbackModelWhenUpstreamOmitsIt(t *testing.T) {
	primary := &recorder{err: statusErr("openai", http.StatusBadGateway)}
	fallback := &recorder{model: ""}
	r, _ := newFallbackRouter(t, primary, fallback, time.Minute)

	resp, err := r.Chat(context.Background(), fallbackRequest)

	if err != nil || resp.Model != "model-b" {
		t.Errorf("Chat() = %+v, %v; want model %q, never the requested model", resp, err, "model-b")
	}
}

func TestFallbackFailureReturnsFallbackError(t *testing.T) {
	// The primary timed out and the fallback is unavailable. The response
	// must reflect the fallback (502), not the primary's deadline (504).
	primaryErr := &llm.ProviderError{Provider: "openai", Err: context.DeadlineExceeded}
	fallbackErr := statusErr("anthropic", http.StatusServiceUnavailable)
	r, _ := newFallbackRouter(t, &recorder{err: primaryErr}, &recorder{err: fallbackErr}, time.Minute)

	_, err := r.Chat(context.Background(), fallbackRequest)

	pe, ok := errors.AsType[*llm.ProviderError](err)
	if !ok || pe != fallbackErr {
		t.Errorf("error = %v, want it to wrap the fallback's error", err)
	}
	if errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("error = %v, must not wrap the primary's deadline", err)
	}
	for _, want := range []string{`fallback to "model-b" failed`, "primary failure: openai"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error = %q, want it to contain %q", err, want)
		}
	}
}

func TestFallbackTriggers(t *testing.T) {
	tests := []struct {
		name     string
		err      error
		fallback bool
	}{
		{"rate limited", statusErr("openai", 429), true},
		{"server error", statusErr("openai", 500), true},
		{"unavailable after retries", fmt.Errorf("after 3 attempts: %w", statusErr("openai", 503)), true},
		{"overloaded", statusErr("anthropic", 529), true},
		{"authentication failure", statusErr("openai", 401), true},
		{"model not found upstream", statusErr("openai", 404), true},
		{"transport failure", &llm.ProviderError{Provider: "openai", Err: errors.New("connection refused")}, true},
		{"unusable response", &llm.ProviderError{Provider: "openai", StatusCode: 200, Err: errors.New("malformed response JSON")}, true},
		{"primary timeout", &llm.ProviderError{Provider: "openai", Err: context.DeadlineExceeded}, true},
		{"open circuit", fmt.Errorf("openai: %w", llm.ErrCircuitOpen), true},
		{"rejected request", statusErr("openai", 400), false},
		{"gateway error", errors.New("openai: invalid request: no messages"), false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			primary := &recorder{err: tt.err}
			fallback := &recorder{model: "model-b"}
			r, _ := newFallbackRouter(t, primary, fallback, time.Minute)

			_, err := r.Chat(context.Background(), fallbackRequest)

			if got := fallback.Calls() == 1; got != tt.fallback {
				t.Errorf("fell back = %v, want %v", got, tt.fallback)
			}
			if !tt.fallback && err != tt.err {
				t.Errorf("error = %v, want the primary's error unchanged", err)
			}
		})
	}
}

// blockingProvider blocks until ctx is done, then fails as the adapters
// do. It signals each call on started.
type blockingProvider struct {
	started chan struct{}
}

func (p *blockingProvider) Chat(ctx context.Context, _ llm.ChatRequest) (llm.ChatResponse, error) {
	p.started <- struct{}{}
	<-ctx.Done()
	return llm.ChatResponse{}, &llm.ProviderError{Provider: "openai", Err: ctx.Err()}
}

func TestFallbackAfterPrimaryTimeout(t *testing.T) {
	primary := &blockingProvider{started: make(chan struct{}, 1)}
	fallback := &recorder{model: "model-b"}
	r, _ := newFallbackRouter(t, primary, fallback, 20*time.Millisecond)
	ctx, cancel := context.WithTimeout(context.Background(), guard)
	defer cancel()

	resp, err := r.Chat(ctx, fallbackRequest)

	if err != nil || resp.Model != "model-b" {
		t.Fatalf("Chat() = %+v, %v; want the fallback's answer after the primary timed out", resp, err)
	}
	if ctx.Err() != nil {
		t.Error("the request's own deadline expired; the primary timeout did not fire first")
	}
}

func TestNoFallbackWhenClientCancelsDuringPrimary(t *testing.T) {
	primary := &blockingProvider{started: make(chan struct{}, 1)}
	fallback := &recorder{model: "model-b"}
	r, logs := newFallbackRouter(t, primary, fallback, time.Hour)
	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan error, 1)
	go func() {
		_, err := r.Chat(ctx, fallbackRequest)
		done <- err
	}()
	waitFor(t, primary.started, "the primary call")
	cancel()

	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Errorf("error = %v, want context.Canceled", err)
		}
	case <-time.After(guard):
		t.Fatal("Chat did not return after cancellation")
	}
	if fallback.Calls() != 0 {
		t.Errorf("fallback called %d times after the client canceled, want 0", fallback.Calls())
	}
	if logs.Len() != 0 {
		t.Errorf("logged a fallback after cancellation:\n%s", logs)
	}
}

func TestCancellationDuringFallbackStopsIt(t *testing.T) {
	primary := &recorder{err: statusErr("openai", http.StatusServiceUnavailable)}
	fallback := &blockingProvider{started: make(chan struct{}, 1)}
	r, _ := newFallbackRouter(t, primary, fallback, time.Hour)
	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan error, 1)
	go func() {
		_, err := r.Chat(ctx, fallbackRequest)
		done <- err
	}()
	waitFor(t, fallback.started, "the fallback call")
	cancel()

	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Errorf("error = %v, want context.Canceled", err)
		}
	case <-time.After(guard):
		t.Fatal("Chat did not return after cancellation during the fallback")
	}
	if primary.Calls() != 1 {
		t.Errorf("primary called %d times, want 1: fallback never goes back", primary.Calls())
	}
}

func TestFallbackDoesNotMutateRequest(t *testing.T) {
	primary := &recorder{err: statusErr("openai", http.StatusServiceUnavailable)}
	r, _ := newFallbackRouter(t, primary, &recorder{model: "model-b"}, time.Minute)
	req := fallbackRequest
	req.Messages = append([]llm.Message(nil), fallbackRequest.Messages...)

	if _, err := r.Chat(context.Background(), req); err != nil {
		t.Fatalf("Chat() error = %v", err)
	}
	if !reflect.DeepEqual(req, fallbackRequest) {
		t.Errorf("request after Chat = %+v, want it unchanged", req)
	}
}

func TestNewRejectsInvalidFallbacks(t *testing.T) {
	p := &recorder{}
	tests := []struct {
		name     string
		fallback Fallback
	}{
		{"blank model", Fallback{Model: " ", Provider: p, PrimaryTimeout: time.Second}},
		{"nil provider", Fallback{Model: "model-b", PrimaryTimeout: time.Second}},
		{"zero primary timeout", Fallback{Model: "model-b", Provider: p}},
		{"negative primary timeout", Fallback{Model: "model-b", Provider: p, PrimaryTimeout: -time.Second}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := tt.fallback
			if _, err := New(map[string]Route{"model-a": {Provider: p, Fallback: &f}}, nil); err == nil {
				t.Error("New() error = nil, want error")
			}
		})
	}
}

func TestNewCopiesFallback(t *testing.T) {
	primary := &recorder{err: statusErr("openai", http.StatusServiceUnavailable)}
	fallback := &recorder{model: "model-b"}
	f := &Fallback{Model: "model-b", Provider: fallback, PrimaryTimeout: time.Minute}
	r, err := New(map[string]Route{"model-a": {Provider: primary, Fallback: f}}, nil)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}

	f.Model = "changed"

	if _, err := r.Chat(context.Background(), fallbackRequest); err != nil {
		t.Fatalf("Chat() error = %v", err)
	}
	if got := fallback.reqs[0].Model; got != "model-b" {
		t.Errorf("fallback model = %q, want %q: the router must copy its fallback", got, "model-b")
	}
}

func waitFor(t *testing.T, ch <-chan struct{}, what string) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(guard):
		t.Fatalf("timed out waiting for %s", what)
	}
}

func TestFallbackRecordedInStats(t *testing.T) {
	for _, tt := range []struct {
		name       string
		primaryErr error
		want       bool
	}{
		{"primary succeeds", nil, false},
		{"primary fails", statusErr("openai", http.StatusServiceUnavailable), true},
		{"primary rejects the request", statusErr("openai", http.StatusBadRequest), false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			r, _ := newFallbackRouter(t, &recorder{model: "model-a", err: tt.primaryErr}, &recorder{model: "model-b"}, time.Minute)
			ctx, stats := llm.WithStats(context.Background())

			_, _ = r.Chat(ctx, fallbackRequest)

			if stats.Fallback() != tt.want {
				t.Errorf("Fallback() = %v, want %v", stats.Fallback(), tt.want)
			}
		})
	}
}
