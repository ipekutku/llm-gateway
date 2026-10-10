package httpapi

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ipekutku/llm-gateway/internal/auth"
	"github.com/ipekutku/llm-gateway/internal/llm"
	"github.com/ipekutku/llm-gateway/internal/ratelimit"
)

func requestWithAuth(values ...string) *http.Request {
	r := httptest.NewRequest(http.MethodPost, ChatCompletionsPath, strings.NewReader(validBody))
	for _, v := range values {
		r.Header.Add("Authorization", v)
	}
	return r
}

func TestAuthenticationAcceptsValidKeys(t *testing.T) {
	for name, tc := range map[string]struct {
		header string
		client string
	}{
		"team-a":                {"Bearer " + keyA, "team-a"},
		"team-b":                {"Bearer " + keyB, "team-b"},
		"lowercase scheme":      {"bearer " + keyA, "team-a"},
		"several spaces before": {"Bearer   " + keyA, "team-a"},
	} {
		t.Run(name, func(t *testing.T) {
			var got auth.Identity
			var ok bool
			h, _ := newHandler(t, providerFunc(func(ctx context.Context, req llm.ChatRequest) (llm.ChatResponse, error) {
				got, ok = auth.FromContext(ctx)
				return okProvider(ctx, req)
			}))

			rec := serveRaw(h, requestWithAuth(tc.header))

			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200 (body %s)", rec.Code, rec.Body)
			}
			if !ok || got.ClientID != tc.client {
				t.Errorf("provider saw identity %+v (present %v), want client %s", got, ok, tc.client)
			}
		})
	}
}

func TestAuthenticationRejectsMissingOrMalformedHeader(t *testing.T) {
	for name, headers := range map[string][]string{
		"no header":          nil,
		"empty header":       {""},
		"scheme only":        {"Bearer"},
		"scheme and space":   {"Bearer "},
		"basic auth":         {"Basic dGVhbS1hOnNlY3JldA=="},
		"key without scheme": {keyA},
		"tab separator":      {"Bearer\t" + keyA},
		"trailing data":      {"Bearer " + keyA + " extra"},
		"two headers":        {"Bearer " + keyA, "Bearer " + keyB},
	} {
		t.Run(name, func(t *testing.T) {
			p := &recordingProvider{}
			h, logs := newHandler(t, p)

			rec := serveRaw(h, requestWithAuth(headers...))

			assertError(t, rec, http.StatusUnauthorized, typeAuthentication, codeMissingAPIKey)
			if got := rec.Header().Get("WWW-Authenticate"); got != "Bearer" {
				t.Errorf("WWW-Authenticate = %q, want Bearer", got)
			}
			if p.calls != 0 {
				t.Errorf("provider called %d times, want 0", p.calls)
			}
			if strings.Contains(logs.String(), keyA) {
				t.Errorf("log exposes the key:\n%s", logs)
			}
		})
	}
}

func TestAuthenticationRejectsInvalidAndDisabledKeysAlike(t *testing.T) {
	var bodies []string
	for name, key := range map[string]string{
		"unknown":  "not-a-gateway-key",
		"disabled": keyDisabled,
	} {
		t.Run(name, func(t *testing.T) {
			p := &recordingProvider{}
			h, logs := newHandler(t, p)

			rec := serveRaw(h, requestWithAuth("Bearer "+key))

			assertError(t, rec, http.StatusUnauthorized, typeAuthentication, codeInvalidAPIKey)
			if got := rec.Header().Get("WWW-Authenticate"); got != `Bearer error="invalid_token"` {
				t.Errorf("WWW-Authenticate = %q, want Bearer error=\"invalid_token\"", got)
			}
			if p.calls != 0 {
				t.Errorf("provider called %d times, want 0", p.calls)
			}
			bodies = append(bodies, rec.Body.String())
			if strings.Contains(logs.String(), key) || strings.Contains(rec.Body.String(), key) {
				t.Errorf("key exposed in the log or response:\n%s\n%s", logs, rec.Body)
			}
			// The log tells the operator why; the response does not.
			if name == "disabled" && !strings.Contains(logs.String(), "API key disabled") {
				t.Errorf("log does not say the key is disabled:\n%s", logs)
			}
		})
	}
	if len(bodies) == 2 && bodies[0] != bodies[1] {
		t.Errorf("invalid and disabled keys get different responses:\n%s\n%s", bodies[0], bodies[1])
	}
}

func TestAuthenticationHappensBeforeBodyIsRead(t *testing.T) {
	h, _ := newHandler(t, okProvider)
	// An oversized body is not read, so the response is about the key.
	r := httptest.NewRequest(http.MethodPost, ChatCompletionsPath, strings.NewReader(strings.Repeat("x", maxRequestBytes+1)))
	rec := serveRaw(h, r)
	assertError(t, rec, http.StatusUnauthorized, typeAuthentication, codeMissingAPIKey)
}

