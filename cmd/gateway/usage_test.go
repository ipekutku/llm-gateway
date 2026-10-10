package main

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/ipekutku/llm-gateway/internal/httpapi"
	"github.com/ipekutku/llm-gateway/internal/ratelimit"
	"github.com/ipekutku/llm-gateway/internal/usage"
)

type usageCapture struct {
	records chan usage.Record
	accept  bool
}

func (c *usageCapture) Record(r usage.Record) bool { c.records <- r; return c.accept }

func receiveUsage(t *testing.T, c *usageCapture) usage.Record {
	t.Helper()
	select {
	case r := <-c.records:
		if err := r.Validate(); err != nil {
			t.Fatalf("invalid accounting record: %v; %+v", err, r)
		}
		return r
	case <-time.After(guard):
		t.Fatal("timed out waiting for accounting record")
		return usage.Record{}
	}
}

func usageGateway(t *testing.T, oa, an *upstream, edit func(*config)) (*httptest.Server, *usageCapture) {
	t.Helper()
	cfg := gatewayConfig(oa, an)
	prices, err := usage.NewPricing(map[usage.Model]usage.Price{
		{Provider: "openai", Model: "gpt-4o"}:             {Input: 2_500_000, CacheRead: 1_250_000, Output: 10_000_000},
		{Provider: "anthropic", Model: "claude-opus-5-5"}: {Input: 1_000_000, CacheRead: 100_000, CacheWrite: 2_000_000, Output: 5_000_000},
	})
	if err != nil {
		t.Fatal(err)
	}
	cfg.Pricing = prices
	edit(&cfg)
	capture := &usageCapture{records: make(chan usage.Record, 100), accept: true}
	h, err := newHandler(cfg, nil, capture, nil, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	gw := httptest.NewServer(h)
	t.Cleanup(gw.Close)
	return gw, capture
}

func TestUsageRecordsSuccessAndFallback(t *testing.T) {
	for _, fallback := range []bool{false, true} {
		t.Run(map[bool]string{false: "direct", true: "fallback"}[fallback], func(t *testing.T) {
			oaStatus := http.StatusOK
			if fallback {
				oaStatus = http.StatusServiceUnavailable
			}
			oaBody := strings.Replace(openaiReply, `"prompt_tokens":12`, `"prompt_tokens":12,"prompt_tokens_details":{"cached_tokens":4}`, 1)
			anBody := strings.Replace(anthropicReply, `"input_tokens":7`, `"input_tokens":7,"cache_read_input_tokens":3,"cache_creation_input_tokens":2`, 1)
			oa := newUpstream(t, "/v1/chat/completions", reply(oaStatus, oaBody))
			an := newUpstream(t, "/v1/messages", reply(http.StatusOK, anBody))
			gw, c := usageGateway(t, oa, an, func(cfg *config) {
				if fallback {
					cfg.OpenAI.FallbackTo = "anthropic"
				}
			})
			before := time.Now()
			resp, body, err := postChat(t, t.Context(), gw.URL, chatBody("gpt-4o"))
			if err != nil || resp.StatusCode != http.StatusOK {
				t.Fatalf("POST = %v, %s", err, body)
			}
			r := receiveUsage(t, c)
			if r.RequestID != resp.Header.Get(httpapi.RequestIDHeader) || r.ClientID != "team-a" || r.RequestedModel != "gpt-4o" || r.Status != 200 || r.ErrorCode != "" || r.Time.Before(before) || r.Duration < 0 {
				t.Errorf("metadata = %+v", r)
			}
			if fallback {
				if r.Provider != "anthropic" || r.Model != "claude-opus-5-5" || r.Cost == nil || *r.Cost != usage.Cost(31_300_000) || r.Usage.InputTokens != 12 || r.Usage.CacheWriteInputTokens != 2 {
					t.Errorf("fallback record = %+v", r)
				}
			} else {
				if r.Provider != "openai" || r.Model != "gpt-4o-2024-08-06" || r.Cost == nil || *r.Cost != usage.Cost(55_000_000) || r.Usage.CacheReadInputTokens != 4 {
					t.Errorf("direct record = %+v", r)
				}
			}
			if len(c.records) != 0 {
				t.Error("request produced more than one record")
			}
		})
	}
}

func TestUsageRecordsFailures(t *testing.T) {
	for _, tt := range []struct {
		name, model, code      string
		upstreamStatus, status int
	}{
		{"unknown model", "unknown", "model_not_found", 200, 404},
		{"rejected upstream", "gpt-4o", "invalid_request", 400, 400},
		{"failed retries", "gpt-4o", "upstream_error", 503, 502},
		{"provider limited", "gpt-4o", "provider_rate_limited", 429, 429},
	} {
		t.Run(tt.name, func(t *testing.T) {
			oa := newUpstream(t, "/v1/chat/completions", reply(tt.upstreamStatus, ""))
			an := newUpstream(t, "/v1/messages", reply(http.StatusOK, anthropicReply))
			gw, c := usageGateway(t, oa, an, func(*config) {})
			resp, _, err := postChat(t, t.Context(), gw.URL, chatBody(tt.model))
			if err != nil {
				t.Fatal(err)
			}
			r := receiveUsage(t, c)
			if resp.StatusCode != tt.status || r.Status != tt.status || r.ErrorCode != tt.code || r.Usage != nil || r.Cost != nil || r.Model != "" {
				t.Errorf("failure record = %+v", r)
			}
			if tt.model == "unknown" {
				if r.Provider != "" {
					t.Errorf("provider = %q", r.Provider)
				}
			} else if r.Provider != "openai" {
				t.Errorf("provider = %q", r.Provider)
			}
			if len(c.records) != 0 {
				t.Error("failed attempts produced extra records")
			}
		})
	}
}

func TestUsageRecordsTimeoutAndClientCancellation(t *testing.T) {
	for _, disconnect := range []bool{false, true} {
		t.Run(map[bool]string{false: "timeout", true: "client cancellation"}[disconnect], func(t *testing.T) {
			slow := newBlockingHandler(t, "")
			oa := newUpstream(t, "/v1/chat/completions", slow.ServeHTTP)
			an := newUpstream(t, "/v1/messages", reply(http.StatusOK, anthropicReply))
			gw, c := usageGateway(t, oa, an, func(cfg *config) {
				if !disconnect {
					cfg.UpstreamTimeout = 30 * time.Millisecond
				}
			})
			t.Cleanup(slow.Release)
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			finished := make(chan struct{})
			go func() { defer close(finished); _, _, _ = postChat(t, ctx, gw.URL, chatBody("gpt-4o")) }()
			waitFor(t, slow.started, "the upstream to start")
			if disconnect {
				cancel()
			}
			r := receiveUsage(t, c)
			waitFor(t, finished, "client request to finish")
			status, code := 504, "upstream_timeout"
			if disconnect {
				status, code = usage.StatusClientClosed, "client_closed"
			}
			if r.Status != status || r.ErrorCode != code || r.Provider != "openai" || r.Usage != nil || r.Cost != nil {
				t.Errorf("record = %+v", r)
			}
		})
	}
}

func TestUsageExcludesPreValidationRejections(t *testing.T) {
	oa := newUpstream(t, "/v1/chat/completions", reply(http.StatusOK, openaiReply))
	an := newUpstream(t, "/v1/messages", reply(http.StatusOK, anthropicReply))
	gw, c := usageGateway(t, oa, an, func(cfg *config) {
		cfg.RateLimits = map[string]ratelimit.Limits{"team-a": {RequestsPerMinute: 1, Burst: 1, MaxConcurrent: 1}}
	})
	for _, tt := range []struct {
		key, body string
		status    int
	}{
		{"", chatBody("gpt-4o"), 401},
		{"Bearer " + clientKey, `{"messages":[]}`, 400},
		{"Bearer " + clientKey, chatBody("gpt-4o"), 429},
	} {
		resp, _, err := postChatAs(t, t.Context(), gw.URL, tt.key, tt.body)
		if err != nil || resp.StatusCode != tt.status {
			t.Fatalf("rejection = %v, expected %d", err, tt.status)
		}
	}
	if len(c.records) != 0 || len(oa.received()) != 0 {
		t.Error("pre-validation rejection was recorded or called a provider")
	}
}

func TestUsageUnknownCostAndDroppedRecordDoNotFailRequest(t *testing.T) {
	oa := newUpstream(t, "/v1/chat/completions", reply(http.StatusOK, openaiReply))
	an := newUpstream(t, "/v1/messages", reply(http.StatusOK, anthropicReply))
	gw, c := usageGateway(t, oa, an, func(cfg *config) { cfg.Pricing = testPricing })
	c.accept = false
	resp, _, err := postChat(t, t.Context(), gw.URL, chatBody("gpt-4o"))
	if err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("POST = %v", err)
	}
	r := receiveUsage(t, c)
	if r.Usage == nil || r.Cost != nil {
		t.Errorf("unpriced record = %+v", r)
	}
}

