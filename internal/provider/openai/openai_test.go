package openai

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ipekutku/llm-gateway/internal/llm"
)

const testKey = "sk-test-not-a-real-key"

// guard bounds how long a test waits for something that should happen
// promptly. It is a failure guard, not a synchronization mechanism.
const guard = 5 * time.Second

// successBody is a non-streaming response modeled on the official example.
const successBody = `{
  "id": "chatcmpl-B9MHDbslfkBeAs8l4bebGdFOJ6PeG",
  "object": "chat.completion",
  "created": 1741570283,
  "model": "gpt-4o-2024-08-06",
  "choices": [
    {
      "index": 0,
      "message": {
        "role": "assistant",
        "content": "TCP provides reliable, ordered delivery.",
        "refusal": null,
        "annotations": []
      },
      "logprobs": null,
      "finish_reason": "stop"
    }
  ],
  "usage": {
    "prompt_tokens": 12,
    "completion_tokens": 8,
    "total_tokens": 20,
    "prompt_tokens_details": {"cached_tokens": 0, "audio_tokens": 0},
    "completion_tokens_details": {"reasoning_tokens": 0, "audio_tokens": 0, "accepted_prediction_tokens": 0, "rejected_prediction_tokens": 0}
  },
  "service_tier": "default",
  "system_fingerprint": "fp_fc9f1d7035"
}`

var testRequest = llm.ChatRequest{
	Model: "gpt-4o",
	Messages: []llm.Message{
		{Role: llm.RoleSystem, Content: "Answer concisely."},
		{Role: llm.RoleUser, Content: "Explain TCP."},
	},
	MaxTokens: 100,
}

// newServer starts a fake upstream running handler and returns a Client
// pointed at it.
func newServer(t *testing.T, handler http.HandlerFunc) *Client {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	c, err := New(testKey, srv.URL, srv.Client())
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	return c
}

// respond returns a handler that writes status and body.
func respond(status int, body string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = io.WriteString(w, body)
	}
}

func assertProviderError(t *testing.T, err error, status int) *llm.ProviderError {
	t.Helper()
	pe, ok := errors.AsType[*llm.ProviderError](err)
	if !ok {
		t.Fatalf("error = %v (%T), want *llm.ProviderError", err, err)
	}
	if pe.Provider != ProviderName {
		t.Errorf("Provider = %q, want %q", pe.Provider, ProviderName)
	}
	if pe.StatusCode != status {
		t.Errorf("StatusCode = %d, want %d", pe.StatusCode, status)
	}
	return pe
}

func TestNew(t *testing.T) {
	tests := []struct {
		name    string
		apiKey  string
		baseURL string
		want    string
		wantErr bool
	}{
		{name: "production origin", apiKey: "k", baseURL: DefaultBaseURL, want: "https://api.openai.com/v1/chat/completions"},
		{name: "trailing slash", apiKey: "k", baseURL: "https://api.openai.com/", want: "https://api.openai.com/v1/chat/completions"},
		{name: "path prefix", apiKey: "k", baseURL: "http://127.0.0.1:9000/proxy", want: "http://127.0.0.1:9000/proxy/v1/chat/completions"},
		{name: "blank key", apiKey: " ", baseURL: DefaultBaseURL, wantErr: true},
		{name: "empty URL", apiKey: "k", baseURL: "", wantErr: true},
		{name: "no scheme", apiKey: "k", baseURL: "api.openai.com", wantErr: true},
		{name: "unsupported scheme", apiKey: "k", baseURL: "ftp://api.openai.com", wantErr: true},
		{name: "no host", apiKey: "k", baseURL: "https://", wantErr: true},
		{name: "query", apiKey: "k", baseURL: "https://api.openai.com?x=1", wantErr: true},
		{name: "credentials", apiKey: "k", baseURL: "https://user:pass@api.openai.com", wantErr: true},
		{name: "unparsable", apiKey: "k", baseURL: "https://api.openai.com/%zz", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c, err := New(tt.apiKey, tt.baseURL, nil)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("New() error = nil, want error")
				}
				return
			}
			if err != nil {
				t.Fatalf("New() error = %v", err)
			}
			if c.endpoint != tt.want {
				t.Errorf("endpoint = %q, want %q", c.endpoint, tt.want)
			}
			if c.http != http.DefaultClient {
				t.Error("nil http client did not default to http.DefaultClient")
			}
		})
	}
}

