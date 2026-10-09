package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ipekutku/llm-gateway/internal/breaker"
	"github.com/ipekutku/llm-gateway/internal/retry"
)

// guard bounds how long a test waits for something that should happen
// promptly. It is a failure guard, not a synchronization mechanism.
const guard = 5 * time.Second

const (
	openaiReply = `{"model":"gpt-4o-2024-08-06","choices":[{"index":0,` +
		`"message":{"role":"assistant","content":"from openai"},"finish_reason":"stop"}],` +
		`"usage":{"prompt_tokens":12,"completion_tokens":3,"total_tokens":15}}`
	anthropicReply = `{"type":"message","role":"assistant","model":"claude-opus-5-5",` +
		`"content":[{"type":"thinking","thinking":"","signature":"s"},{"type":"text","text":"from anthropic"}],` +
		`"stop_reason":"max_tokens","usage":{"input_tokens":7,"output_tokens":4}}`
)

// upstream is a fake provider API that records the requests it receives.
type upstream struct {
	mu       sync.Mutex
	requests []upstreamRequest
	srv      *httptest.Server
}

type upstreamRequest struct {
	Path   string
	Header http.Header
	Body   map[string]any
}

func newUpstream(t *testing.T, path string, handler http.HandlerFunc) *upstream {
	t.Helper()
	u := &upstream{}
	mux := http.NewServeMux()
	mux.HandleFunc("POST "+path, func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		u.mu.Lock()
		u.requests = append(u.requests, upstreamRequest{Path: r.URL.Path, Header: r.Header.Clone(), Body: body})
		u.mu.Unlock()
		handler(w, r)
	})
	u.srv = httptest.NewServer(mux)
	t.Cleanup(u.srv.Close)
	return u
}

func (u *upstream) received() []upstreamRequest {
	u.mu.Lock()
	defer u.mu.Unlock()
	return append([]upstreamRequest(nil), u.requests...)
}

func reply(status int, body string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = io.WriteString(w, body)
	}
}

// gateway runs the real handler, router, and both real adapters, pointed
// at the given fake upstreams, behind an httptest.Server.
func gateway(t *testing.T, oa, an *upstream) *httptest.Server {
	t.Helper()
	return gatewayWith(t, oa, an, func(*config) {})
}

// fastRetry keeps retry waits negligible in tests that do not exercise them.
var fastRetry = retry.Policy{MaxAttempts: 3, BaseDelay: time.Millisecond, MaxDelay: time.Millisecond}

// gatewayWith is gateway with the configuration adjusted by edit. Retries
// use fastRetry unless edit changes them.
func gatewayWith(t *testing.T, oa, an *upstream, edit func(*config)) *httptest.Server {
	t.Helper()
	cfg, err := loadConfig(env(map[string]string{
		"OPENAI_MODEL": "gpt-4o", "OPENAI_API_KEY": openaiKey,
		"ANTHROPIC_MODEL": "claude-opus-5-5", "ANTHROPIC_API_KEY": anthropicKey,
	}))
	if err != nil {
		t.Fatalf("loadConfig() error = %v", err)
	}
	cfg.OpenAI.BaseURL = oa.srv.URL
	cfg.Anthropic.BaseURL = an.srv.URL
	cfg.Retry = fastRetry
	edit(&cfg)

	h, err := newHandler(cfg, nil, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatalf("newHandler() error = %v", err)
	}
	gw := httptest.NewServer(h)
	t.Cleanup(gw.Close)
	return gw
}

