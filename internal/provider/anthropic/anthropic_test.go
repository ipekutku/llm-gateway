package anthropic

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

const testKey = "sk-ant-test-not-a-real-key"

// guard bounds how long a test waits for something that should happen
// promptly. It is a failure guard, not a synchronization mechanism.
const guard = 5 * time.Second

// successBody is a non-streaming response modeled on the official example,
// with a thinking block as returned by models that always think.
const successBody = `{
  "id": "msg_01XFDUDYJgAACzvnptvVoYEL",
  "type": "message",
  "role": "assistant",
  "model": "claude-opus-5-5",
  "content": [
    {"type": "thinking", "thinking": "", "signature": "EqQBCgIYAhIM1gbcDa9GJwZA2b3hGgxBdjrkzLoky3dl1pkiMOYds"},
    {"type": "text", "text": "TCP provides reliable, "},
    {"type": "text", "text": "ordered delivery."}
  ],
  "stop_reason": "end_turn",
  "stop_sequence": null,
  "usage": {
    "input_tokens": 12,
    "output_tokens": 8,
    "cache_creation_input_tokens": 0,
    "cache_read_input_tokens": 0
  }
}`

var testRequest = llm.ChatRequest{
	Model: "claude-opus-5-5",
	Messages: []llm.Message{
		{Role: llm.RoleSystem, Content: "Answer concisely."},
		{Role: llm.RoleUser, Content: "Explain TCP."},
		{Role: llm.RoleAssistant, Content: "Sure."},
		{Role: llm.RoleUser, Content: "Go on."},
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

// capture records the decoded body of each upstream request.
type capture struct {
	mu     sync.Mutex
	header http.Header
	method string
	path   string
	body   map[string]any
}

func (c *capture) handler(t *testing.T, respondWith http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("decode upstream request: %v", err)
		}
		c.mu.Lock()
		c.header, c.method, c.path, c.body = r.Header.Clone(), r.Method, r.URL.Path, body
		c.mu.Unlock()
		respondWith(w, r)
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
		{name: "production origin", apiKey: "k", baseURL: DefaultBaseURL, want: "https://api.anthropic.com/v1/messages"},
		{name: "trailing slash", apiKey: "k", baseURL: "https://api.anthropic.com/", want: "https://api.anthropic.com/v1/messages"},
		{name: "path prefix", apiKey: "k", baseURL: "http://127.0.0.1:9000/proxy", want: "http://127.0.0.1:9000/proxy/v1/messages"},
		{name: "blank key", apiKey: " ", baseURL: DefaultBaseURL, wantErr: true},
		{name: "empty URL", apiKey: "k", baseURL: "", wantErr: true},
		{name: "no scheme", apiKey: "k", baseURL: "api.anthropic.com", wantErr: true},
		{name: "unsupported scheme", apiKey: "k", baseURL: "ftp://api.anthropic.com", wantErr: true},
		{name: "no host", apiKey: "k", baseURL: "https://", wantErr: true},
		{name: "query", apiKey: "k", baseURL: "https://api.anthropic.com?x=1", wantErr: true},
		{name: "credentials", apiKey: "k", baseURL: "https://user:pass@api.anthropic.com", wantErr: true},
		{name: "unparsable", apiKey: "k", baseURL: "https://api.anthropic.com/%zz", wantErr: true},
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
	var got capture
	c := newServer(t, got.handler(t, respond(http.StatusOK, successBody)))

	req := testRequest
	req.Messages = append([]llm.Message(nil), testRequest.Messages...)
	if _, err := c.Chat(context.Background(), req); err != nil {
		t.Fatalf("Chat() error = %v", err)
	}

	got.mu.Lock()
	defer got.mu.Unlock()
	if got.method != http.MethodPost || got.path != endpointPath {
		t.Errorf("request = %s %s, want POST %s", got.method, got.path, endpointPath)
	}
	for name, want := range map[string]string{
		"x-api-key":         testKey,
		"anthropic-version": APIVersion,
		"Content-Type":      "application/json",
	} {
		if v := got.header.Get(name); v != want {
			t.Errorf("%s = %q, want %q", name, v, want)
		}
	}
	if v := got.header.Get("Authorization"); v != "" {
		t.Errorf("Authorization = %q, want unset", v)
	}
	want := map[string]any{
		"model":  "claude-opus-5-5",
		"system": "Answer concisely.",
		"messages": []any{
			map[string]any{"role": "user", "content": "Explain TCP."},
			map[string]any{"role": "assistant", "content": "Sure."},
			map[string]any{"role": "user", "content": "Go on."},
		},
		"max_tokens": float64(100),
	}
	if !reflect.DeepEqual(got.body, want) {
		t.Errorf("upstream body = %v, want %v", got.body, want)
	}
	if !reflect.DeepEqual(req, testRequest) {
		t.Errorf("Chat() mutated the request: %+v", req)
	}
}

func TestChatOmitsSystemWhenAbsent(t *testing.T) {
	var got capture
	c := newServer(t, got.handler(t, respond(http.StatusOK, successBody)))

	req := llm.ChatRequest{
		Model:     "claude-opus-5-5",
		Messages:  []llm.Message{{Role: llm.RoleUser, Content: "  hi\n"}},
		MaxTokens: 1024,
	}
	if _, err := c.Chat(context.Background(), req); err != nil {
		t.Fatalf("Chat() error = %v", err)
	}

	got.mu.Lock()
	defer got.mu.Unlock()
	want := map[string]any{
		"model":      "claude-opus-5-5",
		"messages":   []any{map[string]any{"role": "user", "content": "  hi\n"}},
		"max_tokens": float64(1024),
	}
	if !reflect.DeepEqual(got.body, want) {
		t.Errorf("upstream body = %v, want %v", got.body, want)
	}
}

func TestChatUsesPathPrefix(t *testing.T) {
	var got capture
	srv := httptest.NewServer(got.handler(t, respond(http.StatusOK, successBody)))
	t.Cleanup(srv.Close)
	c, err := New(testKey, srv.URL+"/proxy/", srv.Client())
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}

	if _, err := c.Chat(context.Background(), testRequest); err != nil {
		t.Fatalf("Chat() error = %v", err)
	}
	got.mu.Lock()
	defer got.mu.Unlock()
	if got.path != "/proxy/v1/messages" {
		t.Errorf("path = %q, want /proxy/v1/messages", got.path)
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
		Model:        "claude-opus-5-5",
		Message:      llm.Message{Role: llm.RoleAssistant, Content: "TCP provides reliable, ordered delivery."},
		FinishReason: llm.FinishReasonStop,
		Usage:        llm.Usage{InputTokens: 12, OutputTokens: 8},
	}
	if got != want {
		t.Errorf("Chat() = %+v, want %+v", got, want)
	}
}

