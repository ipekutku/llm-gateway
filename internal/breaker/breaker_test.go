package breaker

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
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
	testSettings = Settings{Failures: 3, Cooldown: 30 * time.Second}
)

func statusErr(status int) error {
	return &llm.ProviderError{Provider: "openai", StatusCode: status, Err: errors.New("unexpected status")}
}

var errUnavailable = statusErr(http.StatusServiceUnavailable)

// stub is a provider that returns err for every call and counts calls.
type stub struct {
	mu    sync.Mutex
	err   error
	calls int
}

func (s *stub) Chat(context.Context, llm.ChatRequest) (llm.ChatResponse, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls++
	if s.err != nil {
		return llm.ChatResponse{}, s.err
	}
	return okResponse, nil
}

func (s *stub) set(err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.err = err
}

func (s *stub) Calls() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.calls
}

// clock is a manually advanced time source.
type clock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *clock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

// newTestBreaker returns a Breaker around next with a manual clock and a
// log buffer.
func newTestBreaker(t *testing.T, next llm.Provider, settings Settings) (*Breaker, *clock, *syncBuffer) {
	t.Helper()
	logs := &syncBuffer{}
	b, err := New("openai", next, settings, slog.New(slog.NewTextHandler(logs, nil)))
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	c := &clock{now: time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC)}
	b.now = c.Now
	return b, c, logs
}

func call(b *Breaker) error {
	_, err := b.Chat(context.Background(), testRequest)
	return err
}

// trip fails b's provider until the breaker opens.
func trip(t *testing.T, b *Breaker, next *stub) {
	t.Helper()
	next.set(errUnavailable)
	for range b.settings.Failures {
		if err := call(b); errors.Is(err, llm.ErrCircuitOpen) {
			t.Fatal("breaker opened before reaching the failure threshold")
		}
	}
	if err := call(b); !errors.Is(err, llm.ErrCircuitOpen) {
		t.Fatalf("after %d failures: error = %v, want llm.ErrCircuitOpen", b.settings.Failures, err)
	}
}

func TestNew(t *testing.T) {
	next := &stub{}
	tests := []struct {
		name     string
		provider string
		next     llm.Provider
		settings Settings
		ok       bool
	}{
		{"valid", "openai", next, testSettings, true},
		{"one failure opens", "openai", next, Settings{Failures: 1, Cooldown: time.Second}, true},
		{"blank name", " ", next, testSettings, false},
		{"nil provider", "openai", nil, testSettings, false},
		{"zero failures", "openai", next, Settings{Failures: 0, Cooldown: time.Second}, false},
		{"zero cooldown", "openai", next, Settings{Failures: 3, Cooldown: 0}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := New(tt.provider, tt.next, tt.settings, nil)
			if (err == nil) != tt.ok {
				t.Errorf("New() error = %v, want ok %v", err, tt.ok)
			}
		})
	}
}

func TestClosedBreakerPassesCallsThrough(t *testing.T) {
	next := &stub{}
	b, _, _ := newTestBreaker(t, next, testSettings)

	got, err := b.Chat(context.Background(), testRequest)
	if err != nil || got != okResponse {
		t.Fatalf("Chat() = %+v, %v; want success", got, err)
	}

	// Failures below the threshold are returned unchanged.
	next.set(errUnavailable)
	for range testSettings.Failures - 1 {
		if err := call(b); err != errUnavailable {
			t.Errorf("error = %v, want the provider error unchanged", err)
		}
	}
	if next.Calls() != testSettings.Failures {
		t.Errorf("calls = %d, want %d", next.Calls(), testSettings.Failures)
	}
}

