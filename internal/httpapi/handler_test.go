package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"reflect"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/ipekutku/llm-gateway/internal/auth"
	"github.com/ipekutku/llm-gateway/internal/llm"
	"github.com/ipekutku/llm-gateway/internal/ratelimit"
	"github.com/ipekutku/llm-gateway/internal/routing"
	"github.com/ipekutku/llm-gateway/internal/usage"
)

// providerFunc adapts a function to llm.Provider.
type providerFunc func(ctx context.Context, req llm.ChatRequest) (llm.ChatResponse, error)

func (f providerFunc) Chat(ctx context.Context, req llm.ChatRequest) (llm.ChatResponse, error) {
	return f(ctx, req)
}

// okProvider answers every request successfully.
var okProvider = providerFunc(func(_ context.Context, req llm.ChatRequest) (llm.ChatResponse, error) {
	return llm.ChatResponse{
		Model:        req.Model,
		Message:      llm.Message{Role: llm.RoleAssistant, Content: "ok"},
		FinishReason: llm.FinishReasonStop,
	}, nil
})

// failingProvider returns err for every request.
func failingProvider(err error) llm.Provider {
	return providerFunc(func(context.Context, llm.ChatRequest) (llm.ChatResponse, error) {
		return llm.ChatResponse{}, err
	})
}

// recordingProvider records the last request it received and answers it.
type recordingProvider struct {
	calls int
	req   llm.ChatRequest
}

func (p *recordingProvider) Chat(ctx context.Context, req llm.ChatRequest) (llm.ChatResponse, error) {
	p.calls++
	p.req = req
	return okProvider(ctx, req)
}

// testTimeout is the upstream timeout for tests that do not exercise it. It
// is long enough never to expire.
const testTimeout = time.Minute

type discardRecorder struct{}

func (discardRecorder) Record(usage.Record) bool { return true }
func testAccounting() Accounting {
	pricing, _ := usage.NewPricing(nil)
	return Accounting{Recorder: discardRecorder{}, Pricing: pricing}
}

func TestNewRequiresAccounting(t *testing.T) {
	a := testAuthenticator(t)
	l := testLimiter(t, map[string]ratelimit.Limits{"team-a": generous})
	for _, accounting := range []Accounting{{}, {Recorder: discardRecorder{}}, {Pricing: testAccounting().Pricing}} {
		if _, err := New(okProvider, a, l, testTimeout, accounting, nil); err == nil {
			t.Error("New accepted incomplete accounting dependencies")
		}
	}
}

// Gateway API keys of the test clients.
const (
	keyA        = "test-key-team-a"
	keyB        = "test-key-team-b"
	keyDisabled = "test-key-team-old"
)

func testAuthenticator(t *testing.T) *auth.Authenticator {
	t.Helper()
	a, err := auth.New([]auth.Client{
		{ID: "team-a", KeyHash: auth.HashKey(keyA)},
		{ID: "team-b", KeyHash: auth.HashKey(keyB)},
		{ID: "team-old", KeyHash: auth.HashKey(keyDisabled), Disabled: true},
	})
	if err != nil {
		t.Fatalf("auth.New() error = %v", err)
	}
	return a
}

// generous are limits that tests not about rate limiting never reach.
var generous = ratelimit.Limits{RequestsPerMinute: ratelimit.MaxLimit, Burst: ratelimit.MaxLimit, MaxConcurrent: ratelimit.MaxLimit}

func testLimiter(t *testing.T, limits map[string]ratelimit.Limits) *ratelimit.Limiter {
	t.Helper()
	l, err := ratelimit.New(limits)
	if err != nil {
		t.Fatalf("ratelimit.New() error = %v", err)
	}
	return l
}

// newHandler returns a handler for p and a buffer that collects its logs.
func newHandler(t *testing.T, p llm.Provider) (http.Handler, *bytes.Buffer) {
	t.Helper()
	return newHandlerWithTimeout(t, p, testTimeout)
}

func newHandlerWithTimeout(t *testing.T, p llm.Provider, timeout time.Duration) (http.Handler, *bytes.Buffer) {
	t.Helper()
	limiter := testLimiter(t, map[string]ratelimit.Limits{"team-a": generous, "team-b": generous, "team-old": generous})
	return newHandlerWith(t, p, limiter, timeout)
}

