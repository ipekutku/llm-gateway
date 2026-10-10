package ratelimit

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
)

// switchStore is a redis.Scripter that sends calls to a target the test can
// replace, for example to take Redis away and bring it back, and counts the
// scripts run through it.
type switchStore struct {
	mu     sync.Mutex
	target redis.Scripter
	calls  int
	// called, if not nil, receives a value whenever a script is run.
	called chan struct{}
}

func (s *switchStore) set(target redis.Scripter) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.target = target
}

// runs returns how many scripts have been run, acquiring or releasing,
// counting each EVALSHA, the first command of every script run.
func (s *switchStore) runs() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.calls
}

func (s *switchStore) current() redis.Scripter {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.target
}

func (s *switchStore) EvalSha(ctx context.Context, sha1 string, keys []string, args ...any) *redis.Cmd {
	s.mu.Lock()
	s.calls++
	target, called := s.target, s.called
	s.mu.Unlock()
	if called != nil {
		called <- struct{}{}
	}
	return target.EvalSha(ctx, sha1, keys, args...)
}

func (s *switchStore) Eval(ctx context.Context, script string, keys []string, args ...any) *redis.Cmd {
	return s.current().Eval(ctx, script, keys, args...)
}

func (s *switchStore) EvalRO(ctx context.Context, script string, keys []string, args ...any) *redis.Cmd {
	return s.current().EvalRO(ctx, script, keys, args...)
}

func (s *switchStore) EvalShaRO(ctx context.Context, sha1 string, keys []string, args ...any) *redis.Cmd {
	return s.current().EvalShaRO(ctx, sha1, keys, args...)
}

func (s *switchStore) ScriptExists(ctx context.Context, hashes ...string) *redis.BoolSliceCmd {
	return s.current().ScriptExists(ctx, hashes...)
}

func (s *switchStore) ScriptLoad(ctx context.Context, script string) *redis.StringCmd {
	return s.current().ScriptLoad(ctx, script)
}

// downRedis returns a client for an address where nothing listens, like a
// Redis that is down. Command and dial retries are off, so every call
// fails at once.
func downRedis(t *testing.T) *redis.Client {
	t.Helper()
	client := redis.NewClient(&redis.Options{Addr: "127.0.0.1:1", MaxRetries: -1, DialerRetries: 1, ContextTimeoutEnabled: true})
	t.Cleanup(func() { client.Close() })
	return client
}

func unresponsiveRedis(t *testing.T) *redis.Client {
	t.Helper()
	client := redis.NewClient(&redis.Options{Addr: hangingRedis(t), MaxRetries: -1, ContextTimeoutEnabled: true})
	t.Cleanup(func() { client.Close() })
	return client
}

const testRetryInterval = time.Second

// fallbackTest is a Fallback over a switchStore, with a manual clock, its
// logs, and the modes it reported.
type fallbackTest struct {
	*Fallback
	store *switchStore
	clock *clock
	logs  *bytes.Buffer

	mu    sync.Mutex
	modes []Mode
}

func newFallbackTest(t *testing.T, target redis.Scripter, prefix string, limits map[string]Limits, timeout time.Duration) *fallbackTest {
	t.Helper()
	ft := &fallbackTest{store: &switchStore{target: target}, logs: &bytes.Buffer{}}
	shared := newSharedWith(t, ft.store, limits, SharedOptions{KeyPrefix: prefix, Timeout: timeout, Lease: testLease})
	local, c := newTestLimiter(t, limits)
	ft.clock = c
	f, err := NewFallback(shared, local, FallbackSettings{
		RetryInterval: testRetryInterval,
		OnModeChange: func(m Mode) {
			ft.mu.Lock()
			ft.modes = append(ft.modes, m)
			ft.mu.Unlock()
		},
	}, slog.New(slog.NewTextHandler(ft.logs, nil)))
	if err != nil {
		t.Fatalf("NewFallback: %v", err)
	}
	f.now = c.Now
	ft.Fallback = f
	return ft
}

func (ft *fallbackTest) reportedModes() []Mode {
	ft.mu.Lock()
	defer ft.mu.Unlock()
	return append([]Mode(nil), ft.modes...)
}

