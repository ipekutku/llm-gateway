package ratelimit

import (
	"errors"
	"strings"
	"sync"
	"testing"
	"time"
)

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

func newTestLimiter(t *testing.T, limits map[string]Limits) (*Limiter, *clock) {
	t.Helper()
	l, err := New(limits)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	c := &clock{now: time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC)}
	l.now = c.Now
	return l, c
}

// mustAcquire acquires a request for id and releases it immediately, so
// only the request rate is exercised.
func mustAcquire(t *testing.T, l *Limiter, id string) {
	t.Helper()
	release, err := l.Acquire(id)
	if err != nil {
		t.Fatalf("Acquire(%s): %v", id, err)
	}
	release()
}

func wantRejected(t *testing.T, l *Limiter, id string, limit Limit, retryAfter time.Duration) {
	t.Helper()
	release, err := l.Acquire(id)
	if err == nil {
		release()
		t.Fatalf("Acquire(%s) succeeded, want %s rejection", id, limit)
	}
	if !errors.Is(err, ErrLimitExceeded) {
		t.Fatalf("err = %v, want ErrLimitExceeded", err)
	}
	le, ok := errors.AsType[*Error](err)
	if !ok {
		t.Fatalf("err = %T, want *Error", err)
	}
	if le.ClientID != id || le.Limit != limit || le.RetryAfter != retryAfter {
		t.Errorf("err = %+v, want client %s, limit %s, RetryAfter %v", le, id, limit, retryAfter)
	}
}

func TestBurstThenRejected(t *testing.T) {
	l, _ := newTestLimiter(t, map[string]Limits{"a": {RequestsPerMinute: 60, Burst: 3, MaxConcurrent: 10}})
	for range 3 {
		mustAcquire(t, l, "a")
	}
	// One request is earned per second.
	wantRejected(t, l, "a", RequestRate, time.Second)
}

func TestRateRefills(t *testing.T) {
	l, c := newTestLimiter(t, map[string]Limits{"a": {RequestsPerMinute: 60, Burst: 3, MaxConcurrent: 10}})
	for range 3 {
		mustAcquire(t, l, "a")
	}

	c.Advance(400 * time.Millisecond)
	wantRejected(t, l, "a", RequestRate, 600*time.Millisecond)

	c.Advance(600 * time.Millisecond)
	mustAcquire(t, l, "a")
	wantRejected(t, l, "a", RequestRate, time.Second)

	// A long quiet period refills the bucket only up to Burst.
	c.Advance(time.Hour)
	for range 3 {
		mustAcquire(t, l, "a")
	}
	wantRejected(t, l, "a", RequestRate, time.Second)
}

func TestSustainedRate(t *testing.T) {
	l, c := newTestLimiter(t, map[string]Limits{"a": {RequestsPerMinute: 120, Burst: 1, MaxConcurrent: 10}})
	admitted := 0
	// Try every 100ms for one minute: only one request per 500ms passes.
	for range 600 {
		if release, err := l.Acquire("a"); err == nil {
			release()
			admitted++
		}
		c.Advance(100 * time.Millisecond)
	}
	if admitted != 120 {
		t.Errorf("admitted %d requests in a minute, want 120", admitted)
	}
}

func TestRejectedRequestConsumesNothing(t *testing.T) {
	l, c := newTestLimiter(t, map[string]Limits{"a": {RequestsPerMinute: 60, Burst: 1, MaxConcurrent: 10}})
	mustAcquire(t, l, "a")
	for range 5 {
		wantRejected(t, l, "a", RequestRate, time.Second)
	}
	c.Advance(time.Second)
	mustAcquire(t, l, "a")
}

func TestConcurrencyLimit(t *testing.T) {
	l, _ := newTestLimiter(t, map[string]Limits{"a": {RequestsPerMinute: 600, Burst: 10, MaxConcurrent: 2}})
	r1, err := l.Acquire("a")
	if err != nil {
		t.Fatalf("Acquire 1: %v", err)
	}
	r2, err := l.Acquire("a")
	if err != nil {
		t.Fatalf("Acquire 2: %v", err)
	}
	wantRejected(t, l, "a", ConcurrentRequests, 0)

	r1()
	r3, err := l.Acquire("a")
	if err != nil {
		t.Fatalf("Acquire after release: %v", err)
	}
	r2()
	r3()
}

func TestConcurrencyRejectionConsumesNoRate(t *testing.T) {
	l, _ := newTestLimiter(t, map[string]Limits{"a": {RequestsPerMinute: 60, Burst: 2, MaxConcurrent: 1}})
	release, err := l.Acquire("a")
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	for range 3 {
		wantRejected(t, l, "a", ConcurrentRequests, 0)
	}
	release()
	// The second request of the burst is still available.
	mustAcquire(t, l, "a")
}

func TestReleaseIsIdempotent(t *testing.T) {
	l, _ := newTestLimiter(t, map[string]Limits{"a": {RequestsPerMinute: 600, Burst: 10, MaxConcurrent: 1}})
	r1, err := l.Acquire("a")
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	r1()
	r1()
	r2, err := l.Acquire("a")
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	// A second release of r1 must not free r2's slot.
	r1()
	wantRejected(t, l, "a", ConcurrentRequests, 0)
	r2()
}