func TestBreakerOpensAfterConsecutiveFailures(t *testing.T) {
	next := &stub{}
	b, _, logs := newTestBreaker(t, next, testSettings)

	trip(t, b, next)

	// The rejected call did not reach the provider.
	if next.Calls() != testSettings.Failures {
		t.Errorf("calls = %d, want %d", next.Calls(), testSettings.Failures)
	}
	err := call(b)
	if !strings.Contains(err.Error(), "openai") {
		t.Errorf("error = %q, want it to name the provider", err)
	}
	if _, ok := errors.AsType[*llm.ProviderError](err); ok {
		t.Errorf("error = %v, want no *llm.ProviderError: the provider was not called", err)
	}
	for _, want := range []string{`msg="circuit opened"`, "provider=openai", "from=closed", "consecutive_failures=3", "cooldown=30s"} {
		if !strings.Contains(logs.String(), want) {
			t.Errorf("log does not contain %q:\n%s", want, logs)
		}
	}
}

func TestSuccessResetsFailureCount(t *testing.T) {
	next := &stub{}
	b, _, _ := newTestBreaker(t, next, testSettings)

	for range 3 {
		next.set(errUnavailable)
		_ = call(b)
		_ = call(b)
		next.set(nil)
		if err := call(b); err != nil {
			t.Fatalf("error = %v, want success: failures were not consecutive", err)
		}
	}
}

func TestBreakerStaysOpenDuringCooldown(t *testing.T) {
	next := &stub{}
	b, clk, _ := newTestBreaker(t, next, testSettings)
	trip(t, b, next)
	next.set(nil)
	calls := next.Calls()

	clk.Advance(testSettings.Cooldown - time.Nanosecond)

	if err := call(b); !errors.Is(err, llm.ErrCircuitOpen) {
		t.Errorf("error = %v, want llm.ErrCircuitOpen before the cooldown ends", err)
	}
	if next.Calls() != calls {
		t.Errorf("provider called %d times while open, want 0", next.Calls()-calls)
	}
}

func TestHalfOpenProbeSuccessClosesBreaker(t *testing.T) {
	next := &stub{}
	b, clk, logs := newTestBreaker(t, next, testSettings)
	trip(t, b, next)
	next.set(nil)

	clk.Advance(testSettings.Cooldown)

	if err := call(b); err != nil {
		t.Fatalf("probe error = %v, want success", err)
	}
	// Closed again: calls pass, and it takes a full run of failures to
	// reopen.
	next.set(errUnavailable)
	for range testSettings.Failures {
		if err := call(b); errors.Is(err, llm.ErrCircuitOpen) {
			t.Fatal("breaker rejected a call after closing")
		}
	}
	for _, want := range []string{`msg="circuit half-open, probing provider"`, `msg="circuit closed"`} {
		if !strings.Contains(logs.String(), want) {
			t.Errorf("log does not contain %q:\n%s", want, logs)
		}
	}
}

func TestHalfOpenProbeFailureReopensBreaker(t *testing.T) {
	next := &stub{}
	b, clk, logs := newTestBreaker(t, next, testSettings)
	trip(t, b, next)

	clk.Advance(testSettings.Cooldown)
	if err := call(b); err != errUnavailable {
		t.Fatalf("probe error = %v, want the provider error", err)
	}

	// Open for a whole new cooldown.
	next.set(nil)
	clk.Advance(testSettings.Cooldown - time.Nanosecond)
	if err := call(b); !errors.Is(err, llm.ErrCircuitOpen) {
		t.Errorf("error = %v, want llm.ErrCircuitOpen after a failed probe", err)
	}
	clk.Advance(time.Nanosecond)
	if err := call(b); err != nil {
		t.Errorf("error = %v, want a successful probe after the new cooldown", err)
	}
	if !strings.Contains(logs.String(), "from=half-open") {
		t.Errorf("log does not record reopening from half-open:\n%s", logs)
	}
}

// gate is a provider whose calls block until released, so a probe can be
// held in flight.
type gate struct {
	entered chan struct{}
	release chan error
}

func (g *gate) Chat(ctx context.Context, _ llm.ChatRequest) (llm.ChatResponse, error) {
	g.entered <- struct{}{}
	select {
	case err := <-g.release:
		if err != nil {
			return llm.ChatResponse{}, err
		}
		return okResponse, nil
	case <-ctx.Done():
		return llm.ChatResponse{}, &llm.ProviderError{Provider: "openai", Err: ctx.Err()}
	}
}