func TestChatSendsTranslatedRequest(t *testing.T) {
	var (
		method, path string
		header       http.Header
		body         map[string]any
	)
	c := newServer(t, func(w http.ResponseWriter, r *http.Request) {
		method, path, header = r.Method, r.URL.Path, r.Header.Clone()
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("decode upstream request: %v", err)
		}
		respond(http.StatusOK, successBody)(w, r)
	})

	req := testRequest
	req.Messages = append([]llm.Message(nil), testRequest.Messages...)
	if _, err := c.Chat(context.Background(), req); err != nil {
		t.Fatalf("Chat() error = %v", err)
	}

	if method != http.MethodPost || path != endpointPath {
		t.Errorf("request = %s %s, want POST %s", method, path, endpointPath)
	}
	if got := header.Get("Authorization"); got != "Bearer "+testKey {
		t.Errorf("Authorization = %q, want bearer token", got)
	}
	if got := header.Get("Content-Type"); got != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", got)
	}
	want := map[string]any{
		"model": "gpt-4o",
		"messages": []any{
			map[string]any{"role": "system", "content": "Answer concisely."},
			map[string]any{"role": "user", "content": "Explain TCP."},
		},
		"max_completion_tokens": float64(100),
	}
	if !reflect.DeepEqual(body, want) {
		t.Errorf("upstream body = %v, want %v", body, want)
	}
	if !reflect.DeepEqual(req, testRequest) {
		t.Errorf("Chat() mutated the request: %+v", req)
	}
}

func TestChatUsesPathPrefix(t *testing.T) {
	var path string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path = r.URL.Path
		respond(http.StatusOK, successBody)(w, r)
	}))
	t.Cleanup(srv.Close)
	c, err := New(testKey, srv.URL+"/proxy/", srv.Client())
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}

	if _, err := c.Chat(context.Background(), testRequest); err != nil {
		t.Fatalf("Chat() error = %v", err)
	}
	if path != "/proxy/v1/chat/completions" {
		t.Errorf("path = %q, want /proxy/v1/chat/completions", path)
	}
}

func TestChatTranslatesResponse(t *testing.T) {
	c := newServer(t, respond(http.StatusOK, successBody))

	got, err := c.Chat(context.Background(), testRequest)
	if err != nil {
		t.Fatalf("Chat() error = %v", err)
	}
	want := llm.ChatResponse{
		Provider:     ProviderName,
		Model:        "gpt-4o-2024-08-06",
		Message:      llm.Message{Role: llm.RoleAssistant, Content: "TCP provides reliable, ordered delivery."},
		FinishReason: llm.FinishReasonStop,
		Usage:        llm.Usage{InputTokens: 12, OutputTokens: 8},
	}
	if got != want {
		t.Errorf("Chat() = %+v, want %+v", got, want)
	}
}

// responseWith returns a successful response body with the given message
// JSON, finish reason, and usage JSON.
func responseWith(message, finish, usage string) string {
	return `{"model":"gpt-4o","choices":[{"index":0,"message":` + message +
		`,"finish_reason":"` + finish + `"}],"usage":` + usage + `}`
}

const okUsage = `{"prompt_tokens":3,"completion_tokens":4,"total_tokens":7}`