func TestClientsAreIndependent(t *testing.T) {
	l, _ := newTestLimiter(t, map[string]Limits{
		"a": {RequestsPerMinute: 60, Burst: 1, MaxConcurrent: 1},
		"b": {RequestsPerMinute: 60, Burst: 2, MaxConcurrent: 2},
	})
	release, err := l.Acquire("a")
	if err != nil {
		t.Fatalf("Acquire(a): %v", err)
	}
	wantRejected(t, l, "a", ConcurrentRequests, 0)
	release()
	wantRejected(t, l, "a", RequestRate, time.Second)

	// a's exhausted limits do not affect b, which has its own.
	rb1, err := l.Acquire("b")
	if err != nil {
		t.Fatalf("Acquire(b): %v", err)
	}
	rb2, err := l.Acquire("b")
	if err != nil {
		t.Fatalf("Acquire(b): %v", err)
	}
	wantRejected(t, l, "b", ConcurrentRequests, 0)
	rb1()
	rb2()
	wantRejected(t, l, "b", RequestRate, time.Second)
}

func TestUnknownClient(t *testing.T) {
	l, _ := newTestLimiter(t, map[string]Limits{"a": {RequestsPerMinute: 60, Burst: 1, MaxConcurrent: 1}})
	release, err := l.Acquire("nobody")
	if !errors.Is(err, ErrUnknownClient) {
		t.Fatalf("err = %v, want ErrUnknownClient", err)
	}
	if errors.Is(err, ErrLimitExceeded) {
		t.Error("an unknown client must not look like a rate limit")
	}
	if release != nil {
		t.Error("release must be nil on error")
	}
}

func TestErrorMessage(t *testing.T) {
	err := &Error{ClientID: "team-a", Limit: RequestRate, RetryAfter: time.Second}
	if got, want := err.Error(), "client team-a: rate limit exceeded: request_rate"; got != want {
		t.Errorf("Error() = %q, want %q", got, want)
	}
}

func TestNewRejectsInvalidLimits(t *testing.T) {
	ok := Limits{RequestsPerMinute: 60, Burst: 1, MaxConcurrent: 1}
	for name, tc := range map[string]struct {
		limits map[string]Limits
		want   string
	}{
		"no clients":     {nil, "no clients"},
		"blank ID":       {map[string]Limits{" ": ok}, "blank client ID"},
		"zero rate":      {map[string]Limits{"a": {Burst: 1, MaxConcurrent: 1}}, "requests per minute"},
		"negative rate":  {map[string]Limits{"a": {RequestsPerMinute: -1, Burst: 1, MaxConcurrent: 1}}, "requests per minute"},
		"zero burst":     {map[string]Limits{"a": {RequestsPerMinute: 60, MaxConcurrent: 1}}, "burst"},
		"zero max conc.": {map[string]Limits{"a": {RequestsPerMinute: 60, Burst: 1}}, "max concurrent"},
		"rate too high":  {map[string]Limits{"a": {RequestsPerMinute: MaxLimit + 1, Burst: 1, MaxConcurrent: 1}}, "requests per minute"},
		"burst too high": {map[string]Limits{"a": {RequestsPerMinute: 60, Burst: MaxLimit + 1, MaxConcurrent: 1}}, "burst"},
		"conc. too high": {map[string]Limits{"a": {RequestsPerMinute: 60, Burst: 1, MaxConcurrent: MaxLimit + 1}}, "max concurrent"},
	} {
		t.Run(name, func(t *testing.T) {
			l, err := New(tc.limits)
			if err == nil {
				t.Fatalf("New succeeded: %+v", l)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("err = %q, want it to contain %q", err, tc.want)
			}
		})
	}
}

func TestMaxLimits(t *testing.T) {
	l, c := newTestLimiter(t, map[string]Limits{"a": {RequestsPerMinute: MaxLimit, Burst: MaxLimit, MaxConcurrent: MaxLimit}})
	for range 1000 {
		mustAcquire(t, l, "a")
	}
	c.Advance(time.Minute)
	mustAcquire(t, l, "a")
}

func TestConcurrentAcquire(t *testing.T) {
	const (
		burst      = 50
		goroutines = 200
	)
	// The clock never advances, so exactly Burst requests pass however the
	// goroutines interleave, and MaxConcurrent is never the limit.
	l, _ := newTestLimiter(t, map[string]Limits{"a": {RequestsPerMinute: 1, Burst: burst, MaxConcurrent: goroutines}})

	var (
		mu       sync.Mutex
		admitted int
		wg       sync.WaitGroup
	)
	start := make(chan struct{})
	for range goroutines {
		wg.Go(func() {
			<-start
			release, err := l.Acquire("a")
			if err != nil {
				if !errors.Is(err, ErrLimitExceeded) {
					t.Errorf("Acquire: %v", err)
				}
				return
			}
			defer release()
			mu.Lock()
			admitted++
			mu.Unlock()
		})
	}
	close(start)
	wg.Wait()
	if admitted != burst {
		t.Errorf("admitted %d, want %d", admitted, burst)
	}
}

func TestConcurrentSlotsNeverExceeded(t *testing.T) {
	const maxConcurrent = 3
	l, _ := newTestLimiter(t, map[string]Limits{"a": {RequestsPerMinute: 1, Burst: 1000, MaxConcurrent: maxConcurrent}})

	var (
		mu            sync.Mutex
		inFlight, top int
		wg            sync.WaitGroup
	)
	for range 100 {
		wg.Go(func() {
			release, err := l.Acquire("a")
			if err != nil {
				return
			}
			mu.Lock()
			inFlight++
			top = max(top, inFlight)
			mu.Unlock()

			mu.Lock()
			inFlight--
			mu.Unlock()
			release()
		})
	}
	wg.Wait()
	if top > maxConcurrent {
		t.Errorf("%d requests in flight at once, want at most %d", top, maxConcurrent)
	}
}
