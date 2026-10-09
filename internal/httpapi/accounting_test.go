package httpapi

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

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
			h, err := New(p, testAuthenticator(t), testLimiter(t, map[string]ratelimit.Limits{"team-a": generous}), testTimeout, Accounting{Recorder: recordFunc(func(r usage.Record) bool { got = r; return true }), Pricing: prices, Models: models}, slog.New(slog.DiscardHandler))
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
