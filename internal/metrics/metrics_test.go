package metrics

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/ipekutku/llm-gateway/internal/breaker"
	"github.com/ipekutku/llm-gateway/internal/llm"
)

func newTestMetrics(t *testing.T) *Metrics {
	t.Helper()
	m, err := New([]string{"gpt-4o", "claude-opus-5-5"})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	return m
}

// scrape returns the metrics text served by m.
func scrape(t *testing.T, m *Metrics) string {
	t.Helper()
	rec := httptest.NewRecorder()
	m.Handler(slog.New(slog.DiscardHandler)).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("metrics status = %d", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/plain") {
		t.Errorf("Content-Type = %q, want the Prometheus text format", ct)
	}
	body, _ := io.ReadAll(rec.Body)
	return string(body)
}

// seriesLines returns the lines of text for the metric name, without
// comments.
func seriesLines(text, name string) []string {
	var lines []string
	for line := range strings.Lines(text) {
		if strings.HasPrefix(line, name+"{") || strings.HasPrefix(line, name+" ") {
			lines = append(lines, strings.TrimSpace(line))
		}
	}
	return lines
}

func TestNewRejectsInvalidModels(t *testing.T) {
	for _, models := range [][]string{{""}, {"  "}, {UnknownModel}} {
		if _, err := New(models); err == nil {
			t.Errorf("New(%q) error = nil, want an error", models)
		}
	}
}

func TestObserveRequest(t *testing.T) {
	m := newTestMetrics(t)
	m.ObserveRequest("gpt-4o", http.StatusOK, "", 300*time.Millisecond)
	m.ObserveRequest("gpt-4o", http.StatusOK, "", 2*time.Second)
	m.ObserveRequest("claude-opus-5-5", http.StatusBadGateway, "upstream_error", time.Second)
	m.ObserveRequest("", http.StatusUnauthorized, "invalid_api_key", time.Millisecond)

	text := scrape(t, m)
	for _, want := range []string{
		`gateway_requests_total{code="",model="gpt-4o",status="200"} 2`,
		`gateway_requests_total{code="upstream_error",model="claude-opus-5-5",status="502"} 1`,
		`gateway_requests_total{code="invalid_api_key",model="unknown",status="401"} 1`,
		`gateway_request_duration_seconds_count{model="gpt-4o"} 2`,
		`gateway_request_duration_seconds_sum{model="gpt-4o"} 2.3`,
		`gateway_request_duration_seconds_bucket{model="gpt-4o",le="0.5"} 1`,
		`gateway_request_duration_seconds_count{model="unknown"} 1`,
	} {
		if !strings.Contains(text, want) {
			t.Errorf("metrics do not contain %s:\n%s", want, text)
		}
	}
	if !strings.Contains(text, "go_goroutines") {
		t.Error("metrics do not include the Go runtime metrics")
	}
}

func TestModelLabelsAreBounded(t *testing.T) {
	m := newTestMetrics(t)
	for _, model := range []string{"gpt-5", "GPT-4O", "gpt-4o ", "claude", "../../etc/passwd", strings.Repeat("x", 1000), UnknownModel} {
		m.ObserveRequest(model, http.StatusNotFound, "model_not_found", time.Millisecond)
	}

	text := scrape(t, m)
	got := seriesLines(text, "gateway_requests_total")
	want := `gateway_requests_total{code="model_not_found",model="unknown",status="404"} 7`
	if len(got) != 1 || got[0] != want {
		t.Errorf("request series = %q, want only %s", got, want)
	}
	if n := len(seriesLines(text, "gateway_request_duration_seconds_count")); n != 1 {
		t.Errorf("got %d duration series, want 1", n)
	}
}

type providerFunc func(ctx context.Context, req llm.ChatRequest) (llm.ChatResponse, error)

func (f providerFunc) Chat(ctx context.Context, req llm.ChatRequest) (llm.ChatResponse, error) {
	return f(ctx, req)
}

func returning(err error) llm.Provider {
	return providerFunc(func(context.Context, llm.ChatRequest) (llm.ChatResponse, error) {
		return llm.ChatResponse{}, err
	})
}