func TestFailureLogsIncludeClientID(t *testing.T) {
	h, logs := newHandler(t, failingProvider(&llm.ProviderError{Provider: "openai", StatusCode: http.StatusServiceUnavailable}))
	serveRaw(h, requestWithAuth("Bearer "+keyB))
	if !strings.Contains(logs.String(), "client_id=team-b") {
		t.Errorf("failure log does not name the client:\n%s", logs)
	}
}

func TestRateLimitExceeded(t *testing.T) {
	p := &recordingProvider{}
	limiter := testLimiter(t, map[string]ratelimit.Limits{
		"team-a":   {RequestsPerMinute: 1, Burst: 2, MaxConcurrent: 10},
		"team-b":   generous,
		"team-old": generous,
	})
	h, logs := newHandlerWith(t, p, limiter, testTimeout)

	for i := range 2 {
		if rec := serve(t, h, requestWithAuth("Bearer "+keyA)); rec.Code != http.StatusOK {
			t.Fatalf("request %d: status = %d, want 200", i+1, rec.Code)
		}
	}
	rec := serve(t, h, requestWithAuth("Bearer "+keyA))

	assertError(t, rec, http.StatusTooManyRequests, typeRateLimit, codeRateLimitExceeded)
	// The next request is earned in a minute, less the few moments the
	// test took; Retry-After rounds that up to whole seconds.
	if got := rec.Header().Get("Retry-After"); got != "60" {
		t.Errorf("Retry-After = %q, want 60", got)
	}
	if p.calls != 2 {
		t.Errorf("provider called %d times, want 2", p.calls)
	}
	for _, want := range []string{"client_id=team-a", "code=rate_limit_exceeded", "request_rate"} {
		if !strings.Contains(logs.String(), want) {
			t.Errorf("log does not contain %q:\n%s", want, logs)
		}
	}

	// Another client's limit is independent.
	if rec := serve(t, h, requestWithAuth("Bearer "+keyB)); rec.Code != http.StatusOK {
		t.Errorf("team-b status = %d, want 200", rec.Code)
	}
}

func TestRetryAfterSeconds(t *testing.T) {
	for d, want := range map[time.Duration]string{
		time.Nanosecond:                       "1",
		500 * time.Millisecond:                "1",
		time.Second:                           "1",
		time.Second + time.Nanosecond:         "2",
		59*time.Second + 999*time.Millisecond: "60",
	} {
		if got := retryAfterSeconds(d); got != want {
			t.Errorf("retryAfterSeconds(%v) = %s, want %s", d, got, want)
		}
	}
}

func TestConcurrencyLimitExceeded(t *testing.T) {
	// Buffered, so calls after unblock is closed never block on it.
	entered := make(chan struct{}, 10)
	unblock := make(chan struct{})
	p := providerFunc(func(ctx context.Context, req llm.ChatRequest) (llm.ChatResponse, error) {
		entered <- struct{}{}
		<-unblock
		return okProvider(ctx, req)
	})
	limiter := testLimiter(t, map[string]ratelimit.Limits{
		"team-a":   {RequestsPerMinute: ratelimit.MaxLimit, Burst: ratelimit.MaxLimit, MaxConcurrent: 1},
		"team-b":   {RequestsPerMinute: ratelimit.MaxLimit, Burst: ratelimit.MaxLimit, MaxConcurrent: 1},
		"team-old": generous,
	})
	h, _ := newHandlerWith(t, p, limiter, testTimeout)

	var wg sync.WaitGroup
	defer wg.Wait()
	first := make(chan int, 1)
	wg.Go(func() { first <- serve(t, h, requestWithAuth("Bearer "+keyA)).Code })
	waitOrFail(t, entered, "the first request to reach the provider")

	rec := serve(t, h, requestWithAuth("Bearer "+keyA))
	assertError(t, rec, http.StatusTooManyRequests, typeRateLimit, codeConcurrencyLimitExceeded)
	if got := rec.Header().Get("Retry-After"); got != "" {
		t.Errorf("Retry-After = %q, want none", got)
	}

	// team-b has its own slot.
	second := make(chan int, 1)
	wg.Go(func() { second <- serve(t, h, requestWithAuth("Bearer "+keyB)).Code })
	waitOrFail(t, entered, "team-b's request to reach the provider")

	close(unblock)
	if code := <-first; code != http.StatusOK {
		t.Errorf("first request status = %d, want 200", code)
	}
	if code := <-second; code != http.StatusOK {
		t.Errorf("team-b status = %d, want 200", code)
	}

	// The finished request released its slot.
	if rec := serve(t, h, requestWithAuth("Bearer "+keyA)); rec.Code != http.StatusOK {
		t.Errorf("status after release = %d, want 200", rec.Code)
	}
}