func TestChatFinishReasons(t *testing.T) {
	tests := []struct {
		reason  string
		want    string
		wantErr bool
	}{
		{reason: "stop", want: llm.FinishReasonStop},
		{reason: "length", want: llm.FinishReasonLength},
		{reason: "content_filter", want: llm.FinishReasonContentFilter},
		{reason: "tool_calls", wantErr: true},
		{reason: "function_call", wantErr: true},
		{reason: "", wantErr: true},
		{reason: "something_new", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.reason, func(t *testing.T) {
			c := newServer(t, respond(http.StatusOK,
				responseWith(`{"role":"assistant","content":"hi"}`, tt.reason, okUsage)))

			got, err := c.Chat(context.Background(), testRequest)
			if tt.wantErr {
				assertProviderError(t, err, http.StatusOK)
				return
			}
			if err != nil {
				t.Fatalf("Chat() error = %v", err)
			}
			if got.FinishReason != tt.want {
				t.Errorf("FinishReason = %q, want %q", got.FinishReason, tt.want)
			}
		})
	}
}

func TestChatAcceptsEmptyText(t *testing.T) {
	c := newServer(t, respond(http.StatusOK,
		responseWith(`{"role":"assistant","content":""}`, "length", okUsage)))

	got, err := c.Chat(context.Background(), testRequest)
	if err != nil {
		t.Fatalf("Chat() error = %v", err)
	}
	if got.Message.Content != "" || got.FinishReason != llm.FinishReasonLength {
		t.Errorf("Chat() = %+v, want empty content with length", got)
	}
}

func TestChatRefusal(t *testing.T) {
	c := newServer(t, respond(http.StatusOK,
		responseWith(`{"role":"assistant","content":null,"refusal":"I can't help with that."}`, "stop", okUsage)))

	got, err := c.Chat(context.Background(), testRequest)
	if err != nil {
		t.Fatalf("Chat() error = %v", err)
	}
	want := llm.ChatResponse{
		Provider:     ProviderName,
		Model:        "gpt-4o",
		Message:      llm.Message{Role: llm.RoleAssistant, Content: "I can't help with that."},
		FinishReason: llm.FinishReasonContentFilter,
		Usage:        llm.Usage{InputTokens: 3, OutputTokens: 4},
	}
	if got != want {
		t.Errorf("Chat() = %+v, want %+v", got, want)
	}
}

func TestChatUsageCachedTokens(t *testing.T) {
	tests := []struct {
		name  string
		usage string
		want  llm.Usage
	}{
		{"cached", `{"prompt_tokens":2006,"completion_tokens":300,"total_tokens":2306,"prompt_tokens_details":{"cached_tokens":1920,"audio_tokens":0}}`,
			llm.Usage{InputTokens: 2006, CacheReadInputTokens: 1920, OutputTokens: 300}},
		{"all cached", `{"prompt_tokens":1024,"completion_tokens":1,"prompt_tokens_details":{"cached_tokens":1024}}`,
			llm.Usage{InputTokens: 1024, CacheReadInputTokens: 1024, OutputTokens: 1}},
		{"no details", okUsage, llm.Usage{InputTokens: 3, OutputTokens: 4}},
		{"null details", `{"prompt_tokens":3,"completion_tokens":4,"prompt_tokens_details":null}`, llm.Usage{InputTokens: 3, OutputTokens: 4}},
		{"details without cached tokens", `{"prompt_tokens":3,"completion_tokens":4,"prompt_tokens_details":{"audio_tokens":0}}`, llm.Usage{InputTokens: 3, OutputTokens: 4}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := newServer(t, respond(http.StatusOK,
				responseWith(`{"role":"assistant","content":"hi"}`, "stop", tt.usage)))

			got, err := c.Chat(context.Background(), testRequest)
			if err != nil {
				t.Fatalf("Chat() error = %v", err)
			}
			if got.Usage != tt.want {
				t.Errorf("Usage = %+v, want %+v", got.Usage, tt.want)
			}
		})
	}
}