// responseWith returns a successful response body with the given content
// JSON, stop reason, and usage JSON.
func responseWith(content, stopReason, usage string) string {
	return `{"type":"message","role":"assistant","model":"claude-opus-5-5","content":` + content +
		`,"stop_reason":"` + stopReason + `","usage":` + usage + `}`
}

const (
	okContent = `[{"type":"text","text":"hi"}]`
	okUsage   = `{"input_tokens":3,"output_tokens":4}`
)

func TestChatContentBlocks(t *testing.T) {
	tests := []struct {
		name    string
		content string
		want    string
	}{
		{"single text", `[{"type":"text","text":"hello"}]`, "hello"},
		{"text blocks joined without separator", `[{"type":"text","text":"a "},{"type":"text","text":"b"},{"type":"text","text":"\nc"}]`, "a b\nc"},
		{"thinking skipped", `[{"type":"thinking","thinking":"secret reasoning","signature":"x"},{"type":"text","text":"answer"}]`, "answer"},
		{"redacted thinking skipped", `[{"type":"redacted_thinking","data":"abc"},{"type":"text","text":"answer"}]`, "answer"},
		{"empty text", `[{"type":"text","text":""}]`, ""},
		{"no blocks", `[]`, ""},
		{"thinking only", `[{"type":"thinking","thinking":"","signature":"x"}]`, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := newServer(t, respond(http.StatusOK, responseWith(tt.content, "end_turn", okUsage)))

			got, err := c.Chat(context.Background(), testRequest)
			if err != nil {
				t.Fatalf("Chat() error = %v", err)
			}
			if got.Message.Content != tt.want {
				t.Errorf("Content = %q, want %q", got.Message.Content, tt.want)
			}
		})
	}
}