func postChat(t *testing.T, ctx context.Context, url, body string) (*http.Response, []byte, error) {
	t.Helper()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url+"/v1/chat/completions", strings.NewReader(body))
	if err != nil {
		t.Fatalf("NewRequest() error = %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, nil, err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	return resp, data, err
}

func chatBody(model string) string {
	return `{"model":"` + model + `","messages":[{"role":"system","content":"Be brief."},{"role":"user","content":"Explain TCP."}]}`
}

func TestRequestPathRoutesToEachProvider(t *testing.T) {
	oa := newUpstream(t, "/v1/chat/completions", reply(http.StatusOK, openaiReply))
	an := newUpstream(t, "/v1/messages", reply(http.StatusOK, anthropicReply))
	gw := gateway(t, oa, an)

	tests := []struct {
		model        string
		hit, miss    *upstream
		content      string
		finishReason string
		usage        map[string]any
	}{
		{"gpt-4o", oa, an, "from openai", "stop", map[string]any{"prompt_tokens": 12.0, "completion_tokens": 3.0, "total_tokens": 15.0}},
		{"claude-opus-5-5", an, oa, "from anthropic", "length", map[string]any{"prompt_tokens": 7.0, "completion_tokens": 4.0, "total_tokens": 11.0}},
	}
	for _, tt := range tests {
		t.Run(tt.model, func(t *testing.T) {
			beforeHit, beforeMiss := len(tt.hit.received()), len(tt.miss.received())

			resp, data, err := postChat(t, context.Background(), gw.URL, chatBody(tt.model))
			if err != nil {
				t.Fatalf("POST error = %v", err)
			}
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("status = %d, want 200 (body %s)", resp.StatusCode, data)
			}
			if got := len(tt.hit.received()) - beforeHit; got != 1 {
				t.Errorf("selected upstream received %d requests, want 1", got)
			}
			if got := len(tt.miss.received()) - beforeMiss; got != 0 {
				t.Errorf("other upstream received %d requests, want 0", got)
			}

			var got struct {
				Object  string `json:"object"`
				Choices []struct {
					Message      struct{ Role, Content string } `json:"message"`
					FinishReason string                         `json:"finish_reason"`
				} `json:"choices"`
				Usage map[string]any `json:"usage"`
			}
			if err := json.Unmarshal(data, &got); err != nil {
				t.Fatalf("decode response: %v", err)
			}
			if got.Object != "chat.completion" || len(got.Choices) != 1 {
				t.Fatalf("response = %s, want one chat.completion choice", data)
			}
			c := got.Choices[0]
			if c.Message.Role != "assistant" || c.Message.Content != tt.content || c.FinishReason != tt.finishReason {
				t.Errorf("choice = %+v, want assistant %q with %q", c, tt.content, tt.finishReason)
			}
			if !mapsEqual(got.Usage, tt.usage) {
				t.Errorf("usage = %v, want %v", got.Usage, tt.usage)
			}
		})
	}

	// Each provider received its own wire format and credentials.
	oaReq, anReq := oa.received()[0], an.received()[0]
	if got := oaReq.Header.Get("Authorization"); got != "Bearer "+openaiKey {
		t.Errorf("openai Authorization = %q", got)
	}
	if oaReq.Body["max_completion_tokens"] != 1024.0 || len(oaReq.Body["messages"].([]any)) != 2 {
		t.Errorf("openai body = %v, want 2 messages and max_completion_tokens 1024", oaReq.Body)
	}
	if got := anReq.Header.Get("x-api-key"); got != anthropicKey {
		t.Errorf("anthropic x-api-key = %q", got)
	}
	if anReq.Body["system"] != "Be brief." || anReq.Body["max_tokens"] != 1024.0 || len(anReq.Body["messages"].([]any)) != 1 {
		t.Errorf("anthropic body = %v, want top-level system, 1 message, and max_tokens 1024", anReq.Body)
	}
}

func mapsEqual(a, b map[string]any) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if b[k] != v {
			return false
		}
	}
	return true
}

func TestRequestPathErrors(t *testing.T) {
	tests := []struct {
		name     string
		model    string
		oaStatus int
		anStatus int
		status   int
		code     string
	}{
		{"unknown model", "gpt-5", 200, 200, http.StatusNotFound, "model_not_found"},
		{"openai rate limited", "gpt-4o", 429, 200, http.StatusTooManyRequests, "provider_rate_limited"},
		{"anthropic overloaded", "claude-opus-5-5", 200, 529, http.StatusBadGateway, "upstream_error"},
		{"anthropic rejects request", "claude-opus-5-5", 200, 400, http.StatusBadRequest, "invalid_request"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			const secret = "upstream secret detail"
			oa := newUpstream(t, "/v1/chat/completions", reply(tt.oaStatus, openaiReply+secret))
			an := newUpstream(t, "/v1/messages", reply(tt.anStatus, anthropicReply+secret))
			gw := gateway(t, oa, an)

			resp, data, err := postChat(t, context.Background(), gw.URL, chatBody(tt.model))
			if err != nil {
				t.Fatalf("POST error = %v", err)
			}
			if resp.StatusCode != tt.status {
				t.Errorf("status = %d, want %d", resp.StatusCode, tt.status)
			}
			var got struct {
				Error struct{ Code string } `json:"error"`
			}
			if err := json.Unmarshal(data, &got); err != nil || got.Error.Code != tt.code {
				t.Errorf("body = %s, want error code %q", data, tt.code)
			}
			if bytes.Contains(data, []byte(secret)) {
				t.Errorf("response exposes upstream body: %s", data)
			}
			if tt.code == "model_not_found" && len(oa.received())+len(an.received()) != 0 {
				t.Error("an upstream was called for an unknown model")
			}
		})
	}
}

// blockingHandler signals started when a request arrives, then waits until
// the request is canceled (signaling canceled) or release is closed.
type blockingHandler struct {
	started, canceled, release chan struct{}
	releaseOnce                sync.Once
	reply                      string
}

func newBlockingHandler(t *testing.T, reply string) *blockingHandler {
	b := &blockingHandler{
		started:  make(chan struct{}),
		canceled: make(chan struct{}),
		release:  make(chan struct{}),
		reply:    reply,
	}
	t.Cleanup(b.Release)
	return b
}

// Release lets a blocked request complete. It is safe to call repeatedly.
func (b *blockingHandler) Release() {
	b.releaseOnce.Do(func() { close(b.release) })
}