func TestHalfOpenAdmitsOneProbeAtATime(t *testing.T) {
	g := &gate{entered: make(chan struct{}, 8), release: make(chan error, 1)}
	b, clk, _ := newTestBreaker(t, g, Settings{Failures: 1, Cooldown: time.Second})

	// Open the breaker with one failure.
	g.release <- errUnavailable
	_ = call(b)
	<-g.entered
	clk.Advance(time.Second)

	probe := make(chan error, 1)
	go func() { probe <- call(b) }()
	select {
	case <-g.entered:
	case <-time.After(guard):
		t.Fatal("timed out waiting for the probe")
	}

	// While the probe is in flight, other calls are rejected. Their
	// context is already canceled, so a call wrongly let through returns
	// at once instead of blocking in the gate.
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	for range 3 {
		if _, err := b.Chat(canceled, testRequest); !errors.Is(err, llm.ErrCircuitOpen) {
			t.Errorf("error = %v, want llm.ErrCircuitOpen while probing", err)
		}
	}

	g.release <- nil
	select {
	case err := <-probe:
		if err != nil {
			t.Fatalf("probe error = %v, want success", err)
		}
	case <-time.After(guard):
		t.Fatal("timed out waiting for the probe to finish")
	}
	g.release <- nil
	if err := call(b); err != nil {
		t.Errorf("error = %v, want success after the probe closed the breaker", err)
	}
}

func TestCanceledProbeLetsTheNextCallProbe(t *testing.T) {
	g := &gate{entered: make(chan struct{}, 1), release: make(chan error, 1)}
	b, clk, _ := newTestBreaker(t, g, Settings{Failures: 1, Cooldown: time.Second})
	g.release <- errUnavailable
	_ = call(b)
	<-g.entered
	clk.Advance(time.Second)

	// The client of the probe goes away.
	ctx, cancel := context.WithCancel(context.Background())
	probe := make(chan error, 1)
	go func() {
		_, err := b.Chat(ctx, testRequest)
		probe <- err
	}()
	select {
	case <-g.entered:
	case <-time.After(guard):
		t.Fatal("timed out waiting for the probe")
	}
	cancel()
	select {
	case err := <-probe:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("probe error = %v, want context.Canceled", err)
		}
	case <-time.After(guard):
		t.Fatal("timed out waiting for the canceled probe")
	}

	// The breaker learned nothing, so the next call probes.
	g.release <- nil
	if err := call(b); err != nil {
		t.Errorf("error = %v, want a new probe to succeed", err)
	}
}

