package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ipekutku/llm-gateway/internal/auth"
	"github.com/ipekutku/llm-gateway/internal/httpapi"
	"github.com/ipekutku/llm-gateway/internal/metrics"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
)

// Exercise all three telemetry outputs together, including failures that
// might accidentally copy an upstream response field into a diagnostic.
func TestObservabilityExcludesSensitiveData(t *testing.T) {
	const (
		prompt         = "PROMPT-MARKER-6c809a"
		completion     = "COMPLETION-MARKER-72d491"
		key            = "GATEWAY-KEY-MARKER-429f3a"
		upstreamSecret = "UPSTREAM-BODY-MARKER-157ade"
		baggage        = "BAGGAGE-MARKER-493fb0"
	)
	oaReply := strings.Replace(openaiReply, "from openai", completion, 1)
	anReply := strings.Replace(anthropicReply, "from anthropic", completion, 1)
	malformedHTTP := func(w http.ResponseWriter, r *http.Request) {
		conn, rw, err := w.(http.Hijacker).Hijack()
		if err != nil {
			t.Error(err)
			return
		}
		defer conn.Close()
		_, _ = fmt.Fprintf(rw, "HTTP/1.1 %s invalid\r\n\r\n", upstreamSecret)
		_ = rw.Flush()
	}
	for _, format := range []string{"text", "json"} {
		t.Run(format, func(t *testing.T) {
			for _, tt := range []struct {
				name, model, body, authorization string
				oa, an                           http.HandlerFunc
				fallback                         bool
				status, attempts                 int
			}{
				{name: "openai success", oa: reply(200, oaReply), status: 200, attempts: 1},
				{name: "anthropic success", model: "claude-opus-5-5", status: 200, attempts: 1},
				{name: "retry", oa: sequence(oaReply, "", 503, 200), status: 200, attempts: 2},
				{name: "retry exhaustion and fallback", oa: reply(503, upstreamSecret), fallback: true, status: 200, attempts: 4},
				{name: "upstream failure", oa: reply(500, upstreamSecret), status: 502, attempts: 1},
				{name: "malformed upstream JSON", oa: reply(200, upstreamSecret), status: 502, attempts: 1},
				{name: "openai malformed HTTP", oa: malformedHTTP, status: 502, attempts: 1},
				{name: "anthropic malformed HTTP", model: "claude-opus-5-5", an: malformedHTTP, status: 502, attempts: 1},
				{name: "openai role", oa: reply(200, strings.Replace(oaReply, "assistant", upstreamSecret, 1)), status: 502, attempts: 1},
				{name: "openai finish reason", oa: reply(200, strings.Replace(oaReply, `"stop"`, `"`+upstreamSecret+`"`, 1)), status: 502, attempts: 1},
				{name: "anthropic role", model: "claude-opus-5-5", an: reply(200, strings.Replace(anReply, "assistant", upstreamSecret, 1)), status: 502, attempts: 1},
				{name: "anthropic response type", model: "claude-opus-5-5", an: reply(200, strings.Replace(anReply, `"message"`, `"`+upstreamSecret+`"`, 1)), status: 502, attempts: 1},
				{name: "anthropic content type", model: "claude-opus-5-5", an: reply(200, strings.Replace(anReply, `"thinking"`, `"`+upstreamSecret+`"`, 1)), status: 502, attempts: 1},
				{name: "anthropic stop reason", model: "claude-opus-5-5", an: reply(200, strings.Replace(anReply, "max_tokens", upstreamSecret, 1)), status: 502, attempts: 1},
				{name: "unknown key", authorization: "Bearer invalid-" + key, status: 401},
				{name: "malformed authorization", authorization: "Token " + key, status: 401},
				{name: "invalid body", body: `{"model":"gpt-4o","messages":[{"role":"` + prompt + `","content":"` + prompt + `"}]}`, status: 400},
				{name: "unknown model", model: "unconfigured-model", status: 404},
			} {
				t.Run(tt.name, func(t *testing.T) {
					if tt.oa == nil {
						tt.oa = reply(200, oaReply)
					}
					if tt.an == nil {
						tt.an = reply(200, anReply)
					}
					oa := newUpstream(t, "/v1/chat/completions", tt.oa)
					an := newUpstream(t, "/v1/messages", tt.an)
					cfg := gatewayConfig(oa, an)
					cfg.Clients = []auth.Client{{ID: "team-a", KeyHash: auth.HashKey(key)}}
					if tt.fallback {
						cfg.OpenAI.FallbackTo = "anthropic"
					}
					m, err := metrics.New(configuredModels(cfg))
					if err != nil {
						t.Fatal(err)
					}
					logs := new(syncBuffer)
					logger, err := newLogger(logs, env(map[string]string{logFormatVar: format}))
					if err != nil {
						t.Fatal(err)
					}
					exporter := tracetest.NewInMemoryExporter()
					tp := sdktrace.NewTracerProvider(sdktrace.WithSyncer(exporter), sdktrace.WithSampler(sdktrace.AlwaysSample()))
					t.Cleanup(func() { _ = tp.Shutdown(context.Background()) })
					h, err := newHandler(cfg, nil, discardRecorder{}, m, tp, logger)
					if err != nil {
						t.Fatal(err)
					}
					body := tt.body
					if body == "" {
						body = `{"model":"` + orDefault(tt.model, "gpt-4o") + `","messages":[{"role":"user","content":"` + prompt + `"}]}`
					}
					req := httptest.NewRequest(http.MethodPost, httpapi.ChatCompletionsPath, strings.NewReader(body))
					req.Header.Set("Authorization", orDefault(tt.authorization, "Bearer "+key))
					req.Header.Set("baggage", "private="+baggage)
					req.Header.Set(httpapi.RequestIDHeader, key)
					resp := httptest.NewRecorder()
					h.ServeHTTP(resp, req) // Returns after logs, metrics, and spans are complete.
					if resp.Code != tt.status {
						t.Fatalf("status = %d, want %d", resp.Code, tt.status)
					}
					if tt.status == 200 && !strings.Contains(resp.Body.String(), completion) {
						t.Fatal("successful response did not carry the marker completion")
					}
					received := append(oa.received(), an.received()...)
					if len(received) != tt.attempts {
						t.Fatalf("attempts = %d, want %d", len(received), tt.attempts)
					}
					for _, r := range received {
						b, err := json.Marshal(r.Body)
						if err != nil || !strings.Contains(string(b), prompt) {
							t.Fatal("upstream did not receive the marker prompt")
						}
						if strings.Contains(fmt.Sprint(r.Header), key) {
							t.Error("gateway key reached an upstream")
						}
					}
					spans := exporter.GetSpans()
					if len(spans) == 0 {
						t.Fatal("no spans exported")
					}
					spanJSON, err := json.Marshal(spans)
					if err != nil {
						t.Fatal(err)
					}
					metricText := scrapeMetrics(t, m.Handler(slog.New(slog.DiscardHandler)))
					if !strings.Contains(metricText, "gateway_requests_total{") {
						t.Fatal("request metric missing")
					}
					if id := resp.Header().Get(httpapi.RequestIDHeader); id == "" || !strings.Contains(logs.String(), id) {
						t.Fatal("request log missing")
					}
					for surface, output := range map[string]string{"logs": logs.String(), "metrics": metricText, "exported spans": string(spanJSON)} {
						for _, secret := range []string{prompt, completion, key, fmt.Sprintf("%x", auth.HashKey(key)), openaiKey, anthropicKey, upstreamSecret, baggage} {
							if strings.Contains(output, secret) {
								t.Errorf("%s contains sensitive marker %q", surface, secret)
							}
						}
					}
				})
			}
		})
	}
}