func newHandlerWith(t *testing.T, p llm.Provider, limiter *ratelimit.Limiter, timeout time.Duration) (http.Handler, *bytes.Buffer) {
	t.Helper()
	var logs bytes.Buffer
	h, err := New(p, testAuthenticator(t), limiter, timeout, testAccounting(), slog.New(slog.NewTextHandler(&logs, nil)))
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	return h, &logs
}

func post(t *testing.T, h http.Handler, body string) *httptest.ResponseRecorder {
	t.Helper()
	return serve(t, h, httptest.NewRequest(http.MethodPost, ChatCompletionsPath, strings.NewReader(body)))
}

// serve serves r, authenticated as team-a unless r already has an
// Authorization header.
func serve(t *testing.T, h http.Handler, r *http.Request) *httptest.ResponseRecorder {
	t.Helper()
	if _, ok := r.Header["Authorization"]; !ok {
		r.Header.Set("Authorization", "Bearer "+keyA)
	}
	return serveRaw(h, r)
}

// serveRaw serves r with its headers unchanged.
func serveRaw(h http.Handler, r *http.Request) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, r)
	return rec
}

func assertError(t *testing.T, rec *httptest.ResponseRecorder, status int, typ, code string) errorBody {
	t.Helper()
	if rec.Code != status {
		t.Errorf("status = %d, want %d (body %s)", rec.Code, status, rec.Body)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", ct)
	}
	var got errorResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode error response: %v (body %s)", err, rec.Body)
	}
	if got.Error.Type != typ || got.Error.Code != code {
		t.Errorf("error type/code = %q/%q, want %q/%q", got.Error.Type, got.Error.Code, typ, code)
	}
	if got.Error.Message == "" {
		t.Error("error message is empty")
	}
	return got.Error
}

const validBody = `{"model":"model-a","messages":[{"role":"user","content":"hi"}]}`

func TestNewRejectsNilDependencies(t *testing.T) {
	a := testAuthenticator(t)
	l := testLimiter(t, map[string]ratelimit.Limits{"team-a": generous})
	if _, err := New(nil, a, l, testTimeout, testAccounting(), nil); err == nil {
		t.Error("New(nil provider) error = nil, want error")
	}
	if _, err := New(okProvider, nil, l, testTimeout, testAccounting(), nil); err == nil {
		t.Error("New(nil authenticator) error = nil, want error")
	}
	if _, err := New(okProvider, a, nil, testTimeout, testAccounting(), nil); err == nil {
		t.Error("New(nil limiter) error = nil, want error")
	}
}

func TestNewRejectsNonPositiveTimeout(t *testing.T) {
	a := testAuthenticator(t)
	l := testLimiter(t, map[string]ratelimit.Limits{"team-a": generous})
	for _, timeout := range []time.Duration{0, -time.Second} {
		if _, err := New(okProvider, a, l, timeout, testAccounting(), nil); err == nil {
			t.Errorf("New(timeout %v) error = nil, want error", timeout)
		}
	}
}