func TestInstrumentCountsEachAttempt(t *testing.T) {
	m := newTestMetrics(t)
	for _, err := range []error{
		nil,
		nil,
		&llm.ProviderError{Provider: "openai", StatusCode: http.StatusServiceUnavailable},
		fmt.Errorf("after 2 attempts: %w", &llm.ProviderError{Provider: "openai", StatusCode: http.StatusServiceUnavailable}),
		&llm.ProviderError{Provider: "openai", Err: errors.New("connection refused")},
		&llm.ProviderError{Provider: "openai", Err: context.DeadlineExceeded},
		&llm.ProviderError{Provider: "openai", Err: context.Canceled},
	} {
		_, _ = m.Instrument("openai", returning(err)).Chat(context.Background(), llm.ChatRequest{})
	}

	text := scrape(t, m)
	for _, want := range []string{
		`gateway_provider_requests_total{outcome="success",provider="openai"} 2`,
		`gateway_provider_requests_total{outcome="error",provider="openai"} 3`,
		`gateway_provider_requests_total{outcome="timeout",provider="openai"} 1`,
		`gateway_provider_requests_total{outcome="canceled",provider="openai"} 1`,
		`gateway_provider_errors_total{provider="openai",upstream_status="503"} 2`,
		`gateway_provider_errors_total{provider="openai",upstream_status="0"} 1`,
		`gateway_provider_request_duration_seconds_count{provider="openai"} 7`,
	} {
		if !strings.Contains(text, want) {
			t.Errorf("metrics do not contain %s", want)
		}
	}
}

func TestInstrumentStartsProviderSeriesAtZero(t *testing.T) {
	m := newTestMetrics(t)
	m.Instrument("anthropic", returning(nil))
	m.AddFallbackRoute("openai", "anthropic")

	text := scrape(t, m)
	for _, want := range []string{
		`gateway_provider_requests_total{outcome="success",provider="anthropic"} 0`,
		`gateway_provider_requests_total{outcome="error",provider="anthropic"} 0`,
		`gateway_provider_retries_total{provider="anthropic"} 0`,
		`gateway_provider_circuit_state{provider="anthropic"} 0`,
		`gateway_provider_fallbacks_total{from_provider="openai",to_provider="anthropic"} 0`,
	} {
		if !strings.Contains(text, want) {
			t.Errorf("metrics do not contain %s", want)
		}
	}
}

func TestRetryFallbackAndCircuitMetrics(t *testing.T) {
	m := newTestMetrics(t)
	m.ObserveRetry("openai")
	m.ObserveRetry("openai")
	m.ObserveFallback("openai", "anthropic")

	for _, tt := range []struct {
		state breaker.State
		want  string
	}{
		{breaker.Open, "2"},
		{breaker.HalfOpen, "1"},
		{breaker.Closed, "0"},
	} {
		m.SetCircuitState("openai", tt.state)
		if want := `gateway_provider_circuit_state{provider="openai"} ` + tt.want; !strings.Contains(scrape(t, m), want) {
			t.Errorf("after %v, metrics do not contain %s", tt.state, want)
		}
	}
	text := scrape(t, m)
	for _, want := range []string{
		`gateway_provider_retries_total{provider="openai"} 2`,
		`gateway_provider_fallbacks_total{from_provider="openai",to_provider="anthropic"} 1`,
	} {
		if !strings.Contains(text, want) {
			t.Errorf("metrics do not contain %s", want)
		}
	}
}

func TestNilMetricsDoNothing(t *testing.T) {
	var m *Metrics
	p := returning(nil)
	if m.Instrument("openai", p) == nil {
		t.Fatal("Instrument() on nil Metrics returned nil")
	}
	m.ObserveRequest("gpt-4o", http.StatusOK, "", time.Second)
	m.ObserveRetry("openai")
	m.ObserveFallback("openai", "anthropic")
	m.AddFallbackRoute("openai", "anthropic")
	m.SetCircuitState("openai", breaker.Open)
	rec := httptest.NewRecorder()
	m.Handler(slog.New(slog.DiscardHandler)).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if rec.Code != http.StatusNotFound {
		t.Errorf("nil Metrics handler status = %d, want 404", rec.Code)
	}
}