func (ft *fallbackTest) wantMode(t *testing.T, want Mode) {
	t.Helper()
	if got := ft.Mode(); got != want {
		t.Errorf("mode = %s, want %s", got, want)
	}
}

func (ft *fallbackTest) wantLogs(t *testing.T, msg string, n int) {
	t.Helper()
	if got := strings.Count(ft.logs.String(), msg); got != n {
		t.Errorf("%d log lines %q, want %d:\n%s", got, msg, n, ft.logs)
	}
}

func fallbackAcquire(t *testing.T, f *Fallback, id string) {
	t.Helper()
	release, err := f.Acquire(t.Context(), id)
	if err != nil {
		t.Fatalf("Acquire(%s): %v", id, err)
	}
	release()
}

func fallbackRejected(t *testing.T, f *Fallback, id string, limit Limit) {
	t.Helper()
	release, err := f.Acquire(t.Context(), id)
	if err == nil {
		release()
		t.Fatalf("Acquire(%s) succeeded, want a %s rejection", id, limit)
	}
	if le, ok := errors.AsType[*Error](err); !ok || le.Limit != limit {
		t.Fatalf("err = %v, want a %s rejection", err, limit)
	}
}

const (
	msgUnavailable = "rate limit store unavailable, using local limits"
	msgRecovered   = "rate limit store recovered, using shared limits"
)

func TestNewFallbackValidates(t *testing.T) {
	limits := map[string]Limits{"a": {RequestsPerMinute: 1, Burst: 1, MaxConcurrent: 1}}
	shared := newSharedWith(t, downRedis(t), limits, SharedOptions{Timeout: time.Second, Lease: time.Minute})
	local, _ := newTestLimiter(t, limits)
	settings := FallbackSettings{RetryInterval: time.Second}
	if _, err := NewFallback(nil, local, settings, nil); err == nil {
		t.Error("NewFallback(nil shared) succeeded")
	}
	if _, err := NewFallback(shared, nil, settings, nil); err == nil {
		t.Error("NewFallback(nil local) succeeded")
	}
	if _, err := NewFallback(shared, local, FallbackSettings{}, nil); err == nil {
		t.Error("NewFallback(zero retry interval) succeeded")
	}
}

func TestFallbackUsesSharedLimits(t *testing.T) {
	client, prefix := testRedis(t)
	limits := map[string]Limits{"a": {RequestsPerMinute: 1, Burst: 2, MaxConcurrent: 5}}
	first := newFallbackTest(t, client, prefix, limits, testTimeout)
	second := newFallbackTest(t, client, prefix, limits, testTimeout)

	// One bucket for both instances; their local limits are never used.
	fallbackAcquire(t, first.Fallback, "a")
	fallbackAcquire(t, second.Fallback, "a")
	fallbackRejected(t, first.Fallback, "a", RequestRate)
	fallbackRejected(t, second.Fallback, "a", RequestRate)

	for _, ft := range []*fallbackTest{first, second} {
		ft.wantMode(t, SharedMode)
		if ft.logs.Len() != 0 || len(ft.reportedModes()) != 0 {
			t.Errorf("rejections changed the mode or logged:\n%s", ft.logs)
		}
	}
}

func TestFallbackUsesLocalLimitsWhenRedisIsDown(t *testing.T) {
	ft := newFallbackTest(t, downRedis(t), "", map[string]Limits{"a": {RequestsPerMinute: 1, Burst: 2, MaxConcurrent: 5}}, testTimeout)

	// The first request finds Redis down and is admitted locally; the
	// local bucket then applies the client's limits.
	fallbackAcquire(t, ft.Fallback, "a")
	fallbackAcquire(t, ft.Fallback, "a")
	for range 10 {
		fallbackRejected(t, ft.Fallback, "a", RequestRate)
	}

	ft.wantMode(t, LocalMode)
	if got := ft.reportedModes(); len(got) != 1 || got[0] != LocalMode {
		t.Errorf("reported modes = %v, want [local]", got)
	}
	ft.wantLogs(t, msgUnavailable, 1)
	for _, want := range []string{"level=WARN", "retry_in=1s", "error="} {
		if !strings.Contains(ft.logs.String(), want) {
			t.Errorf("log does not contain %q:\n%s", want, ft.logs)
		}
	}
	// Only the first request tried Redis.
	if n := ft.store.runs(); n != 1 {
		t.Errorf("%d Redis calls, want 1", n)
	}
}