func TestChatUpstreamStatus(t *testing.T) {
	const upstreamBody = `{"error":{"message":"Incorrect API key provided: sk-test-not-a-real-key","type":"invalid_request_error"}}`
	for _, status := range []int{400, 401, 403, 404, 429, 500, 503} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			c := newServer(t, respond(status, upstreamBody))

			_, err := c.Chat(context.Background(), testRequest)

			assertProviderError(t, err, status)
			if strings.Contains(err.Error(), "Incorrect API key") || strings.Contains(err.Error(), testKey) {
				t.Errorf("error exposes upstream body or key: %v", err)
			}
		})
	}
}

func TestChatReusesConnectionAfterErrorStatus(t *testing.T) {
	var conns atomic.Int64
	srv := httptest.NewUnstartedServer(respond(http.StatusServiceUnavailable, `{"error":"overloaded"}`))
	srv.Config.ConnState = func(_ net.Conn, s http.ConnState) {
		if s == http.StateNew {
			conns.Add(1)
		}
	}
	srv.Start()
	t.Cleanup(srv.Close)
	c, err := New(testKey, srv.URL, srv.Client())
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}

	for range 3 {
		_, err := c.Chat(context.Background(), testRequest)
		assertProviderError(t, err, http.StatusServiceUnavailable)
	}

	if got := conns.Load(); got != 1 {
		t.Errorf("3 failed requests used %d connections, want 1", got)
	}
}

func TestChatRetryAfter(t *testing.T) {
	tests := []struct {
		name   string
		header string
		want   time.Duration
	}{
		{"absent", "", 0},
		{"seconds", "7", 7 * time.Second},
		{"surrounding whitespace", " 30 ", 30 * time.Second},
		{"zero", "0", 0},
		{"negative", "-5", 0},
		{"fraction", "1.5", 0},
		{"HTTP date", "Wed, 21 Oct 2015 07:28:00 GMT", 0},
		{"capped", "999999999999", 24 * time.Hour},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := newServer(t, func(w http.ResponseWriter, r *http.Request) {
				if tt.header != "" {
					w.Header().Set("Retry-After", tt.header)
				}
				w.WriteHeader(http.StatusTooManyRequests)
			})

			_, err := c.Chat(context.Background(), testRequest)

			pe := assertProviderError(t, err, http.StatusTooManyRequests)
			if pe.RetryAfter != tt.want {
				t.Errorf("RetryAfter = %v, want %v", pe.RetryAfter, tt.want)
			}
		})
	}
}

