package httpapi_test

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/ipekutku/llm-gateway/internal/auth"
	"github.com/ipekutku/llm-gateway/internal/httpapi"
	"github.com/ipekutku/llm-gateway/internal/llm"
	"github.com/ipekutku/llm-gateway/internal/metrics"
	"github.com/ipekutku/llm-gateway/internal/ratelimit"
	"github.com/ipekutku/llm-gateway/internal/routing"
	"github.com/ipekutku/llm-gateway/internal/tracing"
	"github.com/ipekutku/llm-gateway/internal/usage"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
)

type benchmarkProvider struct{}

func (benchmarkProvider) Chat(_ context.Context, req llm.ChatRequest) (llm.ChatResponse, error) {
	return llm.ChatResponse{
		Provider: "openai", Model: req.Model,
		Message:      llm.Message{Role: llm.RoleAssistant, Content: "TCP delivers an ordered stream of bytes reliably."},
		FinishReason: llm.FinishReasonStop,
		Usage:        llm.Usage{InputTokens: 1000, CacheReadInputTokens: 600, OutputTokens: 200},
	}, nil
}

type benchmarkRecorder struct{}

func (benchmarkRecorder) Record(usage.Record) bool { return true }

// Keep only the last exported span so calibration cannot retain millions
// of spans. Synchronous in-memory export measures sampled instrumentation,
// without collector I/O, batching, or unbounded memory growth.
type benchmarkExporter struct {
	count int
	last  sdktrace.ReadOnlySpan
}

func (e *benchmarkExporter) ExportSpans(_ context.Context, spans []sdktrace.ReadOnlySpan) error {
	e.count += len(spans)
	if len(spans) > 0 {
		e.last = spans[len(spans)-1]
	}
	return nil
}

func (*benchmarkExporter) Shutdown(context.Context) error { return nil }

func BenchmarkHandler(b *testing.B) {
	for _, metricEnabled := range []bool{false, true} {
		for _, traceEnabled := range []bool{false, true} {
			name := fmt.Sprintf("metrics_%t/tracing_%t", metricEnabled, traceEnabled)
			b.Run(name, func(b *testing.B) {
				// Distribute requests over a fixed set of clients so even long
				// runs measure successful requests under real local limits.
				const clientCount = 256
				clients := make([]auth.Client, clientCount)
				keys := make([]string, clientCount)
				limits := make(map[string]ratelimit.Limits, clientCount)
				for i := range clients {
					id := fmt.Sprintf("bench-%d", i)
					keys[i] = fmt.Sprintf("synthetic-handler-benchmark-key-%032d", i)
					clients[i] = auth.Client{ID: id, KeyHash: auth.HashKey(keys[i])}
					limits[id] = ratelimit.Limits{RequestsPerMinute: ratelimit.MaxLimit, Burst: ratelimit.MaxLimit, MaxConcurrent: ratelimit.MaxLimit}
				}
				a, err := auth.New(clients)
				if err != nil {
					b.Fatal(err)
				}
				l, err := ratelimit.New(limits)
				if err != nil {
					b.Fatal(err)
				}
				pricing, err := usage.NewPricing(map[usage.Model]usage.Price{
					{Provider: "openai", Model: "bench-model"}: {Input: 2_500_000, CacheRead: 1_250_000, Output: 10_000_000},
				})
				if err != nil {
					b.Fatal(err)
				}
				var m *metrics.Metrics
				if metricEnabled {
					m, err = metrics.New([]string{"bench-model"})
					if err != nil {
						b.Fatal(err)
					}
				}
				var tracer *tracing.Tracer
				var exporter *benchmarkExporter
				if traceEnabled {
					exporter = &benchmarkExporter{}
					tp := sdktrace.NewTracerProvider(sdktrace.WithSampler(sdktrace.AlwaysSample()), sdktrace.WithSyncer(exporter))
					b.Cleanup(func() {
						ctx, cancel := context.WithTimeout(context.Background(), time.Second)
						defer cancel()
						if err := tp.Shutdown(ctx); err != nil {
							b.Error(err)
						}
					})
					tracer = tracing.New(tp)
				}
				provider := tracer.Provider("openai", m.Instrument("openai", tracer.Attempt("openai", benchmarkProvider{})))
				logger := slog.New(slog.DiscardHandler)
				router, err := routing.New(map[string]routing.Route{"bench-model": {Provider: provider}}, logger)
				if err != nil {
					b.Fatal(err)
				}
				h, err := httpapi.New(tracer.Route(router), a, l, time.Minute, 0,
					httpapi.Accounting{Recorder: benchmarkRecorder{}, Pricing: pricing, Models: map[string]string{"openai": "bench-model"}}, m, logger)
				if err != nil {
					b.Fatal(err)
				}
				h = tracer.Handler(h)
				const body = `{"model":"bench-model","messages":[{"role":"system","content":"Answer concisely."},{"role":"user","content":"Explain TCP."}],"max_tokens":200}`
				i := 0
				b.ReportAllocs()
				for b.Loop() {
					req := httptest.NewRequest(http.MethodPost, httpapi.ChatCompletionsPath, strings.NewReader(body))
					req.Header.Set("Authorization", "Bearer "+keys[i%clientCount])
					req.Header.Set("Content-Type", "application/json")
					w := httptest.NewRecorder()
					h.ServeHTTP(w, req)
					if w.Code != http.StatusOK {
						b.Fatalf("handler status %d", w.Code)
					}
					i++
				}
				if exporter != nil && (exporter.count != 4*b.N || exporter.last.Name() != "POST "+httpapi.ChatCompletionsPath) {
					b.Fatalf("exported %d spans, want %d with the server span last", exporter.count, 4*b.N)
				}
			})
		}
	}
}