type usageBatchStore struct {
	batches chan []usage.Record
	err     error
}

func (s *usageBatchStore) Insert(ctx context.Context, records []usage.Record) error {
	select {
	case s.batches <- append([]usage.Record(nil), records...):
		return s.err
	case <-ctx.Done():
		return ctx.Err()
	}
}

func TestDatabaseWriteFailureDoesNotFailCompletion(t *testing.T) {
	store := &usageBatchStore{batches: make(chan []usage.Record, 1), err: errors.New("database down")}
	var logs syncBuffer
	logger := slog.New(slog.NewTextHandler(&logs, nil))
	opts := usage.DefaultRecorderOptions()
	opts.BatchSize = 1
	recorder, err := usage.NewRecorder(store, logger, opts)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), guard)
		defer cancel()
		_ = recorder.Close(ctx)
	})
	oa := newUpstream(t, "/v1/chat/completions", reply(200, openaiReply))
	an := newUpstream(t, "/v1/messages", reply(200, anthropicReply))
	h, err := newHandler(gatewayConfig(oa, an), nil, recorder, nil, logger)
	if err != nil {
		t.Fatal(err)
	}
	gw := httptest.NewServer(h)
	t.Cleanup(gw.Close)
	resp, _, err := postChat(t, t.Context(), gw.URL, chatBody("gpt-4o"))
	if err != nil || resp.StatusCode != 200 {
		t.Fatalf("POST = %v", err)
	}
	select {
	case <-store.batches:
	case <-time.After(guard):
		t.Fatal("writer did not attempt the insert")
	}
	ctx, cancel := context.WithTimeout(t.Context(), guard)
	defer cancel()
	if err := recorder.Close(ctx); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(logs.String(), "usage records not persisted") {
		t.Errorf("missing failure log: %s", logs.String())
	}
}