func TestChatCompletionsSuccess(t *testing.T) {
	var got llm.ChatRequest
	h, _ := newHandler(t, providerFunc(func(_ context.Context, req llm.ChatRequest) (llm.ChatResponse, error) {
		got = req
		return llm.ChatResponse{
			Model:        "model-a-2026",
			Message:      llm.Message{Role: llm.RoleAssistant, Content: "TCP provides reliable, ordered delivery."},
			FinishReason: llm.FinishReasonLength,
			Usage:        llm.Usage{InputTokens: 12, OutputTokens: 8},
		}, nil
	}))

	before := time.Now().Unix()
	rec := post(t, h, `{
		"model": "model-a",
		"messages": [
			{"role": "system", "content": "Answer concisely."},
			{"role": "user", "content": "Explain TCP."},
			{"role": "assistant", "content": "Sure."},
			{"role": "user", "content": "Go on."}
		],
		"max_tokens": 100,
		"stream": false
	}`)
	after := time.Now().Unix()

	wantReq := llm.ChatRequest{
		Model: "model-a",
		Messages: []llm.Message{
			{Role: llm.RoleSystem, Content: "Answer concisely."},
			{Role: llm.RoleUser, Content: "Explain TCP."},
			{Role: llm.RoleAssistant, Content: "Sure."},
			{Role: llm.RoleUser, Content: "Go on."},
		},
		MaxTokens: 100,
	}
	if !reflect.DeepEqual(got, wantReq) {
		t.Errorf("provider request = %+v, want %+v", got, wantReq)
	}

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %s)", rec.Code, rec.Body)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", ct)
	}

	var fields map[string]json.RawMessage
	if err := json.Unmarshal(rec.Body.Bytes(), &fields); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	var keys []string
	for k := range fields {
		keys = append(keys, k)
	}
	wantKeys := []string{"choices", "created", "id", "model", "object", "usage"}
	if !sameSet(keys, wantKeys) {
		t.Errorf("response fields = %v, want %v", keys, wantKeys)
	}

	var resp chatResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if !regexp.MustCompile(`^chatcmpl-[A-Z2-7]{26}$`).MatchString(resp.ID) {
		t.Errorf("id = %q, want chatcmpl-<26 base32 characters>", resp.ID)
	}
	if got := rec.Header().Get(RequestIDHeader); "chatcmpl-"+got != resp.ID {
		t.Errorf("%s = %q, want the id %q without its prefix", RequestIDHeader, got, resp.ID)
	}
	if resp.Created < before || resp.Created > after {
		t.Errorf("created = %d, want within [%d, %d]", resp.Created, before, after)
	}
	resp.ID, resp.Created = "", 0
	want := chatResponse{
		Object: "chat.completion",
		Model:  "model-a-2026",
		Choices: []chatChoice{{
			Index:        0,
			Message:      chatResponseMessage{Role: "assistant", Content: "TCP provides reliable, ordered delivery."},
			FinishReason: "length",
		}},
		Usage: chatUsage{PromptTokens: 12, CompletionTokens: 8, TotalTokens: 20},
	}
	if !reflect.DeepEqual(resp, want) {
		t.Errorf("response = %+v, want %+v", resp, want)
	}
}

func sameSet(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	seen := make(map[string]bool, len(a))
	for _, s := range a {
		seen[s] = true
	}
	for _, s := range b {
		if !seen[s] {
			return false
		}
	}
	return true
}

func TestChatCompletionsFallsBackToRequestedModel(t *testing.T) {
	h, _ := newHandler(t, providerFunc(func(context.Context, llm.ChatRequest) (llm.ChatResponse, error) {
		return llm.ChatResponse{Message: llm.Message{Role: llm.RoleAssistant}, FinishReason: llm.FinishReasonStop}, nil
	}))

	rec := post(t, h, validBody)

	var resp chatResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if resp.Model != "model-a" {
		t.Errorf("model = %q, want model-a", resp.Model)
	}
	if len(resp.Choices) != 1 || resp.Choices[0].Message.Content != "" {
		t.Errorf("choices = %+v, want one choice with empty content", resp.Choices)
	}
}

func TestChatCompletionsGeneratesUniqueIDs(t *testing.T) {
	h, _ := newHandler(t, okProvider)

	ids := make(map[string]bool)
	for range 10 {
		var resp chatResponse
		if err := json.Unmarshal(post(t, h, validBody).Body.Bytes(), &resp); err != nil {
			t.Fatalf("decode response: %v", err)
		}
		if ids[resp.ID] {
			t.Fatalf("duplicate id %q", resp.ID)
		}
		ids[resp.ID] = true
	}
}

var requestIDPattern = regexp.MustCompile(`^[A-Z2-7]{26}$`)