func (b *blockingHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	close(b.started)
	select {
	case <-b.release:
		_, _ = io.WriteString(w, b.reply)
	case <-r.Context().Done():
		close(b.canceled)
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

func TestRequestPathPropagatesClientCancellation(t *testing.T) {
	for _, tt := range []struct {
		model, path string
	}{
		{"gpt-4o", "/v1/chat/completions"},
		{"claude-opus-5-5", "/v1/messages"},
	} {
		t.Run(tt.model, func(t *testing.T) {
			slow := newBlockingHandler(t, "")
			oa := newUpstream(t, "/v1/chat/completions", reply(http.StatusOK, openaiReply))
			an := newUpstream(t, "/v1/messages", reply(http.StatusOK, anthropicReply))
			if tt.path == "/v1/messages" {
				an = newUpstream(t, tt.path, slow.ServeHTTP)
			} else {
				oa = newUpstream(t, tt.path, slow.ServeHTTP)
			}
			gw := gateway(t, oa, an)

			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			done := make(chan error, 1)
			go func() {
				_, _, err := postChat(t, ctx, gw.URL, chatBody(tt.model))
				done <- err
			}()

			waitFor(t, slow.started, "upstream to receive the request")
			cancel()
			waitFor(t, slow.canceled, "cancellation to reach the upstream")

			select {
			case err := <-done:
				if !errors.Is(err, context.Canceled) {
					t.Errorf("client error = %v, want context.Canceled", err)
				}
			case <-time.After(guard):
				t.Fatal("timed out waiting for the client to return")
			}
		})
	}
}

// sequence returns a handler that answers the nth request with the nth
// status, repeating the last one. 200 answers with body; other statuses
// carry retryAfter, if set, as a Retry-After header.
func sequence(body, retryAfter string, statuses ...int) http.HandlerFunc {
	var n atomic.Int64
	return func(w http.ResponseWriter, r *http.Request) {
		status := statuses[min(int(n.Add(1))-1, len(statuses)-1)]
		if status != http.StatusOK && retryAfter != "" {
			w.Header().Set("Retry-After", retryAfter)
		}
		reply(status, body)(w, r)
	}
}

func TestRequestPathRetries(t *testing.T) {
	tests := []struct {
		name       string
		statuses   []int
		retryAfter string
		status     int
		code       string
		requests   int
	}{
		{"recovers from 503", []int{503, 503, 200}, "", http.StatusOK, "", 3},
		{"recovers from 502", []int{502, 200}, "", http.StatusOK, "", 2},
		{"recovers from 504", []int{504, 200}, "", http.StatusOK, "", 2},
		{"recovers from 529", []int{529, 200}, "", http.StatusOK, "", 2},
		{"recovers from 429", []int{429, 200}, "", http.StatusOK, "", 2},
		{"exhausts attempts on 429", []int{429}, "", http.StatusTooManyRequests, "provider_rate_limited", 3},
		{"exhausts attempts on 503", []int{503}, "", http.StatusBadGateway, "upstream_error", 3},
		{"does not retry 400", []int{400, 200}, "", http.StatusBadRequest, "invalid_request", 1},
		{"does not retry 500", []int{500, 200}, "", http.StatusBadGateway, "upstream_error", 1},
		{"does not retry 401", []int{401, 200}, "", http.StatusBadGateway, "upstream_error", 1},
		{"Retry-After beyond the max delay returns at once", []int{429, 200}, "3600", http.StatusTooManyRequests, "provider_rate_limited", 1},
	}
	for _, provider := range []struct {
		model, path, body string
	}{
		{"gpt-4o", "/v1/chat/completions", openaiReply},
		{"claude-opus-5-5", "/v1/messages", anthropicReply},
	} {
		for _, tt := range tests {
			t.Run(provider.model+"/"+tt.name, func(t *testing.T) {
				oa := newUpstream(t, "/v1/chat/completions", reply(http.StatusOK, openaiReply))
				an := newUpstream(t, "/v1/messages", reply(http.StatusOK, anthropicReply))
				target := newUpstream(t, provider.path, sequence(provider.body, tt.retryAfter, tt.statuses...))
				if provider.path == "/v1/messages" {
					an = target
				} else {
					oa = target
				}
				gw := gateway(t, oa, an)

				resp, data, err := postChat(t, context.Background(), gw.URL, chatBody(provider.model))
				if err != nil {
					t.Fatalf("POST error = %v", err)
				}
				if resp.StatusCode != tt.status {
					t.Errorf("status = %d, want %d (body %s)", resp.StatusCode, tt.status, data)
				}
				if tt.code != "" {
					var got struct {
						Error struct{ Code string } `json:"error"`
					}
					if err := json.Unmarshal(data, &got); err != nil || got.Error.Code != tt.code {
						t.Errorf("body = %s, want error code %q", data, tt.code)
					}
				}
				if got := len(target.received()); got != tt.requests {
					t.Errorf("upstream received %d requests, want %d", got, tt.requests)
				}
				// Every attempt resends the same request.
				for i, r := range target.received() {
					if !reflect.DeepEqual(r.Body, target.received()[0].Body) {
						t.Errorf("attempt %d body = %v, want the first attempt's body", i+1, r.Body)
					}
				}
			})
		}
	}
}

func TestRequestPathRetriesConnectionFailures(t *testing.T) {
	// A closed listener refuses connections, so every attempt fails to
	// dial. Attempts are counted through the retry log.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("Listen() error = %v", err)
	}
	refused := "http://" + ln.Addr().String()
	ln.Close()

	cfg := gatewayConfig(newUpstream(t, "/v1/chat/completions", reply(http.StatusOK, openaiReply)),
		newUpstream(t, "/v1/messages", reply(http.StatusOK, anthropicReply)))
	cfg.OpenAI.BaseURL = refused
	var logs syncBuffer
	h, err := newHandler(cfg, nil, slog.New(slog.NewTextHandler(&logs, nil)))
	if err != nil {
		t.Fatalf("newHandler() error = %v", err)
	}
	gw := httptest.NewServer(h)
	t.Cleanup(gw.Close)

	resp, data, err := postChat(t, context.Background(), gw.URL, chatBody("gpt-4o"))
	if err != nil {
		t.Fatalf("POST error = %v", err)
	}
	if resp.StatusCode != http.StatusBadGateway {
		t.Errorf("status = %d, want 502 (body %s)", resp.StatusCode, data)
	}
	if n := strings.Count(logs.String(), "upstream attempt failed, retrying"); n != fastRetry.MaxAttempts-1 {
		t.Errorf("logged %d retries, want %d:\n%s", n, fastRetry.MaxAttempts-1, logs.String())
	}
	if !strings.Contains(logs.String(), "after 3 attempts") {
		t.Errorf("failure log does not report the attempt count:\n%s", logs.String())
	}
}

func TestRequestPathClientCancellationStopsRetries(t *testing.T) {
	// The upstream asks for a long wait, and the budget and max delay allow
	// it, so the gateway is waiting to retry when the client goes away.
	first := make(chan struct{})
	var once sync.Once
	oa := newUpstream(t, "/v1/chat/completions", func(w http.ResponseWriter, r *http.Request) {
		once.Do(func() { close(first) })
		w.Header().Set("Retry-After", "3600")
		w.WriteHeader(http.StatusServiceUnavailable)
	})
	an := newUpstream(t, "/v1/messages", reply(http.StatusOK, anthropicReply))
	cfg := gatewayConfig(oa, an)
	cfg.UpstreamTimeout = 2 * time.Hour
	cfg.Retry.MaxDelay = 2 * time.Hour
	h, err := newHandler(cfg, nil, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatalf("newHandler() error = %v", err)
	}
	handlerDone := make(chan struct{})
	gw := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer close(handlerDone)
		h.ServeHTTP(w, r)
	}))
	t.Cleanup(gw.Close)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _, _, _ = postChat(t, ctx, gw.URL, chatBody("gpt-4o")) }()

	waitFor(t, first, "the first upstream attempt")
	cancel()
	waitFor(t, handlerDone, "the gateway to stop waiting and return")
	if got := len(oa.received()); got != 1 {
		t.Errorf("upstream received %d requests, want 1", got)
	}
}