func TestFallbackDoesNotWaitForRedisDuringOutage(t *testing.T) {
	ft := newFallbackTest(t, unresponsiveRedis(t), "", map[string]Limits{"a": generousLimits}, 100*time.Millisecond)

	// The first request waits for the Redis timeout, then is admitted
	// locally.
	fallbackAcquire(t, ft.Fallback, "a")
	ft.wantMode(t, LocalMode)

	// The others do not call Redis until the retry interval has passed.
	for range 20 {
		fallbackAcquire(t, ft.Fallback, "a")
	}
	if n := ft.store.runs(); n != 1 {
		t.Errorf("%d Redis calls, want 1", n)
	}
	ft.wantLogs(t, msgUnavailable, 1)
}

func TestFallbackRecovers(t *testing.T) {
	client, prefix := testRedis(t)
	// One request a minute keeps the rate key in Redis for the check below.
	limits := map[string]Limits{"a": {RequestsPerMinute: 1, Burst: MaxLimit, MaxConcurrent: MaxLimit}}
	ft := newFallbackTest(t, downRedis(t), prefix, limits, testTimeout)

	fallbackAcquire(t, ft.Fallback, "a")
	ft.wantMode(t, LocalMode)

	// Redis is back, but is not tried before the retry interval.
	ft.store.set(client)
	ft.clock.Advance(testRetryInterval - time.Millisecond)
	fallbackAcquire(t, ft.Fallback, "a")
	if n := ft.store.runs(); n != 1 {
		t.Errorf("%d Redis calls before the retry interval, want 1", n)
	}

	// After it, the next request probes Redis and returns to shared limits;
	// its release goes to Redis too.
	ft.clock.Advance(time.Millisecond)
	fallbackAcquire(t, ft.Fallback, "a")
	ft.wantMode(t, SharedMode)
	if n := ft.store.runs(); n != 3 {
		t.Errorf("%d Redis calls, want 3", n)
	}
	fallbackAcquire(t, ft.Fallback, "a")
	if n := ft.store.runs(); n != 5 {
		t.Errorf("%d Redis calls in shared mode, want 5", n)
	}
	if got := ft.reportedModes(); len(got) != 2 || got[0] != LocalMode || got[1] != SharedMode {
		t.Errorf("reported modes = %v, want [local shared]", got)
	}
	ft.wantLogs(t, msgUnavailable, 1)
	ft.wantLogs(t, msgRecovered, 1)
	if !strings.Contains(ft.logs.String(), "level=INFO msg=\""+msgRecovered) {
		t.Errorf("recovery not logged at info:\n%s", ft.logs)
	}
	// The probe and the request after it used the shared bucket.
	if n, err := client.Exists(t.Context(), ft.shared.clients["a"].rateKey).Result(); err != nil || n != 1 {
		t.Errorf("shared rate key exists = %d, %v; want 1", n, err)
	}
}

func TestFallbackFailedProbeStaysLocal(t *testing.T) {
	ft := newFallbackTest(t, downRedis(t), "", map[string]Limits{"a": generousLimits}, testTimeout)

	fallbackAcquire(t, ft.Fallback, "a")
	ft.clock.Advance(testRetryInterval)
	// The probe fails; the request is still admitted locally.
	fallbackAcquire(t, ft.Fallback, "a")
	// The next probe waits for another interval.
	fallbackAcquire(t, ft.Fallback, "a")

	ft.wantMode(t, LocalMode)
	if n := ft.store.runs(); n != 2 {
		t.Errorf("%d Redis calls, want 2", n)
	}
	ft.wantLogs(t, msgUnavailable, 1)
	if got := ft.reportedModes(); len(got) != 1 {
		t.Errorf("reported modes = %v, want [local]", got)
	}
}