func TestHTTPShutdownRecordsCanceledRequestsBeforeDraining(t *testing.T) {
	for _, serveFailure := range []bool{false, true} {
		t.Run(map[bool]string{false: "forced shutdown", true: "serve failure"}[serveFailure], func(t *testing.T) {
			store := &usageBatchStore{batches: make(chan []usage.Record, 1)}
			logger := slog.New(slog.DiscardHandler)
			recorder, err := usage.NewRecorder(store, logger, usage.DefaultRecorderOptions())
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				ctx, cancel := context.WithTimeout(context.Background(), guard)
				defer cancel()
				_ = recorder.Close(ctx)
			})
			slow := newBlockingHandler(t, "")
			oa := newUpstream(t, "/v1/chat/completions", slow.ServeHTTP)
			an := newUpstream(t, "/v1/messages", reply(200, anthropicReply))
			h, err := newHandler(gatewayConfig(oa, an), nil, recorder, nil, logger)
			if err != nil {
				t.Fatal(err)
			}
			active := &activeHandlers{next: h}
			srv := newServer(active, logger)
			ln, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithCancel(t.Context())
			t.Cleanup(func() { cancel(); _ = srv.Close(); slow.Release() })
			stopped := make(chan error, 1)
			go func() { stopped <- serve(ctx, srv, ln, 20*time.Millisecond) }()
			clientDone := make(chan struct{})
			go func() {
				defer close(clientDone)
				_, _, _ = postChat(t, t.Context(), "http://"+ln.Addr().String(), chatBody("gpt-4o"))
			}()
			waitFor(t, slow.started, "the upstream request")
			if serveFailure {
				_ = ln.Close()
			} else {
				cancel()
			}
			select {
			case err := <-stopped:
				if err == nil || (!serveFailure && !errors.Is(err, context.DeadlineExceeded)) {
					t.Fatalf("forced shutdown = %v", err)
				}
			case <-time.After(guard):
				t.Fatal("HTTP shutdown did not finish")
			}
			drainCtx, drainCancel := context.WithTimeout(t.Context(), guard)
			defer drainCancel()
			if err := active.stop(drainCtx); err != nil {
				t.Fatal(err)
			}
			if err := recorder.Close(drainCtx); err != nil {
				t.Fatal(err)
			}
			select {
			case batch := <-store.batches:
				if len(batch) != 1 || batch[0].Status != usage.StatusClientClosed || batch[0].ErrorCode != "client_closed" {
					t.Errorf("shutdown batch = %+v", batch)
				}
			case <-time.After(guard):
				t.Fatal("canceled record was lost on shutdown")
			}
			waitFor(t, clientDone, "client to finish")
		})
	}
}