func TestStaleResultDuringProbeIsIgnored(t *testing.T) {
	// A slow call admitted while the breaker was closed finishes
	// successfully while a half-open probe is in flight. The probe alone
	// decides the outcome, so the breaker must not close early.
	slow := &gate{entered: make(chan struct{}, 1), release: make(chan error, 1)}
	probe := &gate{entered: make(chan struct{}, 1), release: make(chan error, 1)}
	var calls atomic.Int64
	next := providerFunc(func(ctx context.Context, req llm.ChatRequest) (llm.ChatResponse, error) {
		switch calls.Add(1) {
		case 1:
			return slow.Chat(ctx, req)
		case 2, 3:
			return llm.ChatResponse{}, errUnavailable
		case 4:
			return probe.Chat(ctx, req)
		default:
			return okResponse, nil
		}
	})
	b, clk, _ := newTestBreaker(t, next, Settings{Failures: 2, Cooldown: time.Second})

	slowDone := make(chan error, 1)
	go func() { slowDone <- call(b) }()
	waitFor(t, slow.entered, "the slow call")
	_ = call(b)
	_ = call(b) // The breaker opens.
	clk.Advance(time.Second)

	probeDone := make(chan error, 1)
	go func() { probeDone <- call(b) }()
	waitFor(t, probe.entered, "the probe")

	slow.release <- nil
	if err := <-slowDone; err != nil {
		t.Fatalf("slow call error = %v, want success", err)
	}
	if err := call(b); !errors.Is(err, llm.ErrCircuitOpen) {
		t.Errorf("error = %v, want llm.ErrCircuitOpen: a stale success must not close the breaker", err)
	}

	probe.release <- nil
	if err := <-probeDone; err != nil {
		t.Fatalf("probe error = %v, want success", err)
	}
	if err := call(b); err != nil {
		t.Errorf("error = %v, want success after the probe closed the breaker", err)
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

type providerFunc func(ctx context.Context, req llm.ChatRequest) (llm.ChatResponse, error)

func (f providerFunc) Chat(ctx context.Context, req llm.ChatRequest) (llm.ChatResponse, error) {
	return f(ctx, req)
}

func TestClassify(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want outcome
	}{
		{"success", nil, success},
		{"429", statusErr(429), failure},
		{"500", statusErr(500), failure},
		{"502", statusErr(502), failure},
		{"503", statusErr(503), failure},
		{"529", statusErr(529), failure},
		{"transport failure", &llm.ProviderError{Provider: "openai", Err: errors.New("connection refused")}, failure},
		{"unusable response", &llm.ProviderError{Provider: "openai", StatusCode: 200, Err: errors.New("malformed response JSON")}, failure},
		{"provider timeout", &llm.ProviderError{Provider: "openai", Err: context.DeadlineExceeded}, failure},
		{"retries exhausted", fmt.Errorf("after 3 attempts: %w", statusErr(503)), failure},
		{"400", statusErr(400), success},
		{"401", statusErr(401), success},
		{"404", statusErr(404), success},
		{"client cancellation", &llm.ProviderError{Provider: "openai", StatusCode: 503, Err: context.Canceled}, ignored},
		{"gateway error", errors.New("openai: invalid request: no messages"), ignored},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := classify(tt.err); got != tt.want {
				t.Errorf("classify() = %d, want %d", got, tt.want)
			}
		})
	}
}

func TestIgnoredResultsDoNotResetFailures(t *testing.T) {
	next := &stub{}
	b, _, _ := newTestBreaker(t, next, Settings{Failures: 2, Cooldown: time.Hour})

	next.set(errUnavailable)
	_ = call(b)
	next.set(&llm.ProviderError{Provider: "openai", Err: context.Canceled})
	_ = call(b)
	next.set(errUnavailable)
	_ = call(b)

	if err := call(b); !errors.Is(err, llm.ErrCircuitOpen) {
		t.Errorf("error = %v, want llm.ErrCircuitOpen: a cancellation must not reset the count", err)
	}
}

func TestClientErrorsDoNotOpenBreaker(t *testing.T) {
	next := &stub{err: statusErr(http.StatusBadRequest)}
	b, _, _ := newTestBreaker(t, next, Settings{Failures: 1, Cooldown: time.Hour})

	for range 5 {
		if err := call(b); errors.Is(err, llm.ErrCircuitOpen) {
			t.Fatal("breaker opened on client errors")
		}
	}
}

func TestConcurrentCalls(t *testing.T) {
	// Mixed outcomes from many goroutines exercise the locking under the
	// race detector; the breaker must stay consistent and never panic.
	next := &stub{}
	b, clk, _ := newTestBreaker(t, next, Settings{Failures: 3, Cooldown: time.Millisecond})
	var wg sync.WaitGroup
	for i := range 50 {
		wg.Go(func() {
			for j := range 20 {
				if (i+j)%3 == 0 {
					next.set(errUnavailable)
				} else {
					next.set(nil)
				}
				_ = call(b)
				clk.Advance(time.Millisecond)
			}
		})
	}
	wg.Wait()

	b.mu.Lock()
	defer b.mu.Unlock()
	if b.probing {
		t.Error("a probe is still marked in flight after all calls returned")
	}
}

// syncBuffer is a strings.Builder safe for concurrent writes and reads.
type syncBuffer struct {
	mu  sync.Mutex
	buf strings.Builder
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