func TestFallbackProbesOneAtATime(t *testing.T) {
	ft := newFallbackTest(t, downRedis(t), "", map[string]Limits{"a": generousLimits}, 200*time.Millisecond)
	fallbackAcquire(t, ft.Fallback, "a")

	// Redis now hangs. One request probes it and waits for the timeout.
	ft.store.set(unresponsiveRedis(t))
	called := make(chan struct{}, 1)
	ft.store.mu.Lock()
	ft.store.called = called
	ft.store.mu.Unlock()
	ft.clock.Advance(testRetryInterval)
	probe := make(chan error, 1)
	go func() {
		release, err := ft.Acquire(context.Background(), "a")
		if err == nil {
			release()
		}
		probe <- err
	}()
	select {
	case <-called:
	case <-time.After(5 * time.Second):
		t.Fatal("the probe did not reach Redis")
	}

	// Meanwhile, other requests are decided locally without waiting.
	fallbackAcquire(t, ft.Fallback, "a")
	if n := ft.store.runs(); n != 2 {
		t.Errorf("%d Redis calls during the probe, want 2", n)
	}
	if err := <-probe; err != nil {
		t.Errorf("probe request: %v, want admitted locally", err)
	}
	ft.wantMode(t, LocalMode)
}

func TestFallbackRequestCancellationIsNotAFailure(t *testing.T) {
	ft := newFallbackTest(t, unresponsiveRedis(t), "", map[string]Limits{"a": generousLimits}, 200*time.Millisecond)

	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := ft.Acquire(ctx, "a"); !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want Canceled", err)
	}
	ft.wantMode(t, SharedMode)
	if ft.logs.Len() != 0 {
		t.Errorf("cancellation logged:\n%s", ft.logs)
	}

	// In local mode, a probe canceled by its request lets the next request
	// probe at once.
	ft.store.set(downRedis(t))
	fallbackAcquire(t, ft.Fallback, "a")
	ft.clock.Advance(testRetryInterval)
	ft.store.set(unresponsiveRedis(t))
	if _, err := ft.Acquire(ctx, "a"); !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want Canceled", err)
	}
	before := ft.store.runs()
	ft.store.set(downRedis(t))
	fallbackAcquire(t, ft.Fallback, "a")
	if n := ft.store.runs(); n != before+1 {
		t.Errorf("%d Redis calls after a canceled probe, want %d", n, before+1)
	}
}

func TestFallbackUnknownClientIsNotAFailure(t *testing.T) {
	ft := newFallbackTest(t, downRedis(t), "", map[string]Limits{"a": generousLimits}, testTimeout)
	if _, err := ft.Acquire(t.Context(), "nobody"); !errors.Is(err, ErrUnknownClient) {
		t.Errorf("err = %v, want ErrUnknownClient", err)
	}
	ft.wantMode(t, SharedMode)
}

func TestFallbackReleasesToTheAdmittingLimiter(t *testing.T) {
	client, prefix := testRedis(t)
	limits := map[string]Limits{"a": {RequestsPerMinute: MaxLimit, Burst: MaxLimit, MaxConcurrent: 1}}
	ft := newFallbackTest(t, client, prefix, limits, testTimeout)

	// Admitted by Redis.
	shared, err := ft.Acquire(t.Context(), "a")
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	// Redis goes down; the next request is admitted by the local limits,
	// which do not know about the first.
	ft.store.set(downRedis(t))
	local, err := ft.Acquire(t.Context(), "a")
	if err != nil {
		t.Fatalf("Acquire in local mode: %v", err)
	}
	ft.wantMode(t, LocalMode)

	// Redis is back. The first request's release goes to Redis, even
	// though the Fallback is still in local mode.
	ft.store.set(client)
	shared()
	if n := activeLeases(t, client, ft.shared, "a"); n != 0 {
		t.Errorf("%d leases in Redis after release, want 0", n)
	}
	// The local slot is still held until its own release.
	fallbackRejected(t, ft.Fallback, "a", ConcurrentRequests)
	local()
	fallbackAcquire(t, ft.Fallback, "a")
}

// generousLimits are limits that tests not about them never reach.
var generousLimits = Limits{RequestsPerMinute: MaxLimit, Burst: MaxLimit, MaxConcurrent: MaxLimit}