// errorCode returns the error code in a gateway error response.
func errorCode(t *testing.T, data []byte) string {
	t.Helper()
	var got struct {
		Error struct{ Code string } `json:"error"`
	}
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("decode error response %s: %v", data, err)
	}
	return got.Error.Code
}

// replyModel returns the model reported in a successful gateway response.
func replyModel(t *testing.T, data []byte) string {
	t.Helper()
	var got struct{ Model string }
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("decode response %s: %v", data, err)
	}
	return got.Model
}

func TestRequestPathFallsBackToOtherProvider(t *testing.T) {
	tests := []struct {
		name     string
		oaStatus int
		requests int // primary requests, including retries
		fallback bool
		status   int
	}{
		{"unavailable after retries", 503, fastRetry.MaxAttempts, true, http.StatusOK},
		{"rate limited after retries", 429, fastRetry.MaxAttempts, true, http.StatusOK},
		{"server error", 500, 1, true, http.StatusOK},
		{"authentication failure", 401, 1, true, http.StatusOK},
		{"rejected request", 400, 1, false, http.StatusBadRequest},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			oa := newUpstream(t, "/v1/chat/completions", reply(tt.oaStatus, openaiReply))
			an := newUpstream(t, "/v1/messages", reply(http.StatusOK, anthropicReply))
			gw := gatewayWith(t, oa, an, func(c *config) { c.OpenAI.FallbackTo = "anthropic" })

			resp, data, err := postChat(t, context.Background(), gw.URL, chatBody("gpt-4o"))
			if err != nil {
				t.Fatalf("POST error = %v", err)
			}
			if resp.StatusCode != tt.status {
				t.Fatalf("status = %d, want %d (body %s)", resp.StatusCode, tt.status, data)
			}
			if got := len(oa.received()); got != tt.requests {
				t.Errorf("primary received %d requests, want %d", got, tt.requests)
			}
			if !tt.fallback {
				if len(an.received()) != 0 {
					t.Error("fallback was called for a rejected request")
				}
				return
			}
			// The fallback received its own model and wire format, and the
			// response names the model that answered.
			got := an.received()
			if len(got) != 1 {
				t.Fatalf("fallback received %d requests, want 1", len(got))
			}
			if got[0].Body["model"] != "claude-opus-5-5" || got[0].Body["system"] != "Be brief." {
				t.Errorf("fallback body = %v, want the Anthropic model and wire format", got[0].Body)
			}
			if got[0].Header.Get("x-api-key") != anthropicKey {
				t.Error("fallback request does not carry the Anthropic key")
			}
			if m := replyModel(t, data); m != "claude-opus-5-5" {
				t.Errorf("response model = %q, want the fallback model", m)
			}
		})
	}
}

