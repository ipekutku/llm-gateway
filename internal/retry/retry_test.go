package retry

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ipekutku/llm-gateway/internal/llm"
)

// guard bounds how long a test waits for something that should happen
// promptly. It is a failure guard, not a synchronization mechanism.
const guard = 5 * time.Second

var (
	testRequest = llm.ChatRequest{
		Model:     "model-a",
		Messages:  []llm.Message{{Role: llm.RoleUser, Content: "hi"}},
		MaxTokens: 10,
	}
	okResponse = llm.ChatResponse{
		Model:        "model-a",
		Message:      llm.Message{Role: llm.RoleAssistant, Content: "ok"},
		FinishReason: llm.FinishReasonStop,
	}
	testPolicy = Policy{MaxAttempts: 3, BaseDelay: 100 * time.Millisecond, MaxDelay: time.Second}
)

func statusErr(status int) error {
	return &llm.ProviderError{Provider: "openai", StatusCode: status, Err: errors.New("unexpected status")}
}

// dialErr is a transport failure before the request was sent, as the
// adapters report it.
func dialErr() error {
	return &llm.ProviderError{Provider: "openai", Err: fmt.Errorf("send request: %w",
		&net.OpError{Op: "dial", Net: "tcp", Err: errors.New("connection refused")})}
}

// scripted is a provider that returns the scripted results in order and
// records each call.
type scripted struct {
	mu      sync.Mutex
	results []error // nil means success
	calls   int
}

func (s *scripted) Chat(ctx context.Context, req llm.ChatRequest) (llm.ChatResponse, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	err := s.results[min(s.calls, len(s.results)-1)]
	s.calls++
	if err != nil {
		return llm.ChatResponse{}, err
	}
	return okResponse, nil
}

func (s *scripted) Calls() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.calls
}

// newTestProvider returns a Provider around next whose jitter returns the
// full backoff and whose sleep records the delays without waiting.
func newTestProvider(t *testing.T, next llm.Provider, policy Policy) (*Provider, *[]time.Duration) {
	t.Helper()
	p, err := New(next, policy)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	var delays []time.Duration
	p.jitter = func(d time.Duration) time.Duration { return d }
	p.sleep = func(_ context.Context, d time.Duration) error {
		delays = append(delays, d)
		return nil
	}
	return p, &delays
}

func TestNew(t *testing.T) {
	next := &scripted{results: []error{nil}}
	tests := []struct {
		name   string
		next   llm.Provider
		policy Policy
		ok     bool
	}{
		{"valid", next, testPolicy, true},
		{"single attempt", next, Policy{MaxAttempts: 1, BaseDelay: time.Second, MaxDelay: time.Second}, true},
		{"nil provider", nil, testPolicy, false},
		{"zero attempts", next, Policy{MaxAttempts: 0, BaseDelay: time.Second, MaxDelay: time.Second}, false},
		{"zero base delay", next, Policy{MaxAttempts: 3, BaseDelay: 0, MaxDelay: time.Second}, false},
		{"max below base", next, Policy{MaxAttempts: 3, BaseDelay: time.Second, MaxDelay: time.Millisecond}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := New(tt.next, tt.policy)
			if (err == nil) != tt.ok {
				t.Errorf("New() error = %v, want ok %v", err, tt.ok)
			}
		})
	}
}

func TestChatSucceedsOnFirstAttempt(t *testing.T) {
	next := &scripted{results: []error{nil}}
	p, delays := newTestProvider(t, next, testPolicy)

	got, err := p.Chat(context.Background(), testRequest)

	if err != nil || got != okResponse {
		t.Fatalf("Chat() = %+v, %v; want success", got, err)
	}
	if next.Calls() != 1 || len(*delays) != 0 {
		t.Errorf("calls = %d, waits = %v; want 1 call and no waits", next.Calls(), *delays)
	}
}

func TestChatSucceedsOnRetry(t *testing.T) {
	next := &scripted{results: []error{statusErr(http.StatusServiceUnavailable), dialErr(), nil}}
	p, delays := newTestProvider(t, next, testPolicy)

	got, err := p.Chat(context.Background(), testRequest)

	if err != nil || got != okResponse {
		t.Fatalf("Chat() = %+v, %v; want success", got, err)
	}
	if next.Calls() != 3 || len(*delays) != 2 {
		t.Errorf("calls = %d, waits = %v; want 3 calls and 2 waits", next.Calls(), *delays)
	}
}

func TestChatExhaustsAttempts(t *testing.T) {
	next := &scripted{results: []error{statusErr(http.StatusTooManyRequests)}}
	p, delays := newTestProvider(t, next, testPolicy)

	_, err := p.Chat(context.Background(), testRequest)

	if next.Calls() != 3 || len(*delays) != 2 {
		t.Errorf("calls = %d, waits = %v; want 3 calls and 2 waits", next.Calls(), *delays)
	}
	pe, ok := errors.AsType[*llm.ProviderError](err)
	if !ok || pe.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("error = %v, want the last *llm.ProviderError with status 429", err)
	}
	if !strings.Contains(err.Error(), "after 3 attempts") {
		t.Errorf("error = %q, want the attempt count", err)
	}
}