func TestEveryResponseHasARequestID(t *testing.T) {
	h, _ := newHandler(t, failingProvider(&llm.ProviderError{Provider: "openai", StatusCode: http.StatusServiceUnavailable}))

	tests := []struct {
		name       string
		r          *http.Request
		wantStatus int
	}{
		{"upstream failure", httptest.NewRequest(http.MethodPost, ChatCompletionsPath, strings.NewReader(validBody)), http.StatusBadGateway},
		{"invalid request", httptest.NewRequest(http.MethodPost, ChatCompletionsPath, strings.NewReader(`{}`)), http.StatusBadRequest},
		{"missing key", requestWithAuth(""), http.StatusUnauthorized},
		{"unknown path", httptest.NewRequest(http.MethodPost, "/v1/other", nil), http.StatusNotFound},
		{"wrong method", httptest.NewRequest(http.MethodGet, ChatCompletionsPath, nil), http.StatusMethodNotAllowed},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec := serve(t, h, tt.r)
			if rec.Code != tt.wantStatus {
				t.Errorf("status = %d, want %d", rec.Code, tt.wantStatus)
			}
			if got := rec.Header().Get(RequestIDHeader); !requestIDPattern.MatchString(got) {
				t.Errorf("%s = %q, want 26 base32 characters", RequestIDHeader, got)
			}
		})
	}
}

func TestClientRequestIDIsIgnored(t *testing.T) {
	h, _ := newHandler(t, okProvider)
	r := httptest.NewRequest(http.MethodPost, ChatCompletionsPath, strings.NewReader(validBody))
	r.Header.Set(RequestIDHeader, "client-chosen")

	rec := serve(t, h, r)

	if got := rec.Header().Get(RequestIDHeader); !requestIDPattern.MatchString(got) {
		t.Errorf("%s = %q, want a gateway-assigned ID", RequestIDHeader, got)
	}
	if strings.Contains(rec.Body.String(), "client-chosen") {
		t.Errorf("response uses the client's request ID: %s", rec.Body)
	}
}

func TestLogsIncludeRequestID(t *testing.T) {
	t.Run("failure", func(t *testing.T) {
		h, logs := newHandler(t, failingProvider(&llm.ProviderError{Provider: "openai", StatusCode: http.StatusServiceUnavailable}))
		rec := post(t, h, validBody)
		if want := "request_id=" + rec.Header().Get(RequestIDHeader); !strings.Contains(logs.String(), want) {
			t.Errorf("failure log does not contain %s:\n%s", want, logs)
		}
	})

	t.Run("client cancellation", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		h, logs := newHandler(t, providerFunc(func(context.Context, llm.ChatRequest) (llm.ChatResponse, error) {
			cancel()
			return llm.ChatResponse{}, &llm.ProviderError{Provider: "openai", Err: context.Canceled}
		}))
		rec := serve(t, h, httptest.NewRequestWithContext(ctx, http.MethodPost, ChatCompletionsPath, strings.NewReader(validBody)))
		if want := "request_id=" + rec.Header().Get(RequestIDHeader); !strings.Contains(logs.String(), want) {
			t.Errorf("cancellation log does not contain %s:\n%s", want, logs)
		}
	})
}

func TestChatCompletionsForwardsContentUnchangedAndIgnoresUnknownFields(t *testing.T) {
	p := &recordingProvider{}
	h, _ := newHandler(t, p)

	rec := post(t, h, `{
		"model": " model a ",
		"messages": [{"role": "user", "content": "  hi\n\tthere  ", "name": "bob"}],
		"temperature": 0.2,
		"tools": [{"type": "function"}],
		"n": 3
	}`)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %s)", rec.Code, rec.Body)
	}
	want := llm.ChatRequest{
		Model:     " model a ",
		Messages:  []llm.Message{{Role: llm.RoleUser, Content: "  hi\n\tthere  "}},
		MaxTokens: defaultMaxTokens,
	}
	if !reflect.DeepEqual(p.req, want) {
		t.Errorf("provider request = %+v, want %+v", p.req, want)
	}
}