func TestChatStopReasons(t *testing.T) {
	tests := []struct {
		reason  string
		want    string
		wantErr bool
	}{
		{reason: "end_turn", want: llm.FinishReasonStop},
		{reason: "stop_sequence", want: llm.FinishReasonStop},
		{reason: "max_tokens", want: llm.FinishReasonLength},
		{reason: "model_context_window_exceeded", want: llm.FinishReasonLength},
		{reason: "refusal", want: llm.FinishReasonContentFilter},
		{reason: "tool_use", wantErr: true},
		{reason: "pause_turn", wantErr: true},
		{reason: "", wantErr: true},
		{reason: "something_new", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.reason, func(t *testing.T) {
			c := newServer(t, respond(http.StatusOK, responseWith(okContent, tt.reason, okUsage)))

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

func TestChatRefusalKeepsPartialText(t *testing.T) {
	c := newServer(t, respond(http.StatusOK, `{"type":"message","role":"assistant","model":"claude-opus-5-5",`+
		`"content":[{"type":"text","text":"Here is"}],"stop_reason":"refusal",`+
		`"stop_details":{"type":"refusal","category":"cyber","explanation":"..."},"usage":`+okUsage+`}`))

	got, err := c.Chat(context.Background(), testRequest)
	if err != nil {
		t.Fatalf("Chat() error = %v", err)
	}
	if got.Message.Content != "Here is" || got.FinishReason != llm.FinishReasonContentFilter {
		t.Errorf("Chat() = %+v, want partial text with content_filter", got)
	}
}

func TestChatUsageIncludesCachedInput(t *testing.T) {
	tests := []struct {
		name  string
		usage string
		want  llm.Usage
	}{
		{"reads and writes", `{"input_tokens":10,"output_tokens":4,"cache_creation_input_tokens":100,"cache_read_input_tokens":1000}`,
			llm.Usage{InputTokens: 1110, CacheReadInputTokens: 1000, CacheWriteInputTokens: 100, OutputTokens: 4}},
		{"no cache fields", okUsage, llm.Usage{InputTokens: 3, OutputTokens: 4}},
		{"null cache fields", `{"input_tokens":3,"output_tokens":4,"cache_creation_input_tokens":null,"cache_read_input_tokens":null}`,
			llm.Usage{InputTokens: 3, OutputTokens: 4}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := newServer(t, respond(http.StatusOK, responseWith(okContent, "end_turn", tt.usage)))

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
	const upstreamBody = `{"type":"error","error":{"type":"authentication_error","message":"invalid x-api-key sk-ant-test-not-a-real-key"}}`
	for _, status := range []int{400, 401, 403, 404, 413, 429, 500, 529} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			c := newServer(t, respond(status, upstreamBody))

			_, err := c.Chat(context.Background(), testRequest)

			assertProviderError(t, err, status)
			if strings.Contains(err.Error(), "invalid x-api-key") || strings.Contains(err.Error(), testKey) {
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
		{"malformed JSON", `{"content":[`},
		{"not JSON", `<html>overloaded</html>`},
		{"empty body", ``},
		{"JSON array", `[]`},
		{"error object with 200", `{"type":"error","error":{"type":"overloaded_error","message":"Overloaded"}}`},
		{"wrong role", `{"type":"message","role":"user","content":` + okContent + `,"stop_reason":"end_turn","usage":` + okUsage + `}`},
		{"missing content", `{"type":"message","role":"assistant","stop_reason":"end_turn","usage":` + okUsage + `}`},
		{"null content", responseWith(`null`, "end_turn", okUsage)},
		{"content not array", responseWith(`"hi"`, "end_turn", okUsage)},
		{"text block without text", responseWith(`[{"type":"text"}]`, "end_turn", okUsage)},
		{"text block with null text", responseWith(`[{"type":"text","text":null}]`, "end_turn", okUsage)},
		{"tool use block", responseWith(`[{"type":"text","text":"Let me check."},{"type":"tool_use","id":"toolu_1","name":"f","input":{}}]`, "tool_use", okUsage)},
		{"server tool block", responseWith(`[{"type":"server_tool_use","id":"srvtoolu_1","name":"web_search","input":{}}]`, "end_turn", okUsage)},
		{"unknown block", responseWith(`[{"type":"hologram"}]`, "end_turn", okUsage)},
		{"missing usage", `{"type":"message","role":"assistant","content":` + okContent + `,"stop_reason":"end_turn"}`},
		{"null usage", responseWith(okContent, "end_turn", `null`)},
		{"missing output tokens", responseWith(okContent, "end_turn", `{"input_tokens":3}`)},
		{"missing input tokens", responseWith(okContent, "end_turn", `{"output_tokens":3}`)},
		{"negative usage", responseWith(okContent, "end_turn", `{"input_tokens":-3,"output_tokens":4}`)},
		{"negative cache reads", responseWith(okContent, "end_turn", `{"input_tokens":3,"output_tokens":4,"cache_read_input_tokens":-1}`)},
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
	user := llm.Message{Role: llm.RoleUser, Content: "hi"}
	system := llm.Message{Role: llm.RoleSystem, Content: "be brief"}
	tests := []struct {
		name string
		req  llm.ChatRequest
	}{
		{"blank model", llm.ChatRequest{Model: " ", Messages: []llm.Message{user}, MaxTokens: 1}},
		{"zero max tokens", llm.ChatRequest{Model: "m", Messages: []llm.Message{user}}},
		{"no messages", llm.ChatRequest{Model: "m", MaxTokens: 1}},
		{"system only", llm.ChatRequest{Model: "m", Messages: []llm.Message{system}, MaxTokens: 1}},
		{"system not first", llm.ChatRequest{Model: "m", Messages: []llm.Message{user, system}, MaxTokens: 1}},
		{"unknown role", llm.ChatRequest{Model: "m", Messages: []llm.Message{{Role: "tool", Content: "x"}}, MaxTokens: 1}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var calls atomic.Int32
			c := newServer(t, func(w http.ResponseWriter, r *http.Request) { calls.Add(1) })

			_, err := c.Chat(context.Background(), tt.req)

			if err == nil {
				t.Fatal("Chat() error = nil, want error")
			}
			if _, ok := errors.AsType[*llm.ProviderError](err); ok {
				t.Errorf("error = %v, want a non-provider error", err)
			}
			if calls.Load() != 0 {
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
				_, _ = io.WriteString(w, `{"type":"message","role":"assistant","content":[`)
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