func TestRequestPathFallsBackAfterPrimaryTimeout(t *testing.T) {
	slow := newBlockingHandler(t, "")
	oa := newUpstream(t, "/v1/chat/completions", slow.ServeHTTP)
	an := newUpstream(t, "/v1/messages", reply(http.StatusOK, anthropicReply))
	gw := gatewayWith(t, oa, an, func(c *config) {
		c.OpenAI.FallbackTo = "anthropic"
		c.ProviderTimeout = 50 * time.Millisecond
		c.UpstreamTimeout = guard
	})

	resp, data, err := postChat(t, context.Background(), gw.URL, chatBody("gpt-4o"))
	if err != nil {
		t.Fatalf("POST error = %v", err)
	}
	if resp.StatusCode != http.StatusOK || replyModel(t, data) != "claude-opus-5-5" {
		t.Errorf("status = %d, body %s; want 200 from the fallback", resp.StatusCode, data)
	}
	// The timed-out primary request was canceled, not left running.
	waitFor(t, slow.canceled, "cancellation to reach the primary upstream")
}

func TestRequestPathLongRetryAfterFallsBackAtOnce(t *testing.T) {
	// A rate-limited primary asks to wait 20s: longer than the retry max
	// delay (8s), though within the primary's 60s time limit. Instead of
	// waiting, the gateway uses the healthy fallback.
	oa := newUpstream(t, "/v1/chat/completions", sequence("", "20", http.StatusTooManyRequests))
	an := newUpstream(t, "/v1/messages", reply(http.StatusOK, anthropicReply))
	gw := gatewayWith(t, oa, an, func(c *config) {
		c.OpenAI.FallbackTo = "anthropic"
		c.Retry = defaultRetryPolicy()
	})

	start := time.Now()
	resp, data, err := postChat(t, context.Background(), gw.URL, chatBody("gpt-4o"))
	if err != nil {
		t.Fatalf("POST error = %v", err)
	}
	if resp.StatusCode != http.StatusOK || replyModel(t, data) != "claude-opus-5-5" {
		t.Errorf("status = %d, body %s; want 200 from the fallback", resp.StatusCode, data)
	}
	if got := len(oa.received()); got != 1 {
		t.Errorf("primary received %d requests, want 1: a 20s Retry-After must not be retried", got)
	}
	// Honoring the Retry-After would take 20s.
	if elapsed := time.Since(start); elapsed >= 5*time.Second {
		t.Errorf("fallback answered after %v, want it without waiting", elapsed)
	}
}

func defaultRetryPolicy() retry.Policy {
	return retry.Policy{MaxAttempts: defaultRetryMaxAttempts, BaseDelay: defaultRetryBaseDelay, MaxDelay: defaultRetryMaxDelay}
}

func TestRequestPathBothProvidersFail(t *testing.T) {
	oa := newUpstream(t, "/v1/chat/completions", reply(http.StatusServiceUnavailable, ""))
	an := newUpstream(t, "/v1/messages", reply(http.StatusTooManyRequests, ""))
	gw := gatewayWith(t, oa, an, func(c *config) { c.OpenAI.FallbackTo = "anthropic" })

	resp, data, err := postChat(t, context.Background(), gw.URL, chatBody("gpt-4o"))
	if err != nil {
		t.Fatalf("POST error = %v", err)
	}
	// The last provider tried decides the response: Anthropic's 429.
	if resp.StatusCode != http.StatusTooManyRequests || errorCode(t, data) != "provider_rate_limited" {
		t.Errorf("status = %d, body %s; want 429 provider_rate_limited from the fallback", resp.StatusCode, data)
	}
	if len(oa.received()) != fastRetry.MaxAttempts || len(an.received()) != fastRetry.MaxAttempts {
		t.Errorf("requests = openai %d, anthropic %d; want %d each", len(oa.received()), len(an.received()), fastRetry.MaxAttempts)
	}
}

