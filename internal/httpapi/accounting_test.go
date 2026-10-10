package httpapi

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/ipekutku/llm-gateway/internal/llm"
	"github.com/ipekutku/llm-gateway/internal/ratelimit"
	"github.com/ipekutku/llm-gateway/internal/usage"
)

type recordFunc func(usage.Record) bool

func (f recordFunc) Record(r usage.Record) bool { return f(r) }

type failedWriter struct{ header http.Header }

func (w failedWriter) Header() http.Header       { return w.header }
func (w failedWriter) WriteHeader(int)           {}
func (w failedWriter) Write([]byte) (int, error) { return 0, errors.New("client disconnected") }

func TestAccountingRetainsUsageWhenClientCannotReceiveSuccess(t *testing.T) {
	for _, canceled := range []bool{false, true} {
		t.Run(map[bool]string{false: "write failure", true: "cancellation"}[canceled], func(t *testing.T) {
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			p := providerFunc(func(context.Context, llm.ChatRequest) (llm.ChatResponse, error) {
				if canceled {
					cancel()
				}
				return llm.ChatResponse{Provider: "provider", Model: "snapshot", Message: llm.Message{Role: llm.RoleAssistant, Content: "answer"}, FinishReason: llm.FinishReasonStop, Usage: llm.Usage{InputTokens: 5}}, nil
			})
			prices, _ := usage.NewPricing(map[usage.Model]usage.Price{{Provider: "provider", Model: "configured"}: {Input: 1_000_000}})
			var got usage.Record
			models := map[string]string{"provider": "configured"}
			h, err := New(p, testAuthenticator(t), testLimiter(t, map[string]ratelimit.Limits{"team-a": generous}), testTimeout, Accounting{Recorder: recordFunc(func(r usage.Record) bool { got = r; return true }), Pricing: prices, Models: models}, nil, slog.New(slog.DiscardHandler))
			if err != nil {
				t.Fatal(err)
			}
			models["provider"] = "mutated" // Handler must own its configuration.
			r := httptest.NewRequest("POST", ChatCompletionsPath, strings.NewReader(`{"model":"configured","messages":[{"role":"user","content":"hi"}]}`)).WithContext(ctx)
			r.Header.Set("Authorization", "Bearer "+keyA)
			var w http.ResponseWriter = httptest.NewRecorder()
			if !canceled {
				w = failedWriter{header: make(http.Header)}
			}
			h.ServeHTTP(w, r)
			if got.Status != usage.StatusClientClosed || got.ErrorCode != "client_closed" || got.Usage == nil || got.Usage.InputTokens != 5 || got.Cost == nil || *got.Cost != 5_000_000 {
				t.Errorf("record = %+v", got)
			}
		})
	}
}

type usageObservation struct {
	provider, model string
	usage           llm.Usage
	cost            *usage.Cost
}

// metricsSpy records ObserveUsage calls.
type metricsSpy struct{ usage []usageObservation }

func (*metricsSpy) ObserveRequest(string, int, string, time.Duration) {}
func (m *metricsSpy) ObserveUsage(provider, model string, u llm.Usage, cost *usage.Cost) {
	m.usage = append(m.usage, usageObservation{provider, model, u, cost})
}

func TestUsageMetricsMatchUsageRecord(t *testing.T) {
	for _, tt := range []struct {
		name    string
		resp    llm.ChatResponse
		err     error
		observe bool
	}{
		{"success", llm.ChatResponse{Provider: "provider", Model: "snapshot", FinishReason: llm.FinishReasonStop, Usage: llm.Usage{InputTokens: 5, CacheReadInputTokens: 2, OutputTokens: 1}}, nil, true},
		{"upstream failure", llm.ChatResponse{}, &llm.ProviderError{Provider: "provider", StatusCode: http.StatusServiceUnavailable}, false},
		// The recorder drops a record with inconsistent usage, so metrics
		// skip it too.
		{"inconsistent usage", llm.ChatResponse{Provider: "provider", Model: "snapshot", FinishReason: llm.FinishReasonStop, Usage: llm.Usage{InputTokens: 1, CacheReadInputTokens: 2}}, nil, false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			p := providerFunc(func(context.Context, llm.ChatRequest) (llm.ChatResponse, error) { return tt.resp, tt.err })
			prices, _ := usage.NewPricing(map[usage.Model]usage.Price{{Provider: "provider", Model: "configured"}: {Input: 1_000_000}})
			var record usage.Record
			spy := &metricsSpy{}
			accounting := Accounting{Recorder: recordFunc(func(r usage.Record) bool { record = r; return true }), Pricing: prices, Models: map[string]string{"provider": "configured"}}
			h, err := New(p, testAuthenticator(t), testLimiter(t, map[string]ratelimit.Limits{"team-a": generous}), testTimeout, accounting, spy, slog.New(slog.DiscardHandler))
			if err != nil {
				t.Fatal(err)
			}

			post(t, h, `{"model":"configured","messages":[{"role":"user","content":"hi"}]}`)

			if !tt.observe {
				if len(spy.usage) != 0 {
					t.Errorf("observed usage %+v, want none", spy.usage)
				}
				return
			}
			if len(spy.usage) != 1 {
				t.Fatalf("observed usage %d times, want 1", len(spy.usage))
			}
			got := spy.usage[0]
			if got.provider != "provider" || got.model != "configured" || got.usage != *record.Usage || got.cost == nil || *got.cost != *record.Cost {
				t.Errorf("observed %+v, want the record's provider, configured model, usage %+v, and cost %v", got, *record.Usage, record.Cost)
			}
		})
	}
}