func TestChatCompletionsMaxTokens(t *testing.T) {
	tests := []struct {
		name      string
		field     string
		wantValue int
		wantError bool
	}{
		{name: "omitted", field: ``, wantValue: defaultMaxTokens},
		{name: "null", field: `,"max_tokens":null`, wantValue: defaultMaxTokens},
		{name: "one", field: `,"max_tokens":1`, wantValue: 1},
		{name: "large", field: `,"max_tokens":200000`, wantValue: 200000},
		{name: "zero", field: `,"max_tokens":0`, wantError: true},
		{name: "negative", field: `,"max_tokens":-5`, wantError: true},
		{name: "fraction", field: `,"max_tokens":1.5`, wantError: true},
		{name: "string", field: `,"max_tokens":"10"`, wantError: true},
		{name: "overflow", field: `,"max_tokens":1e40`, wantError: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := &recordingProvider{}
			h, _ := newHandler(t, p)

			rec := post(t, h, `{"model":"m","messages":[{"role":"user","content":"hi"}]`+tt.field+`}`)

			if tt.wantError {
				assertError(t, rec, http.StatusBadRequest, typeInvalidRequest, codeInvalidRequest)
				if p.calls != 0 {
					t.Errorf("provider called %d times, want 0", p.calls)
				}
				return
			}
			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200 (body %s)", rec.Code, rec.Body)
			}
			if p.req.MaxTokens != tt.wantValue {
				t.Errorf("MaxTokens = %d, want %d", p.req.MaxTokens, tt.wantValue)
			}
		})
	}
}

func TestChatCompletionsDefaultMaxTokensIsSharedAcrossRoutes(t *testing.T) {
	a, b := &recordingProvider{}, &recordingProvider{}
	router, err := routing.New(map[string]routing.Route{"model-a": {Provider: a}, "model-b": {Provider: b}}, nil)
	if err != nil {
		t.Fatalf("routing.New() error = %v", err)
	}
	h, _ := newHandler(t, router)

	for _, model := range []string{"model-a", "model-b"} {
		rec := post(t, h, `{"model":"`+model+`","messages":[{"role":"user","content":"hi"}]}`)
		if rec.Code != http.StatusOK {
			t.Fatalf("%s: status = %d, want 200 (body %s)", model, rec.Code, rec.Body)
		}
	}
	if a.calls != 1 || b.calls != 1 {
		t.Fatalf("calls = %d/%d, want 1/1", a.calls, b.calls)
	}
	if a.req.MaxTokens != defaultMaxTokens || b.req.MaxTokens != defaultMaxTokens {
		t.Errorf("MaxTokens = %d/%d, want %d for both", a.req.MaxTokens, b.req.MaxTokens, defaultMaxTokens)
	}
}