func TestRequestPathOpensCircuitAfterRepeatedFailures(t *testing.T) {
	oa := newUpstream(t, "/v1/chat/completions", reply(http.StatusServiceUnavailable, ""))
	an := newUpstream(t, "/v1/messages", reply(http.StatusOK, anthropicReply))
	gw := gatewayWith(t, oa, an, func(c *config) {
		c.Retry.MaxAttempts = 1
		c.Breaker = breaker.Settings{Failures: 2, Cooldown: time.Hour}
	})

	for i := range 2 {
		resp, _, err := postChat(t, context.Background(), gw.URL, chatBody("gpt-4o"))
		if err != nil || resp.StatusCode != http.StatusBadGateway {
			t.Fatalf("request %d: status %v, error %v; want 502", i+1, resp, err)
		}
	}

	// The circuit is open: the next request fails fast without reaching
	// the upstream.
	resp, data, err := postChat(t, context.Background(), gw.URL, chatBody("gpt-4o"))
	if err != nil {
		t.Fatalf("POST error = %v", err)
	}
	if resp.StatusCode != http.StatusServiceUnavailable || errorCode(t, data) != "provider_unavailable" {
		t.Errorf("status = %d, body %s; want 503 provider_unavailable", resp.StatusCode, data)
	}
	if got := len(oa.received()); got != 2 {
		t.Errorf("upstream received %d requests, want 2: an open circuit must not call it", got)
	}

	// The other provider has its own breaker and is unaffected.
	if resp, _, err := postChat(t, context.Background(), gw.URL, chatBody("claude-opus-5-5")); err != nil || resp.StatusCode != http.StatusOK {
		t.Errorf("other provider: status %v, error %v; want 200", resp, err)
	}
}

func TestRequestPathOpenCircuitGoesStraightToFallback(t *testing.T) {
	oa := newUpstream(t, "/v1/chat/completions", reply(http.StatusServiceUnavailable, ""))
	an := newUpstream(t, "/v1/messages", reply(http.StatusOK, anthropicReply))
	gw := gatewayWith(t, oa, an, func(c *config) {
		c.OpenAI.FallbackTo = "anthropic"
		c.Retry.MaxAttempts = 1
		c.Breaker = breaker.Settings{Failures: 1, Cooldown: time.Hour}
	})

	for i := range 3 {
		resp, data, err := postChat(t, context.Background(), gw.URL, chatBody("gpt-4o"))
		if err != nil || resp.StatusCode != http.StatusOK || replyModel(t, data) != "claude-opus-5-5" {
			t.Fatalf("request %d: status %v, error %v; want 200 from the fallback", i+1, resp, err)
		}
	}
	// Only the first request reached the failing primary; the open circuit
	// sent the others straight to the fallback.
	if got := len(oa.received()); got != 1 {
		t.Errorf("primary received %d requests, want 1", got)
	}
	if got := len(an.received()); got != 3 {
		t.Errorf("fallback received %d requests, want 3", got)
	}
}

func TestRequestPathFallbackSharesProviderBreaker(t *testing.T) {
	// Anthropic's circuit opens through its own route. When OpenAI then
	// falls back to Anthropic, the same breaker rejects the fallback.
	oa := newUpstream(t, "/v1/chat/completions", reply(http.StatusServiceUnavailable, ""))
	an := newUpstream(t, "/v1/messages", reply(http.StatusServiceUnavailable, ""))
	gw := gatewayWith(t, oa, an, func(c *config) {
		c.OpenAI.FallbackTo = "anthropic"
		c.Retry.MaxAttempts = 1
		c.Breaker = breaker.Settings{Failures: 1, Cooldown: time.Hour}
	})

	if resp, _, err := postChat(t, context.Background(), gw.URL, chatBody("claude-opus-5-5")); err != nil || resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("anthropic request: status %v, error %v; want 502", resp, err)
	}
	resp, data, err := postChat(t, context.Background(), gw.URL, chatBody("gpt-4o"))
	if err != nil {
		t.Fatalf("POST error = %v", err)
	}
	if resp.StatusCode != http.StatusServiceUnavailable || errorCode(t, data) != "provider_unavailable" {
		t.Errorf("status = %d, body %s; want 503 provider_unavailable from the open fallback circuit", resp.StatusCode, data)
	}
	if got := len(an.received()); got != 1 {
		t.Errorf("anthropic received %d requests, want 1: its open circuit must also guard the fallback", got)
	}
}

func TestRequestPathTimesOutSlowUpstream(t *testing.T) {
	for _, tt := range []struct {
		model, path string
	}{
		{"gpt-4o", "/v1/chat/completions"},
		{"claude-opus-5-5", "/v1/messages"},
	} {
		t.Run(tt.model, func(t *testing.T) {
			slow := newBlockingHandler(t, "")
			oa := newUpstream(t, "/v1/chat/completions", reply(http.StatusOK, openaiReply))
			an := newUpstream(t, "/v1/messages", reply(http.StatusOK, anthropicReply))
			if tt.path == "/v1/messages" {
				an = newUpstream(t, tt.path, slow.ServeHTTP)
			} else {
				oa = newUpstream(t, tt.path, slow.ServeHTTP)
			}
			gw := gatewayWith(t, oa, an, func(c *config) { c.UpstreamTimeout = 50 * time.Millisecond })

			resp, data, err := postChat(t, context.Background(), gw.URL, chatBody(tt.model))
			if err != nil {
				t.Fatalf("POST error = %v", err)
			}
			if resp.StatusCode != http.StatusGatewayTimeout {
				t.Errorf("status = %d, want 504 (body %s)", resp.StatusCode, data)
			}
			var got struct {
				Error struct{ Code string } `json:"error"`
			}
			if err := json.Unmarshal(data, &got); err != nil || got.Error.Code != "upstream_timeout" {
				t.Errorf("body = %s, want error code upstream_timeout", data)
			}
			// The timed-out upstream request is canceled, not left running.
			waitFor(t, slow.canceled, "cancellation to reach the upstream")
		})
	}
}