func TestFailedRequestReleasesConcurrencySlot(t *testing.T) {
	limiter := testLimiter(t, map[string]ratelimit.Limits{
		"team-a":   {RequestsPerMinute: ratelimit.MaxLimit, Burst: ratelimit.MaxLimit, MaxConcurrent: 1},
		"team-b":   generous,
		"team-old": generous,
	})
	h, _ := newHandlerWith(t, okProvider, limiter, testTimeout)

	// Invalid bodies and provider failures both free the slot.
	r := httptest.NewRequest(http.MethodPost, ChatCompletionsPath, strings.NewReader(`{`))
	assertError(t, serve(t, h, r), http.StatusBadRequest, typeInvalidRequest, codeInvalidRequest)
	if rec := post(t, h, validBody); rec.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", rec.Code)
	}
}

func TestUnauthenticatedRequestsDoNotUseLimits(t *testing.T) {
	limiter := testLimiter(t, map[string]ratelimit.Limits{
		"team-a":   {RequestsPerMinute: 1, Burst: 1, MaxConcurrent: 1},
		"team-b":   generous,
		"team-old": generous,
	})
	h, _ := newHandlerWith(t, okProvider, limiter, testTimeout)
	for range 3 {
		serveRaw(h, requestWithAuth("Bearer not-a-key"))
	}
	if rec := post(t, h, validBody); rec.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", rec.Code)
	}
}

func TestClientWithoutLimitsIsAnInternalError(t *testing.T) {
	p := &recordingProvider{}
	limiter := testLimiter(t, map[string]ratelimit.Limits{"team-b": generous})
	h, _ := newHandlerWith(t, p, limiter, testTimeout)

	rec := post(t, h, validBody)

	assertError(t, rec, http.StatusInternalServerError, typeServer, codeInternalError)
	if p.calls != 0 {
		t.Errorf("provider called %d times, want 0", p.calls)
	}
}

// limiterFunc adapts a function to Limiter.
type limiterFunc func(ctx context.Context, clientID string) (func(), error)

func (f limiterFunc) Acquire(ctx context.Context, clientID string) (func(), error) {
	return f(ctx, clientID)
}

func TestLimiterReceivesRequestContext(t *testing.T) {
	var (
		gotClient   string
		gotIdentity auth.Identity
		gotID       string
		released    int
	)
	limiter := limiterFunc(func(ctx context.Context, clientID string) (func(), error) {
		gotClient = clientID
		gotIdentity, _ = auth.FromContext(ctx)
		gotID, _ = ctx.Value(requestIDKey{}).(string)
		return func() { released++ }, nil
	})
	h, err := New(okProvider, testAuthenticator(t), limiter, testTimeout, testAccounting(), nil, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}

	rec := serve(t, h, requestWithAuth("Bearer "+keyB))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %s)", rec.Code, rec.Body)
	}
	if gotClient != "team-b" || gotIdentity.ClientID != "team-b" {
		t.Errorf("Acquire got client %q and context identity %+v, want team-b", gotClient, gotIdentity)
	}
	if want := rec.Header().Get(RequestIDHeader); gotID == "" || gotID != want {
		t.Errorf("Acquire context request ID = %q, want %q", gotID, want)
	}
	if released != 1 {
		t.Errorf("release called %d times, want 1", released)
	}
}

func TestLimiterErrorIsAnInternalError(t *testing.T) {
	p := &recordingProvider{}
	limiter := limiterFunc(func(context.Context, string) (func(), error) {
		return nil, errors.New("limiter unavailable")
	})
	h, err := New(p, testAuthenticator(t), limiter, testTimeout, testAccounting(), nil, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}

	assertError(t, post(t, h, validBody), http.StatusInternalServerError, typeServer, codeInternalError)
	if p.calls != 0 {
		t.Errorf("provider called %d times, want 0", p.calls)
	}
}

func TestClientGoneDuringAdmissionIsClientClosed(t *testing.T) {
	p := &recordingProvider{}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	// The client goes away while a limiter that does I/O is deciding.
	limiter := limiterFunc(func(ctx context.Context, _ string) (func(), error) {
		cancel()
		return nil, ctx.Err()
	})
	var logs strings.Builder
	h, err := New(p, testAuthenticator(t), limiter, testTimeout, testAccounting(), nil, slog.New(slog.NewTextHandler(&logs, nil)))
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}

	rec := serve(t, h, requestWithAuth("Bearer "+keyA).WithContext(ctx))

	if rec.Body.Len() != 0 {
		t.Errorf("response written to a client that went away: %s", rec.Body)
	}
	if p.calls != 0 {
		t.Errorf("provider called %d times, want 0", p.calls)
	}
	for _, want := range []string{"level=INFO", "status=499", "code=client_closed"} {
		if !strings.Contains(logs.String(), want) {
			t.Errorf("log does not contain %q:\n%s", want, logs.String())
		}
	}
}

func waitOrFail(t *testing.T, ch <-chan struct{}, what string) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(5 * time.Second):
		t.Fatalf("timed out waiting for %s", what)
	}
}