func TestChatCompletionsRejectsInvalidRequests(t *testing.T) {
	tests := []struct {
		name    string
		body    string
		message string
	}{
		{"empty body", ``, "request body is empty"},
		{"whitespace body", " \n\t ", "request body is empty"},
		{"malformed JSON", `{"model":`, "request body is not valid JSON"},
		{"not JSON", `hello`, "request body is not valid JSON"},
		{"null", `null`, "request body must be a JSON object"},
		{"array", `[]`, "request body must be a JSON object"},
		{"string", `"hi"`, "request body must be a JSON object"},
		{"trailing object", validBody + ` {}`, "request body must contain a single JSON object"},
		{"trailing garbage", validBody + `x`, "request body must contain a single JSON object"},
		{"missing model", `{"messages":[{"role":"user","content":"hi"}]}`, "model is required"},
		{"null model", `{"model":null,"messages":[{"role":"user","content":"hi"}]}`, "model is required"},
		{"blank model", `{"model":"  ","messages":[{"role":"user","content":"hi"}]}`, "model is required"},
		{"numeric model", `{"model":7,"messages":[{"role":"user","content":"hi"}]}`, `field "model" has the wrong type`},
		{"stream true", `{"model":"m","stream":true,"messages":[{"role":"user","content":"hi"}]}`, "streaming is not supported"},
		{"stream string", `{"model":"m","stream":"yes","messages":[{"role":"user","content":"hi"}]}`, `field "stream" has the wrong type`},
		{"missing messages", `{"model":"m"}`, "messages must not be empty"},
		{"null messages", `{"model":"m","messages":null}`, "messages must not be empty"},
		{"empty messages", `{"model":"m","messages":[]}`, "messages must not be empty"},
		{"messages object", `{"model":"m","messages":{"role":"user"}}`, `field "messages" has the wrong type`},
		{"null message", `{"model":"m","messages":[null]}`, "messages[0].role is required"},
		{"missing role", `{"model":"m","messages":[{"content":"hi"}]}`, "messages[0].role is required"},
		{"unknown role", `{"model":"m","messages":[{"role":"tool","content":"hi"}]}`, "messages[0].role must be one of system, user, or assistant"},
		{"role wrong case", `{"model":"m","messages":[{"role":"User","content":"hi"}]}`, "messages[0].role must be one of system, user, or assistant"},
		{"system only", `{"model":"m","messages":[{"role":"system","content":"be brief"}]}`, "messages must contain at least one user or assistant message"},
		{"system not first", `{"model":"m","messages":[{"role":"user","content":"hi"},{"role":"system","content":"be brief"}]}`, "messages[1]: a system message is only allowed as the first message"},
		{"two system messages", `{"model":"m","messages":[{"role":"system","content":"a"},{"role":"system","content":"b"},{"role":"user","content":"hi"}]}`, "messages[1]: a system message is only allowed as the first message"},
		{"missing content", `{"model":"m","messages":[{"role":"user"}]}`, "messages[0].content must be a string"},
		{"null content", `{"model":"m","messages":[{"role":"user","content":null}]}`, "messages[0].content must be a string"},
		{"blank content", `{"model":"m","messages":[{"role":"user","content":" \n "}]}`, "messages[0].content must not be blank"},
		{"array content", `{"model":"m","messages":[{"role":"user","content":[{"type":"text","text":"hi"}]}]}`, `field "messages.content" has the wrong type`},
		{"object content", `{"model":"m","messages":[{"role":"user","content":{"text":"hi"}}]}`, `field "messages.content" has the wrong type`},
		{"numeric content", `{"model":"m","messages":[{"role":"user","content":1}]}`, `field "messages.content" has the wrong type`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := &recordingProvider{}
			h, _ := newHandler(t, p)

			rec := post(t, h, tt.body)

			got := assertError(t, rec, http.StatusBadRequest, typeInvalidRequest, codeInvalidRequest)
			if got.Message != tt.message {
				t.Errorf("message = %q, want %q", got.Message, tt.message)
			}
			if p.calls != 0 {
				t.Errorf("provider called %d times, want 0", p.calls)
			}
		})
	}
}

func TestChatCompletionsAcceptsSurroundingWhitespace(t *testing.T) {
	h, _ := newHandler(t, okProvider)

	rec := post(t, h, " \n"+validBody+"\n\t ")

	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want 200 (body %s)", rec.Code, rec.Body)
	}
}

func TestChatCompletionsBodyLimit(t *testing.T) {
	// padded returns validBody followed by whitespace, n bytes in total.
	padded := func(n int) string {
		return validBody + strings.Repeat(" ", n-len(validBody))
	}

	t.Run("at limit", func(t *testing.T) {
		h, _ := newHandler(t, okProvider)
		rec := post(t, h, padded(maxRequestBytes))
		if rec.Code != http.StatusOK {
			t.Errorf("status = %d, want 200 (body %s)", rec.Code, rec.Body)
		}
	})

	t.Run("over limit", func(t *testing.T) {
		p := &recordingProvider{}
		h, _ := newHandler(t, p)
		rec := post(t, h, padded(maxRequestBytes+1))
		assertError(t, rec, http.StatusRequestEntityTooLarge, typeInvalidRequest, codeRequestTooLarge)
		if p.calls != 0 {
			t.Errorf("provider called %d times, want 0", p.calls)
		}
	})
}

func TestChatCompletionsRouting(t *testing.T) {
	p := &recordingProvider{}
	h, _ := newHandler(t, p)

	for _, method := range []string{http.MethodGet, http.MethodPut, http.MethodDelete} {
		rec := serve(t, h, httptest.NewRequest(method, ChatCompletionsPath, nil))
		if rec.Code != http.StatusMethodNotAllowed {
			t.Errorf("%s: status = %d, want 405", method, rec.Code)
		}
		if allow := rec.Header().Get("Allow"); allow != http.MethodPost {
			t.Errorf("%s: Allow = %q, want POST", method, allow)
		}
	}

	for _, path := range []string{"/", "/v1/chat", "/v1/chat/completions/x", "/v1/completions"} {
		rec := serve(t, h, httptest.NewRequest(http.MethodPost, path, strings.NewReader(validBody)))
		if rec.Code != http.StatusNotFound {
			t.Errorf("%s: status = %d, want 404", path, rec.Code)
		}
	}

	if p.calls != 0 {
		t.Errorf("provider called %d times, want 0", p.calls)
	}
}