func TestChatBacksOffExponentiallyUpToMaxDelay(t *testing.T) {
	next := &scripted{results: []error{statusErr(http.StatusBadGateway)}}
	p, delays := newTestProvider(t, next, Policy{MaxAttempts: 6, BaseDelay: 100 * time.Millisecond, MaxDelay: 500 * time.Millisecond})

	_, _ = p.Chat(context.Background(), testRequest)

	want := []time.Duration{100 * time.Millisecond, 200 * time.Millisecond, 400 * time.Millisecond, 500 * time.Millisecond, 500 * time.Millisecond}
	if fmt.Sprint(*delays) != fmt.Sprint(want) {
		t.Errorf("waits = %v, want %v", *delays, want)
	}
}

func TestChatAppliesJitterToBackoff(t *testing.T) {
	next := &scripted{results: []error{statusErr(http.StatusBadGateway), nil}}
	p, delays := newTestProvider(t, next, testPolicy)
	var ceilings []time.Duration
	p.jitter = func(d time.Duration) time.Duration {
		ceilings = append(ceilings, d)
		return d / 4
	}

	_, _ = p.Chat(context.Background(), testRequest)

	if fmt.Sprint(ceilings) != fmt.Sprint([]time.Duration{testPolicy.BaseDelay}) {
		t.Errorf("jitter ceilings = %v, want [%v]", ceilings, testPolicy.BaseDelay)
	}
	if fmt.Sprint(*delays) != fmt.Sprint([]time.Duration{testPolicy.BaseDelay / 4}) {
		t.Errorf("waits = %v, want the jittered delay", *delays)
	}
}

func TestChatHonorsRetryAfter(t *testing.T) {
	limited := &llm.ProviderError{Provider: "anthropic", StatusCode: http.StatusTooManyRequests, RetryAfter: 3 * time.Second}
	next := &scripted{results: []error{limited, nil}}
	p, delays := newTestProvider(t, next, testPolicy)
	p.jitter = func(time.Duration) time.Duration { return 0 }

	if _, err := p.Chat(context.Background(), testRequest); err != nil {
		t.Fatalf("Chat() error = %v", err)
	}
	// Retry-After wins over both the jittered backoff and MaxDelay.
	if fmt.Sprint(*delays) != fmt.Sprint([]time.Duration{3 * time.Second}) {
		t.Errorf("waits = %v, want [3s]", *delays)
	}
}

func TestChatDoesNotRetryNonRetryableFailures(t *testing.T) {
	tests := []struct {
		name string
		err  error
	}{
		{"upstream 400", statusErr(http.StatusBadRequest)},
		{"upstream 401", statusErr(http.StatusUnauthorized)},
		{"upstream 500", statusErr(http.StatusInternalServerError)},
		{"unusable response", &llm.ProviderError{Provider: "openai", StatusCode: http.StatusOK, Err: errors.New("malformed response JSON")}},
		{"connection reset after sending", &llm.ProviderError{Provider: "openai", Err: fmt.Errorf("send request: %w",
			&net.OpError{Op: "read", Net: "tcp", Err: errors.New("connection reset by peer")})}},
		{"upstream deadline", &llm.ProviderError{Provider: "openai", Err: context.DeadlineExceeded}},
		{"cancellation", &llm.ProviderError{Provider: "openai", StatusCode: http.StatusServiceUnavailable, Err: context.Canceled}},
		{"unknown model", fmt.Errorf("%w: %q", llm.ErrUnknownModel, "m")},
		{"gateway error", errors.New("openai: invalid request: no messages")},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			next := &scripted{results: []error{tt.err, nil}}
			p, delays := newTestProvider(t, next, testPolicy)

			_, err := p.Chat(context.Background(), testRequest)

			if err != tt.err {
				t.Errorf("error = %v, want the original error unchanged", err)
			}
			if next.Calls() != 1 || len(*delays) != 0 {
				t.Errorf("calls = %d, waits = %v; want 1 call and no waits", next.Calls(), *delays)
			}
		})
	}
}

func TestRetryable(t *testing.T) {
	for _, status := range []int{429, 502, 503, 504, 529} {
		if !Retryable(statusErr(status)) {
			t.Errorf("Retryable(status %d) = false, want true", status)
		}
		if !Retryable(fmt.Errorf("routed: %w", statusErr(status))) {
			t.Errorf("Retryable(wrapped status %d) = false, want true", status)
		}
	}
	if !Retryable(dialErr()) {
		t.Error("Retryable(dial failure) = false, want true")
	}
	// A bare dial error did not come from an adapter.
	if Retryable(&net.OpError{Op: "dial", Err: errors.New("refused")}) {
		t.Error("Retryable(bare dial error) = true, want false")
	}
}