func TestChatInvalidResponses(t *testing.T) {
	tests := []struct {
		name string
		body string
	}{
		{"malformed JSON", `{"choices":[`},
		{"not JSON", `<html>bad gateway</html>`},
		{"empty body", ``},
		{"JSON array", `[]`},
		{"no choices", `{"model":"gpt-4o","choices":[],"usage":` + okUsage + `}`},
		{"missing choices", `{"model":"gpt-4o","usage":` + okUsage + `}`},
		{"two choices", `{"model":"gpt-4o","choices":[` +
			`{"message":{"role":"assistant","content":"a"},"finish_reason":"stop"},` +
			`{"message":{"role":"assistant","content":"b"},"finish_reason":"stop"}],"usage":` + okUsage + `}`},
		{"missing message", `{"model":"gpt-4o","choices":[{"finish_reason":"stop"}],"usage":` + okUsage + `}`},
		{"wrong role", responseWith(`{"role":"user","content":"hi"}`, "stop", okUsage)},
		{"null content", responseWith(`{"role":"assistant","content":null}`, "stop", okUsage)},
		{"tool calls", responseWith(`{"role":"assistant","content":null,"tool_calls":[{"id":"call_1","type":"function","function":{"name":"f","arguments":"{}"}}]}`, "tool_calls", okUsage)},
		{"tool calls with text", responseWith(`{"role":"assistant","content":"hi","tool_calls":[{"id":"call_1","type":"function","function":{"name":"f","arguments":"{}"}}]}`, "stop", okUsage)},
		{"function call", responseWith(`{"role":"assistant","content":null,"function_call":{"name":"f","arguments":"{}"}}`, "function_call", okUsage)},
		{"wrong content type", responseWith(`{"role":"assistant","content":["hi"]}`, "stop", okUsage)},
		{"missing usage", `{"model":"gpt-4o","choices":[{"message":{"role":"assistant","content":"hi"},"finish_reason":"stop"}]}`},
		{"null usage", responseWith(`{"role":"assistant","content":"hi"}`, "stop", `null`)},
		{"partial usage", responseWith(`{"role":"assistant","content":"hi"}`, "stop", `{"prompt_tokens":3}`)},
		{"negative usage", responseWith(`{"role":"assistant","content":"hi"}`, "stop", `{"prompt_tokens":3,"completion_tokens":-1}`)},
		{"negative cached tokens", responseWith(`{"role":"assistant","content":"hi"}`, "stop", `{"prompt_tokens":3,"completion_tokens":4,"prompt_tokens_details":{"cached_tokens":-1}}`)},
		{"more cached than prompt tokens", responseWith(`{"role":"assistant","content":"hi"}`, "stop", `{"prompt_tokens":3,"completion_tokens":4,"prompt_tokens_details":{"cached_tokens":4}}`)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := newServer(t, respond(http.StatusOK, tt.body))

			got, err := c.Chat(context.Background(), testRequest)

			assertProviderError(t, err, http.StatusOK)
			if got != (llm.ChatResponse{}) {
				t.Errorf("Chat() = %+v, want zero response", got)
			}
		})
	}
}

func TestChatResponseSizeLimit(t *testing.T) {
	// padded returns successBody followed by whitespace, n bytes in total.
	padded := func(n int) string {
		return successBody + strings.Repeat(" ", n-len(successBody))
	}

	t.Run("at limit", func(t *testing.T) {
		c := newServer(t, respond(http.StatusOK, padded(maxResponseBytes)))
		if _, err := c.Chat(context.Background(), testRequest); err != nil {
			t.Fatalf("Chat() error = %v", err)
		}
	})

	t.Run("over limit", func(t *testing.T) {
		c := newServer(t, respond(http.StatusOK, padded(maxResponseBytes+1)))
		_, err := c.Chat(context.Background(), testRequest)
		assertProviderError(t, err, http.StatusOK)
	})
}

func TestChatTransportFailure(t *testing.T) {
	srv := httptest.NewServer(respond(http.StatusOK, successBody))
	c, err := New(testKey, srv.URL, srv.Client())
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	srv.Close()

	_, err = c.Chat(context.Background(), testRequest)

	assertProviderError(t, err, 0)
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("transport failure reported as a context error: %v", err)
	}
}

func TestChatRejectsInvalidRequestWithoutCallingUpstream(t *testing.T) {
	tests := []struct {
		name string
		req  llm.ChatRequest
	}{
		{"blank model", llm.ChatRequest{Model: " ", Messages: testRequest.Messages, MaxTokens: 1}},
		{"zero max tokens", llm.ChatRequest{Model: "m", Messages: testRequest.Messages}},
		{"no messages", llm.ChatRequest{Model: "m", MaxTokens: 1}},
		{"unknown role", llm.ChatRequest{Model: "m", Messages: []llm.Message{{Role: "tool", Content: "x"}}, MaxTokens: 1}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			called := false
			c := newServer(t, func(w http.ResponseWriter, r *http.Request) { called = true })

			_, err := c.Chat(context.Background(), tt.req)

			if err == nil {
				t.Fatal("Chat() error = nil, want error")
			}
			if _, ok := errors.AsType[*llm.ProviderError](err); ok {
				t.Errorf("error = %v, want a non-provider error", err)
			}
			if called {
				t.Error("upstream was called")
			}
		})
	}
}