func TestChatCompletionsErrorMapping(t *testing.T) {
	providerErr := func(status int, cause error) error {
		return fmt.Errorf("routed: %w", &llm.ProviderError{Provider: "openai", StatusCode: status, Err: cause})
	}
	tests := []struct {
		name   string
		err    error
		status int
		typ    string
		code   string
	}{
		{"unknown model", fmt.Errorf("%w: %q", llm.ErrUnknownModel, "m"), 404, typeInvalidRequest, codeModelNotFound},
		{"upstream 400", providerErr(400, nil), 400, typeInvalidRequest, codeInvalidRequest},
		{"upstream 401", providerErr(401, nil), 502, typeServer, codeUpstreamError},
		{"upstream 403", providerErr(403, nil), 502, typeServer, codeUpstreamError},
		{"upstream 404", providerErr(404, nil), 502, typeServer, codeUpstreamError},
		{"upstream 429", providerErr(429, nil), 429, typeRateLimit, codeProviderRateLimited},
		{"upstream 500", providerErr(500, nil), 502, typeServer, codeUpstreamError},
		{"upstream 503", providerErr(503, nil), 502, typeServer, codeUpstreamError},
		{"transport failure", providerErr(0, errors.New("connection refused")), 502, typeServer, codeUpstreamError},
		{"malformed upstream response", providerErr(200, errors.New("decode response")), 502, typeServer, codeUpstreamError},
		{"upstream deadline", providerErr(0, context.DeadlineExceeded), 504, typeServer, codeUpstreamTimeout},
		{"bare deadline", fmt.Errorf("wrapped: %w", context.DeadlineExceeded), 504, typeServer, codeUpstreamTimeout},
		{"provider-only cancellation", providerErr(0, context.Canceled), 502, typeServer, codeUpstreamError},
		{"open circuit", fmt.Errorf("openai: %w", llm.ErrCircuitOpen), 503, typeServer, codeProviderUnavailable},
		{"open circuit on fallback", fmt.Errorf("fallback to %q failed: %w (primary failure: openai: upstream status 503)", "m", fmt.Errorf("anthropic: %w", llm.ErrCircuitOpen)), 503, typeServer, codeProviderUnavailable},
		{"unexpected error", errors.New("boom"), 500, typeServer, codeInternalError},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h, logs := newHandler(t, failingProvider(tt.err))

			rec := post(t, h, validBody)

			assertError(t, rec, tt.status, tt.typ, tt.code)
			if n := strings.Count(logs.String(), "request completed"); n != 1 {
				t.Errorf("logged the outcome %d times, want 1:\n%s", n, logs)
			}
			if !strings.Contains(logs.String(), "code="+tt.code) {
				t.Errorf("log does not contain code=%s:\n%s", tt.code, logs)
			}
		})
	}
}

func TestChatCompletionsDoesNotExposeUpstreamDetails(t *testing.T) {
	const secretDetail = "upstream said: invalid key sk-test-123"
	const prompt = "my private prompt"
	h, logs := newHandler(t, failingProvider(&llm.ProviderError{
		Provider:   "anthropic",
		StatusCode: 400,
		Err:        errors.New(secretDetail),
	}))

	rec := post(t, h, `{"model":"m","messages":[{"role":"user","content":"`+prompt+`"}]}`)

	got := assertError(t, rec, http.StatusBadRequest, typeInvalidRequest, codeInvalidRequest)
	if got.Message != errUpstreamRejected.message {
		t.Errorf("message = %q, want %q", got.Message, errUpstreamRejected.message)
	}
	if strings.Contains(rec.Body.String(), "sk-test-123") || strings.Contains(rec.Body.String(), "anthropic") {
		t.Errorf("response exposes upstream details: %s", rec.Body)
	}
	if strings.Contains(logs.String(), prompt) {
		t.Errorf("log contains prompt content:\n%s", logs)
	}
	for _, want := range []string{"provider=anthropic", "upstream_status=400", "model=m"} {
		if !strings.Contains(logs.String(), want) {
			t.Errorf("log does not contain %q:\n%s", want, logs)
		}
	}
}