func TestUpstreamClientBoundsConnectionSetup(t *testing.T) {
	c := newUpstreamClient(config{ConnectTimeout: 3 * time.Second})
	tr, ok := c.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("Transport = %T, want *http.Transport", c.Transport)
	}
	if tr == http.DefaultTransport {
		t.Error("upstream client shares http.DefaultTransport")
	}
	if tr.DialContext == nil || tr.TLSHandshakeTimeout != 3*time.Second || tr.IdleConnTimeout != idleConnTimeout {
		t.Errorf("transport timeouts: dialer set %v, TLS %v, idle %v; want dialer, 3s, %v",
			tr.DialContext != nil, tr.TLSHandshakeTimeout, tr.IdleConnTimeout, idleConnTimeout)
	}
	if c.Timeout != 0 {
		t.Errorf("client Timeout = %v, want 0: the request context carries the deadline", c.Timeout)
	}
}

func TestUpstreamClientTimesOutStalledTLSHandshake(t *testing.T) {
	// The listener accepts TCP connections but never answers the TLS
	// handshake, like an overloaded or broken upstream.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("Listen() error = %v", err)
	}
	accepted := make(chan struct{}, 1)
	var conns []net.Conn
	var mu sync.Mutex
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			mu.Lock()
			conns = append(conns, c)
			mu.Unlock()
			select {
			case accepted <- struct{}{}:
			default:
			}
		}
	}()
	t.Cleanup(func() {
		ln.Close()
		mu.Lock()
		defer mu.Unlock()
		for _, c := range conns {
			c.Close()
		}
	})

	c := newUpstreamClient(config{ConnectTimeout: 50 * time.Millisecond})
	// guard is only a failure guard: the handshake timeout must fire first.
	ctx, cancel := context.WithTimeout(context.Background(), guard)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://"+ln.Addr().String()+"/v1/messages", nil)
	if err != nil {
		t.Fatalf("NewRequest() error = %v", err)
	}

	resp, err := c.Do(req)
	if err == nil {
		resp.Body.Close()
		t.Fatal("Do() error = nil, want TLS handshake timeout")
	}
	waitFor(t, accepted, "the TCP connection to be accepted")
	if !strings.Contains(err.Error(), "TLS handshake timeout") {
		t.Errorf("Do() error = %v, want TLS handshake timeout", err)
	}
	if ctx.Err() != nil {
		t.Errorf("request context expired (%v); the connect timeout did not fire", ctx.Err())
	}
}

// startServe runs serve with the gateway handler on a free local port. The
// shuttingDown channel is closed once graceful shutdown has begun.
func startServe(t *testing.T, handler http.Handler, timeout time.Duration) (url string, cancel context.CancelFunc, shuttingDown <-chan struct{}, result <-chan error) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("Listen() error = %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	srv := &http.Server{Handler: handler, ReadHeaderTimeout: readHeaderTimeout}
	shutdownCh := make(chan struct{})
	srv.RegisterOnShutdown(func() { close(shutdownCh) })
	errCh := make(chan error, 1)
	go func() { errCh <- serve(ctx, srv, ln, timeout) }()
	return "http://" + ln.Addr().String(), cancel, shutdownCh, errCh
}

func TestServeShutsDownGracefully(t *testing.T) {
	slow := newBlockingHandler(t, openaiReply)
	oa := newUpstream(t, "/v1/chat/completions", slow.ServeHTTP)
	an := newUpstream(t, "/v1/messages", reply(http.StatusOK, anthropicReply))
	h, err := newHandler(gatewayConfig(oa, an), nil, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatalf("newHandler() error = %v", err)
	}
	url, stop, shuttingDown, result := startServe(t, h, guard)

	// A request in flight when shutdown starts still completes.
	type outcome struct {
		status int
		err    error
	}
	inFlight := make(chan outcome, 1)
	go func() {
		resp, _, err := postChat(t, context.Background(), url, chatBody("gpt-4o"))
		if err != nil {
			inFlight <- outcome{err: err}
			return
		}
		inFlight <- outcome{status: resp.StatusCode}
	}()
	waitFor(t, slow.started, "upstream to receive the request")

	stop()
	waitFor(t, shuttingDown, "graceful shutdown to begin")
	slow.Release()

	select {
	case got := <-inFlight:
		if got.err != nil || got.status != http.StatusOK {
			t.Errorf("in-flight request = %+v, want 200", got)
		}
	case <-time.After(guard):
		t.Fatal("timed out waiting for the in-flight request")
	}
	select {
	case err := <-result:
		if err != nil {
			t.Errorf("serve() error = %v, want nil", err)
		}
	case <-time.After(guard):
		t.Fatal("timed out waiting for serve to return")
	}

	// New connections are refused after shutdown.
	if _, _, err := postChat(t, context.Background(), url, chatBody("gpt-4o")); err == nil {
		t.Error("request after shutdown succeeded, want connection error")
	}
}