func TestChatStopsWhenWaitWouldExceedDeadline(t *testing.T) {
	limited := &llm.ProviderError{Provider: "openai", StatusCode: http.StatusTooManyRequests, RetryAfter: time.Hour}
	next := &scripted{results: []error{limited, nil}}
	p, delays := newTestProvider(t, next, testPolicy)
	ctx, cancel := context.WithTimeout(context.Background(), guard)
	defer cancel()

	_, err := p.Chat(ctx, testRequest)

	if err != limited {
		t.Errorf("error = %v, want the 429 unchanged", err)
	}
	if next.Calls() != 1 || len(*delays) != 0 {
		t.Errorf("calls = %d, waits = %v; want 1 call and no waits", next.Calls(), *delays)
	}
}

func TestChatStopsWhenAttemptExhaustsDeadline(t *testing.T) {
	// A slow upstream uses up the whole budget; its deadline failure is
	// returned without another attempt.
	next := &blocking{}
	p, delays := newTestProvider(t, next, testPolicy)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()

	_, err := p.Chat(ctx, testRequest)

	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("error = %v, want context.DeadlineExceeded", err)
	}
	if next.Calls() != 1 || len(*delays) != 0 {
		t.Errorf("calls = %d, waits = %v; want 1 call and no waits", next.Calls(), *delays)
	}
}

// blocking is a provider whose calls block until ctx is done, then fail as
// the adapters do.
type blocking struct {
	mu    sync.Mutex
	calls int
}

func (b *blocking) Chat(ctx context.Context, _ llm.ChatRequest) (llm.ChatResponse, error) {
	b.mu.Lock()
	b.calls++
	b.mu.Unlock()
	<-ctx.Done()
	return llm.ChatResponse{}, &llm.ProviderError{Provider: "openai", Err: ctx.Err()}
}

func (b *blocking) Calls() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.calls
}

func TestChatCancellationDuringWaitStopsRetries(t *testing.T) {
	next := &scripted{results: []error{statusErr(http.StatusServiceUnavailable), nil}}
	// The real sleep, with a backoff far longer than the test.
	p, err := New(next, Policy{MaxAttempts: 3, BaseDelay: time.Hour, MaxDelay: time.Hour})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	p.jitter = func(d time.Duration) time.Duration { return d }
	waiting := make(chan struct{})
	p.sleep = func(ctx context.Context, d time.Duration) error {
		close(waiting)
		return sleep(ctx, d)
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := p.Chat(ctx, testRequest)
		done <- err
	}()

	select {
	case <-waiting:
	case <-time.After(guard):
		t.Fatal("timed out waiting for the retry wait to start")
	}
	cancel()

	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Errorf("error = %v, want context.Canceled", err)
		}
		if pe, ok := errors.AsType[*llm.ProviderError](err); !ok || pe.StatusCode != http.StatusServiceUnavailable {
			t.Errorf("error = %v, want it to wrap the last upstream failure", err)
		}
	case <-time.After(guard):
		t.Fatal("Chat did not return promptly after cancellation")
	}
	if next.Calls() != 1 {
		t.Errorf("calls = %d, want 1: no attempt after cancellation", next.Calls())
	}
}

func TestChatDoesNotRetryAfterCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	// The client goes away while the upstream reports a retryable failure.
	next := providerFunc(func(context.Context, llm.ChatRequest) (llm.ChatResponse, error) {
		cancel()
		return llm.ChatResponse{}, statusErr(http.StatusServiceUnavailable)
	})
	calls := 0
	counted := providerFunc(func(ctx context.Context, req llm.ChatRequest) (llm.ChatResponse, error) {
		calls++
		return next(ctx, req)
	})
	p, delays := newTestProvider(t, counted, testPolicy)

	_, _ = p.Chat(ctx, testRequest)

	if calls != 1 || len(*delays) != 0 {
		t.Errorf("calls = %d, waits = %v; want 1 call and no waits", calls, *delays)
	}
}

type providerFunc func(ctx context.Context, req llm.ChatRequest) (llm.ChatResponse, error)

func (f providerFunc) Chat(ctx context.Context, req llm.ChatRequest) (llm.ChatResponse, error) {
	return f(ctx, req)
}

func TestFullJitterStaysWithinBounds(t *testing.T) {
	if got := fullJitter(0); got != 0 {
		t.Errorf("fullJitter(0) = %v, want 0", got)
	}
	const d = 10 * time.Millisecond
	for range 1000 {
		if got := fullJitter(d); got < 0 || got > d {
			t.Fatalf("fullJitter(%v) = %v, want within [0, %v]", d, got, d)
		}
	}
}

func TestSleep(t *testing.T) {
	if err := sleep(context.Background(), time.Millisecond); err != nil {
		t.Errorf("sleep() error = %v, want nil", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := sleep(ctx, time.Hour); !errors.Is(err, context.Canceled) {
		t.Errorf("sleep(canceled) error = %v, want context.Canceled", err)
	}
}