func TestChatCompletionsPropagatesRequestContext(t *testing.T) {
	type ctxKey struct{}
	var got any
	h, _ := newHandler(t, providerFunc(func(ctx context.Context, req llm.ChatRequest) (llm.ChatResponse, error) {
		got = ctx.Value(ctxKey{})
		return okProvider(ctx, req)
	}))

	r := httptest.NewRequest(http.MethodPost, ChatCompletionsPath, strings.NewReader(validBody))
	r = r.WithContext(context.WithValue(r.Context(), ctxKey{}, "request-scoped"))
	serve(t, h, r)

	if got != "request-scoped" {
		t.Errorf("provider context value = %v, want request-scoped", got)
	}
}

func TestChatCompletionsBoundsProviderCallWithUpstreamTimeout(t *testing.T) {
	const timeout = 20 * time.Millisecond
	var remaining time.Duration
	h, logs := newHandlerWithTimeout(t, providerFunc(func(ctx context.Context, req llm.ChatRequest) (llm.ChatResponse, error) {
		deadline, ok := ctx.Deadline()
		if !ok {
			return llm.ChatResponse{}, errors.New("provider context has no deadline")
		}
		remaining = time.Until(deadline)
		<-ctx.Done() // A slow upstream that never answers.
		return llm.ChatResponse{}, &llm.ProviderError{Provider: "openai", Err: ctx.Err()}
	}), timeout)

	rec := post(t, h, validBody)

	assertError(t, rec, http.StatusGatewayTimeout, typeServer, codeUpstreamTimeout)
	if remaining <= 0 || remaining > timeout {
		t.Errorf("provider deadline in %v, want within (0, %v]", remaining, timeout)
	}
	if strings.Contains(logs.String(), "code=client_closed") {
		t.Errorf("upstream timeout logged as a client cancellation:\n%s", logs)
	}
}

func TestChatCompletionsSkipsResponseWhenClientCanceled(t *testing.T) {
	for _, tt := range []struct {
		name string
		err  error
	}{
		{"cancellation", context.Canceled},
		{"unrelated error after cancellation", errors.New("boom")},
	} {
		t.Run(tt.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			h, logs := newHandler(t, providerFunc(func(context.Context, llm.ChatRequest) (llm.ChatResponse, error) {
				cancel() // The client disconnects while the provider is working.
				return llm.ChatResponse{}, &llm.ProviderError{Provider: "openai", Err: tt.err}
			}))

			r := httptest.NewRequestWithContext(ctx, http.MethodPost, ChatCompletionsPath, strings.NewReader(validBody))
			rec := serve(t, h, r)

			// The request ID header is set when the request arrives.
			header := rec.Header().Clone()
			header.Del(RequestIDHeader)
			if rec.Body.Len() != 0 || len(header) != 0 {
				t.Errorf("wrote response %d %v %s, want nothing", rec.Code, rec.Header(), rec.Body)
			}
			if !strings.Contains(logs.String(), "status=499 code=client_closed") {
				t.Errorf("log does not record the cancellation:\n%s", logs)
			}
		})
	}
}

func TestChatCompletionsBodyReadFailure(t *testing.T) {
	p := &recordingProvider{}
	h, _ := newHandler(t, p)

	r := httptest.NewRequest(http.MethodPost, ChatCompletionsPath, io.MultiReader(strings.NewReader(`{"model"`), errReader{}))
	rec := serve(t, h, r)

	assertError(t, rec, http.StatusBadRequest, typeInvalidRequest, codeInvalidRequest)
	if p.calls != 0 {
		t.Errorf("provider called %d times, want 0", p.calls)
	}
}

type errReader struct{}

func (errReader) Read([]byte) (int, error) { return 0, errors.New("connection reset") }