func TestServeCutsOffRequestsAfterShutdownTimeout(t *testing.T) {
	slow := newBlockingHandler(t, openaiReply)
	oa := newUpstream(t, "/v1/chat/completions", slow.ServeHTTP)
	an := newUpstream(t, "/v1/messages", reply(http.StatusOK, anthropicReply))
	h, err := newHandler(gatewayConfig(oa, an), nil, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatalf("newHandler() error = %v", err)
	}
	url, stop, _, result := startServe(t, h, 50*time.Millisecond)

	go func() { _, _, _ = postChat(t, context.Background(), url, chatBody("gpt-4o")) }()
	waitFor(t, slow.started, "upstream to receive the request")
	stop()

	select {
	case err := <-result:
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Errorf("serve() error = %v, want graceful shutdown deadline exceeded", err)
		}
	case <-time.After(guard):
		t.Fatal("timed out waiting for serve to return")
	}
	// Closing the server cancels the stuck request all the way upstream.
	waitFor(t, slow.canceled, "cancellation to reach the upstream")
}

func gatewayConfig(oa, an *upstream) config {
	return config{
		Addr:            "127.0.0.1:0",
		UpstreamTimeout: defaultUpstreamTimeout,
		ConnectTimeout:  defaultConnectTimeout,
		ProviderTimeout: defaultUpstreamTimeout / 2,
		Retry:           fastRetry,
		Breaker:         breaker.Settings{Failures: defaultBreakerFailures, Cooldown: defaultBreakerCooldown},
		OpenAI:          &providerConfig{Model: "gpt-4o", APIKey: openaiKey, BaseURL: oa.srv.URL},
		Anthropic:       &providerConfig{Model: "claude-opus-5-5", APIKey: anthropicKey, BaseURL: an.srv.URL},
	}
}

func TestServeReportsServeErrors(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("Listen() error = %v", err)
	}
	ln.Close() // Serve fails immediately on a closed listener.

	err = serve(context.Background(), &http.Server{}, ln, guard)
	if err == nil {
		t.Fatal("serve() error = nil, want error")
	}
}

func TestRunFailsOnInvalidConfiguration(t *testing.T) {
	var logs bytes.Buffer
	err := run(context.Background(), env(map[string]string{"OPENAI_MODEL": "gpt-4o"}), slog.New(slog.NewTextHandler(&logs, nil)))
	if err == nil || !strings.Contains(err.Error(), "OPENAI_API_KEY is missing") {
		t.Errorf("run() error = %v, want missing OPENAI_API_KEY", err)
	}
}

func TestRunFailsWhenAddressIsUnavailable(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("Listen() error = %v", err)
	}
	defer ln.Close()

	err = run(context.Background(), env(map[string]string{
		"OPENAI_MODEL": "gpt-4o", "OPENAI_API_KEY": openaiKey,
		"GATEWAY_ADDR": ln.Addr().String(),
	}), slog.New(slog.DiscardHandler))
	if err == nil || !strings.Contains(err.Error(), "listen") {
		t.Errorf("run() error = %v, want listen error", err)
	}
}

func TestRunServesUntilCanceled(t *testing.T) {
	var logs syncBuffer
	ctx, cancel := context.WithCancel(context.Background())
	result := make(chan error, 1)
	go func() {
		result <- run(ctx, env(map[string]string{
			"ANTHROPIC_MODEL": "claude-opus-5-5", "ANTHROPIC_API_KEY": anthropicKey,
			"GATEWAY_ADDR": "127.0.0.1:0",
		}), slog.New(slog.NewTextHandler(&logs, nil)))
	}()

	deadline := time.After(guard)
	for !strings.Contains(logs.String(), "gateway listening") {
		select {
		case err := <-result:
			t.Fatalf("run() returned early: %v", err)
		case <-deadline:
			t.Fatal("timed out waiting for the gateway to listen")
		case <-time.After(10 * time.Millisecond):
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
	if !strings.Contains(logs.String(), "anthropic_model=claude-opus-5-5") {
		t.Errorf("startup log does not name the configured model:\n%s", logs.String())
	}
	if strings.Contains(logs.String(), anthropicKey) {
		t.Errorf("log exposes the API key:\n%s", logs.String())
	}
}

// syncBuffer is a bytes.Buffer safe for concurrent writes and reads.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}
