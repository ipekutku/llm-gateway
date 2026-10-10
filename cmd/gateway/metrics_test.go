package main

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/ipekutku/llm-gateway/internal/metrics"
)

// scrapeMetrics returns the metrics text served by h.
func scrapeMetrics(t *testing.T, h http.Handler) string {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("metrics status = %d", rec.Code)
	}
	return rec.Body.String()
}

func TestRequestPathMetrics(t *testing.T) {
	oa := newUpstream(t, "/v1/chat/completions", reply(http.StatusOK, openaiReply))
	an := newUpstream(t, "/v1/messages", reply(http.StatusInternalServerError, anthropicReply))
	cfg := gatewayConfig(oa, an)
	m, err := metrics.New(configuredModels(cfg))
	if err != nil {
		t.Fatalf("metrics.New() error = %v", err)
	}
	h, err := newHandler(cfg, nil, discardRecorder{}, m, nil, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatalf("newHandler() error = %v", err)
	}
	gw := httptest.NewServer(h)
	t.Cleanup(gw.Close)

	ctx := context.Background()
	for range 2 {
		postChat(t, ctx, gw.URL, chatBody("gpt-4o"))
	}
	postChat(t, ctx, gw.URL, chatBody("claude-opus-5-5"))
	// Arbitrary model names, an unauthenticated request, and an invalid
	// body are all labeled model="unknown".
	postChat(t, ctx, gw.URL, chatBody("gpt-5"))
	postChat(t, ctx, gw.URL, chatBody("client-chosen-model-name"))
	postChatAs(t, ctx, gw.URL, "Bearer not-a-gateway-key", chatBody("gpt-4o"))
	postChat(t, ctx, gw.URL, `{"model":"gpt-4o","messages":[]}`)

	text := scrapeMetrics(t, m.Handler(slog.New(slog.DiscardHandler)))
	want := []string{
		`gateway_requests_total{code="",model="gpt-4o",status="200"} 2`,
		`gateway_requests_total{code="invalid_api_key",model="unknown",status="401"} 1`,
		`gateway_requests_total{code="invalid_request",model="unknown",status="400"} 1`,
		`gateway_requests_total{code="model_not_found",model="unknown",status="404"} 2`,
		`gateway_requests_total{code="upstream_error",model="claude-opus-5-5",status="502"} 1`,
	}
	var got []string
	for line := range strings.Lines(text) {
		if strings.HasPrefix(line, "gateway_requests_total{") {
			got = append(got, strings.TrimSpace(line))
		}
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("request counters:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	for _, want := range []string{
		`gateway_request_duration_seconds_count{model="gpt-4o"} 2`,
		`gateway_request_duration_seconds_count{model="claude-opus-5-5"} 1`,
		`gateway_request_duration_seconds_count{model="unknown"} 4`,
	} {
		if !strings.Contains(text, want) {
			t.Errorf("metrics do not contain %s", want)
		}
	}
	for _, secret := range []string{clientKey, "not-a-gateway-key", "client-chosen-model-name", "team-a", "Be brief."} {
		if strings.Contains(text, secret) {
			t.Errorf("metrics contain %q", secret)
		}
	}
}

var listenAddrs = regexp.MustCompile(`addr=(\S+) metrics_addr=(\S+)`)

func TestRunServesMetricsOnSeparateListener(t *testing.T) {
	var logs syncBuffer
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	result := make(chan error, 1)
	go func() {
		result <- runWithFakeDatabase(ctx, map[string]string{
			"ANTHROPIC_MODEL": "claude-opus-5-5", "ANTHROPIC_API_KEY": anthropicKey,
			"GATEWAY_ADDR": "127.0.0.1:0", "GATEWAY_METRICS_ADDR": "127.0.0.1:0", clientsFileVar: writeClientsFile(t),
		}, slog.New(slog.NewTextHandler(&logs, nil)))
	}()

	deadline := time.After(guard)
	var match []string
	for match == nil {
		select {
		case err := <-result:
			t.Fatalf("run() returned early: %v", err)
		case <-deadline:
			t.Fatal("timed out waiting for the gateway to listen")
		case <-time.After(10 * time.Millisecond):
		}
		match = listenAddrs.FindStringSubmatch(logs.String())
	}
	api, metricsURL := "http://"+match[1], "http://"+match[2]

	// One rejected request, then a scrape without a gateway key.
	if resp, _, err := postChatAs(t, ctx, api, "", chatBody("claude-opus-5-5")); err != nil || resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("unauthenticated request = %v, %v; want 401", resp, err)
	}
	resp, err := http.Get(metricsURL + "/metrics")
	if err != nil {
		t.Fatalf("GET /metrics error = %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || !strings.Contains(string(body), `gateway_requests_total{code="missing_api_key",model="unknown",status="401"} 1`) {
		t.Errorf("GET /metrics = %d:\n%s", resp.StatusCode, body)
	}

	// The API port does not serve metrics, and the metrics port serves
	// nothing else.
	for _, url := range []string{api + "/metrics", metricsURL + "/v1/chat/completions"} {
		resp, err := http.Get(url)
		if err != nil {
			t.Fatalf("GET %s error = %v", url, err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusNotFound {
			t.Errorf("GET %s = %d, want 404", url, resp.StatusCode)
		}
	}

	cancel()
	select {
	case err := <-result:
		if err != nil {
			t.Errorf("run() error = %v, want nil", err)
		}
	case <-time.After(guard):
		t.Fatal("timed out waiting for run to return")
	}
	if _, err := http.Get(metricsURL + "/metrics"); err == nil {
		t.Error("metrics listener still accepts connections after shutdown")
	}
}

// metricsGateway is gatewayWith, recording metrics in the returned Metrics.
func metricsGateway(t *testing.T, oa, an *upstream, edit func(*config)) (*httptest.Server, *metrics.Metrics) {
	t.Helper()
	cfg := gatewayConfig(oa, an)
	edit(&cfg)
	m, err := metrics.New(configuredModels(cfg))
	if err != nil {
		t.Fatalf("metrics.New() error = %v", err)
	}
	h, err := newHandler(cfg, nil, discardRecorder{}, m, nil, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatalf("newHandler() error = %v", err)
	}
	gw := httptest.NewServer(h)
	t.Cleanup(gw.Close)
	return gw, m
}

func assertMetrics(t *testing.T, m *metrics.Metrics, want ...string) {
	t.Helper()
	text := scrapeMetrics(t, m.Handler(slog.New(slog.DiscardHandler)))
	for _, w := range want {
		if !strings.Contains(text, w+"\n") {
			t.Errorf("metrics do not contain %s", w)
		}
	}
	if t.Failed() {
		t.Logf("metrics:\n%s", text)
	}
}

func TestRequestPathProviderMetrics(t *testing.T) {
	t.Run("retries then success", func(t *testing.T) {
		oa := newUpstream(t, "/v1/chat/completions", sequence(openaiReply, "", 503, 503, 200))
		an := newUpstream(t, "/v1/messages", reply(http.StatusOK, anthropicReply))
		gw, m := metricsGateway(t, oa, an, func(*config) {})

		postChat(t, context.Background(), gw.URL, chatBody("gpt-4o"))

		assertMetrics(t, m,
			`gateway_provider_requests_total{outcome="error",provider="openai"} 2`,
			`gateway_provider_requests_total{outcome="success",provider="openai"} 1`,
			`gateway_provider_errors_total{provider="openai",upstream_status="503"} 2`,
			`gateway_provider_request_duration_seconds_count{provider="openai"} 3`,
			`gateway_provider_retries_total{provider="openai"} 2`,
			`gateway_provider_requests_total{outcome="success",provider="anthropic"} 0`,
			`gateway_requests_total{code="",model="gpt-4o",status="200"} 1`,
		)
	})

	t.Run("retry exhaustion then fallback", func(t *testing.T) {
		oa := newUpstream(t, "/v1/chat/completions", reply(http.StatusServiceUnavailable, openaiReply))
		an := newUpstream(t, "/v1/messages", reply(http.StatusOK, anthropicReply))
		gw, m := metricsGateway(t, oa, an, func(c *config) { c.OpenAI.FallbackTo = "anthropic" })

		postChat(t, context.Background(), gw.URL, chatBody("gpt-4o"))

		attempts := fastRetry.MaxAttempts
		assertMetrics(t, m,
			fmt.Sprintf(`gateway_provider_requests_total{outcome="error",provider="openai"} %d`, attempts),
			fmt.Sprintf(`gateway_provider_errors_total{provider="openai",upstream_status="503"} %d`, attempts),
			fmt.Sprintf(`gateway_provider_retries_total{provider="openai"} %d`, attempts-1),
			`gateway_provider_fallbacks_total{from_provider="openai",to_provider="anthropic"} 1`,
			`gateway_provider_requests_total{outcome="success",provider="anthropic"} 1`,
			`gateway_provider_retries_total{provider="anthropic"} 0`,
			`gateway_requests_total{code="",model="gpt-4o",status="200"} 1`,
		)
	})

	t.Run("circuit opens", func(t *testing.T) {
		oa := newUpstream(t, "/v1/chat/completions", reply(http.StatusInternalServerError, openaiReply))
		an := newUpstream(t, "/v1/messages", reply(http.StatusOK, anthropicReply))
		gw, m := metricsGateway(t, oa, an, func(c *config) { c.Breaker.Failures = 1 })

		// A 500 is not retried and opens the circuit at once; the next
		// request is rejected without an attempt.
		postChat(t, context.Background(), gw.URL, chatBody("gpt-4o"))
		postChat(t, context.Background(), gw.URL, chatBody("gpt-4o"))
		assertMetrics(t, m,
			`gateway_provider_circuit_state{provider="openai"} 2`,
			`gateway_provider_requests_total{outcome="error",provider="openai"} 1`,
			`gateway_provider_errors_total{provider="openai",upstream_status="500"} 1`,
			`gateway_requests_total{code="provider_unavailable",model="gpt-4o",status="503"} 1`,
			`gateway_provider_circuit_state{provider="anthropic"} 0`,
		)
	})
}