// blockingServer starts an upstream whose handler runs before, signals
// started, and then blocks until release is closed or the client goes
// away. The returned Client closes headers once it has received the
// response headers.
func blockingServer(t *testing.T, before func(w http.ResponseWriter)) (c *Client, started, headers <-chan struct{}, release func()) {
	t.Helper()
	startedCh := make(chan struct{})
	headersCh := make(chan struct{})
	releaseCh := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		before(w)
		close(startedCh)
		select {
		case <-releaseCh:
			_, _ = io.WriteString(w, successBody)
		case <-r.Context().Done():
		}
	}))
	t.Cleanup(srv.Close)

	transport := srv.Client().Transport
	httpClient := &http.Client{Transport: roundTripperFunc(func(r *http.Request) (*http.Response, error) {
		resp, err := transport.RoundTrip(r)
		if err == nil {
			close(headersCh)
		}
		return resp, err
	})}
	c, err := New(testKey, srv.URL, httpClient)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}

	var once sync.Once
	release = func() { once.Do(func() { close(releaseCh) }) }
	// Registered after srv.Close, so it runs first and unblocks the handler
	// before the server shuts down.
	t.Cleanup(release)
	return c, startedCh, headersCh, release
}

type roundTripperFunc func(*http.Request) (*http.Response, error)

func (f roundTripperFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// chatAsync runs c.Chat in a goroutine and returns a channel for its error.
func chatAsync(ctx context.Context, c *Client) <-chan error {
	done := make(chan error, 1)
	go func() {
		_, err := c.Chat(ctx, testRequest)
		done <- err
	}()
	return done
}

func waitFor[T any](t *testing.T, ch <-chan T, what string) T {
	t.Helper()
	select {
	case v := <-ch:
		return v
	case <-time.After(guard):
		t.Fatalf("timed out waiting for %s", what)
		panic("unreachable")
	}
}

func TestChatSlowUpstreamSucceedsWhileContextActive(t *testing.T) {
	c, started, _, release := blockingServer(t, func(w http.ResponseWriter) {})

	done := chatAsync(context.Background(), c)
	waitFor(t, started, "upstream to receive the request")
	select {
	case err := <-done:
		t.Fatalf("Chat() returned before the upstream responded: %v", err)
	default:
	}
	release()

	if err := waitFor(t, done, "Chat to return"); err != nil {
		t.Fatalf("Chat() error = %v", err)
	}
}

func TestChatCancellation(t *testing.T) {
	tests := []struct {
		name        string
		before      func(w http.ResponseWriter)
		readingBody bool
		status      int
	}{
		{
			name:   "waiting for response headers",
			before: func(w http.ResponseWriter) {},
			status: 0,
		},
		{
			name: "reading response body",
			before: func(w http.ResponseWriter) {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusOK)
				_, _ = io.WriteString(w, `{"model":"gpt-4o","choices":[`)
				w.(http.Flusher).Flush()
			},
			readingBody: true,
			status:      http.StatusOK,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c, started, headers, _ := blockingServer(t, tt.before)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()

			done := chatAsync(ctx, c)
			waitFor(t, started, "upstream to receive the request")
			if tt.readingBody {
				waitFor(t, headers, "client to receive response headers")
			}
			cancel()
			err := waitFor(t, done, "Chat to return after cancellation")

			assertProviderError(t, err, tt.status)
			if !errors.Is(err, context.Canceled) {
				t.Errorf("errors.Is(err, context.Canceled) = false for %v", err)
			}
		})
	}
}

func TestChatPreservesDeadline(t *testing.T) {
	// The deadline may expire before or after the upstream receives the
	// request; both must report DeadlineExceeded.
	c, _, _, _ := blockingServer(t, func(w http.ResponseWriter) {})
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	done := chatAsync(ctx, c)
	err := waitFor(t, done, "Chat to return after the deadline")

	assertProviderError(t, err, 0)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("errors.Is(err, context.DeadlineExceeded) = false for %v", err)
	}
}
