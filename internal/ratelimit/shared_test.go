package ratelimit

import (
	"context"
	"crypto/rand"
	"errors"
	"io"
	"net"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
)

const testRedisURLVar = "GATEWAY_TEST_REDIS_URL"

// testTimeout bounds each Redis call, and testLease holds each
// concurrency slot, in tests that do not exercise them.
const (
	testTimeout = 5 * time.Second
	testLease   = time.Minute
)

// testRedis returns a client for the Redis named by GATEWAY_TEST_REDIS_URL
// and a key prefix of the test's own. The test's keys are deleted when it
// ends; nothing else in Redis is touched. Without the variable the test is
// skipped, except in CI, where a missing Redis must not pass silently.
func testRedis(t *testing.T) (*redis.Client, string) {
	t.Helper()
	url := os.Getenv(testRedisURLVar)
	if url == "" {
		if os.Getenv("CI") != "" {
			t.Fatalf("%s is not set; CI must run the Redis integration tests", testRedisURLVar)
		}
		t.Skipf("%s is not set; run make redis to enable the Redis integration tests", testRedisURLVar)
	}
	opts, err := redis.ParseURL(url)
	if err != nil {
		t.Fatalf("parse %s: invalid URL", testRedisURLVar)
	}
	opts.ContextTimeoutEnabled = true
	client := redis.NewClient(opts)
	prefix := "llm-gateway-test:" + rand.Text() + ":"
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), testTimeout)
		defer cancel()
		keys, err := scanKeys(ctx, client, prefix)
		if err == nil && len(keys) > 0 {
			err = client.Del(ctx, keys...).Err()
		}
		if err != nil {
			t.Errorf("delete test keys: %v", err)
		}
		client.Close()
	})
	if err := client.Ping(t.Context()).Err(); err != nil {
		t.Fatalf("ping Redis: %v", err)
	}
	return client, prefix
}

// scanKeys returns the keys starting with prefix. SCAN, unlike KEYS, does
// not block a Redis shared with other users.
func scanKeys(ctx context.Context, client *redis.Client, prefix string) ([]string, error) {
	var keys []string
	iter := client.Scan(ctx, 0, prefix+"*", 100).Iterator()
	for iter.Next(ctx) {
		keys = append(keys, iter.Val())
	}
	return keys, iter.Err()
}

func newShared(t *testing.T, client redis.Scripter, prefix string, limits map[string]Limits) *Shared {
	t.Helper()
	return newSharedWith(t, client, limits, SharedOptions{KeyPrefix: prefix, Timeout: testTimeout, Lease: testLease})
}

func newSharedWith(t *testing.T, client redis.Scripter, limits map[string]Limits, opts SharedOptions) *Shared {
	t.Helper()
	s, err := NewShared(client, limits, opts)
	if err != nil {
		t.Fatalf("NewShared: %v", err)
	}
	return s
}

func sharedAcquire(t *testing.T, s *Shared, id string) {
	t.Helper()
	release, err := s.Acquire(t.Context(), id)
	if err != nil {
		t.Fatalf("Acquire(%s): %v", id, err)
	}
	release()
}

// sharedRejected asserts a request-rate rejection and returns its
// RetryAfter.
func sharedRejected(t *testing.T, s *Shared, id string) time.Duration {
	t.Helper()
	return sharedRejectedBy(t, s, id, RequestRate)
}

// sharedRejectedBy asserts a rejection by limit and returns its RetryAfter.
func sharedRejectedBy(t *testing.T, s *Shared, id string, limit Limit) time.Duration {
	t.Helper()
	release, err := s.Acquire(t.Context(), id)
	if err == nil {
		release()
		t.Fatalf("Acquire(%s) succeeded, want a %s rejection", id, limit)
	}
	le, ok := errors.AsType[*Error](err)
	if !ok || !errors.Is(err, ErrLimitExceeded) {
		t.Fatalf("err = %v, want *Error matching ErrLimitExceeded", err)
	}
	if le.ClientID != id || le.Limit != limit {
		t.Fatalf("err = %+v, want client %s, limit %s", le, id, limit)
	}
	return le.RetryAfter
}

func TestNewSharedValidates(t *testing.T) {
	client := redis.NewClient(&redis.Options{Addr: "127.0.0.1:1"})
	t.Cleanup(func() { client.Close() })
	valid := map[string]Limits{"a": {RequestsPerMinute: 1, Burst: 1, MaxConcurrent: 1}}
	for name, tc := range map[string]struct {
		store  redis.Scripter
		limits map[string]Limits
		opts   SharedOptions
	}{
		"nil store":      {nil, valid, SharedOptions{Timeout: time.Second, Lease: time.Minute}},
		"zero timeout":   {client, valid, SharedOptions{Lease: time.Minute}},
		"zero lease":     {client, valid, SharedOptions{Timeout: time.Second}},
		"sub-ms lease":   {client, valid, SharedOptions{Timeout: time.Second, Lease: time.Microsecond}},
		"no clients":     {client, nil, SharedOptions{Timeout: time.Second, Lease: time.Minute}},
		"invalid limits": {client, map[string]Limits{"a": {RequestsPerMinute: 1, Burst: 0, MaxConcurrent: 1}}, SharedOptions{Timeout: time.Second, Lease: time.Minute}},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := NewShared(tc.store, tc.limits, tc.opts); err == nil {
				t.Error("NewShared succeeded, want error")
			}
		})
	}
}

func TestSharedKeys(t *testing.T) {
	client := redis.NewClient(&redis.Options{Addr: "127.0.0.1:1"})
	t.Cleanup(func() { client.Close() })
	limits := map[string]Limits{"team-a": {RequestsPerMinute: 7, Burst: 3, MaxConcurrent: 1}}

	s := newShared(t, client, "", limits)
	c := s.clients["team-a"]
	if c.rateKey != "llm-gateway:ratelimit:{team-a}:rate" || c.activeKey != "llm-gateway:ratelimit:{team-a}:active" {
		t.Errorf("keys = %q, %q", c.rateKey, c.activeKey)
	}
	// 60s / 7 truncated to microseconds.
	if c.interval != 8_571_428 || c.tolerance != 2*8_571_428 {
		t.Errorf("interval, tolerance = %d, %d µs", c.interval, c.tolerance)
	}
}

func TestSharedUnknownClientDoesNotContactRedis(t *testing.T) {
	// Nothing listens on port 1, so any Redis call would fail differently.
	client := redis.NewClient(&redis.Options{Addr: "127.0.0.1:1"})
	t.Cleanup(func() { client.Close() })
	s := newShared(t, client, "", map[string]Limits{"a": {RequestsPerMinute: 1, Burst: 1, MaxConcurrent: 1}})

	release, err := s.Acquire(t.Context(), "nobody")
	if !errors.Is(err, ErrUnknownClient) || release != nil {
		t.Errorf("Acquire(nobody) = %v, %v; want ErrUnknownClient", release != nil, err)
	}
}

// hangingRedis accepts connections and reads from them but never answers,
// like a Redis that has stopped responding. It returns its address.
func hangingRedis(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	var (
		mu    sync.Mutex
		conns []net.Conn
		wg    sync.WaitGroup
	)
	wg.Go(func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			mu.Lock()
			conns = append(conns, conn)
			mu.Unlock()
			wg.Go(func() { _, _ = io.Copy(io.Discard, conn) })
		}
	})
	t.Cleanup(func() {
		ln.Close()
		mu.Lock()
		for _, c := range conns {
			c.Close()
		}
		mu.Unlock()
		wg.Wait()
	})
	return ln.Addr().String()
}

func TestSharedTimesOutOnUnresponsiveRedis(t *testing.T) {
	client := redis.NewClient(&redis.Options{Addr: hangingRedis(t), ContextTimeoutEnabled: true})
	t.Cleanup(func() { client.Close() })
	s, err := NewShared(client, map[string]Limits{"a": {RequestsPerMinute: 1, Burst: 1, MaxConcurrent: 1}}, SharedOptions{Timeout: 100 * time.Millisecond, Lease: testLease})
	if err != nil {
		t.Fatalf("NewShared: %v", err)
	}

	done := make(chan error, 1)
	go func() {
		_, err := s.Acquire(t.Context(), "a")
		done <- err
	}()
	select {
	case err := <-done:
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Errorf("err = %v, want DeadlineExceeded", err)
		}
		if _, ok := errors.AsType[*Error](err); ok {
			t.Errorf("err = %v is a limit rejection, want a store failure", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Acquire did not return within 5s against an unresponsive Redis")
	}
}

func TestSharedCanceledRequest(t *testing.T) {
	client := redis.NewClient(&redis.Options{Addr: hangingRedis(t), ContextTimeoutEnabled: true})
	t.Cleanup(func() { client.Close() })
	s := newShared(t, client, "", map[string]Limits{"a": {RequestsPerMinute: 1, Burst: 1, MaxConcurrent: 1}})

	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := s.Acquire(ctx, "a"); !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v, want Canceled", err)
	}
}

func TestSharedBurstThenRejected(t *testing.T) {
	client, prefix := testRedis(t)
	s := newShared(t, client, prefix, map[string]Limits{"a": {RequestsPerMinute: 1, Burst: 3, MaxConcurrent: 1}})

	for range 3 {
		sharedAcquire(t, s, "a")
	}
	// The next request is earned one minute after the first, less the
	// moments the test took.
	if got := sharedRejected(t, s, "a"); got <= 0 || got > time.Minute {
		t.Errorf("RetryAfter = %v, want in (0, 1m]", got)
	}
}

func TestSharedRejectionConsumesNothing(t *testing.T) {
	client, prefix := testRedis(t)
	s := newShared(t, client, prefix, map[string]Limits{"a": {RequestsPerMinute: 1, Burst: 1, MaxConcurrent: 1}})

	sharedAcquire(t, s, "a")
	first := sharedRejected(t, s, "a")
	for range 5 {
		sharedRejected(t, s, "a")
	}
	// Had the rejections consumed tokens, the wait would have grown by
	// minutes; it can only have shrunk as time passed.
	if last := sharedRejected(t, s, "a"); last > first {
		t.Errorf("RetryAfter grew from %v to %v after rejected requests", first, last)
	}
}

func TestSharedRefill(t *testing.T) {
	client, prefix := testRedis(t)
	// One request every 10ms.
	s := newShared(t, client, prefix, map[string]Limits{"a": {RequestsPerMinute: 6000, Burst: 1, MaxConcurrent: 1}})

	sharedAcquire(t, s, "a")
	wait := sharedRejected(t, s, "a")
	if wait <= 0 || wait > 10*time.Millisecond {
		t.Fatalf("RetryAfter = %v, want in (0, 10ms]", wait)
	}
	// Waiting RetryAfter is enough to be admitted.
	time.Sleep(wait)
	sharedAcquire(t, s, "a")
}

func TestSharedClientsAreIndependent(t *testing.T) {
	client, prefix := testRedis(t)
	s := newShared(t, client, prefix, map[string]Limits{
		"a": {RequestsPerMinute: 1, Burst: 1, MaxConcurrent: 1},
		"b": {RequestsPerMinute: 1, Burst: 2, MaxConcurrent: 1},
	})

	sharedAcquire(t, s, "a")
	sharedRejected(t, s, "a")
	sharedAcquire(t, s, "b")
	sharedAcquire(t, s, "b")
	sharedRejected(t, s, "b")
}

func TestSharedInstancesShareABucket(t *testing.T) {
	client, prefix := testRedis(t)
	limits := map[string]Limits{"a": {RequestsPerMinute: 1, Burst: 2, MaxConcurrent: 1}}
	// Two limiters with their own connections, as two gateway instances
	// would have.
	other := redis.NewClient(client.Options())
	t.Cleanup(func() { other.Close() })
	first := newShared(t, client, prefix, limits)
	second := newShared(t, other, prefix, limits)

	sharedAcquire(t, first, "a")
	sharedAcquire(t, second, "a")
	sharedRejected(t, first, "a")
	sharedRejected(t, second, "a")

	// Another prefix is another set of buckets.
	separate := newShared(t, client, prefix+"other:", limits)
	sharedAcquire(t, separate, "a")
}

func TestSharedKeysExpire(t *testing.T) {
	client, prefix := testRedis(t)
	const lease = 30 * time.Second
	s := newSharedWith(t, client, map[string]Limits{"a": {RequestsPerMinute: 1, Burst: 3, MaxConcurrent: 1}},
		SharedOptions{KeyPrefix: prefix, Timeout: testTimeout, Lease: lease})

	release, err := s.Acquire(t.Context(), "a")
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	ttl, err := client.PTTL(t.Context(), s.clients["a"].rateKey).Result()
	if err != nil {
		t.Fatalf("PTTL: %v", err)
	}
	// One request taken: the bucket is full again after one interval.
	if ttl <= 0 || ttl > time.Minute {
		t.Errorf("rate key PTTL = %v, want in (0, 1m]", ttl)
	}
	// The slot set expires when its newest lease would.
	ttl, err = client.PTTL(t.Context(), s.clients["a"].activeKey).Result()
	if err != nil {
		t.Fatalf("PTTL: %v", err)
	}
	if ttl <= 0 || ttl > lease {
		t.Errorf("active key PTTL = %v, want in (0, %v]", ttl, lease)
	}
	release()

	// The arrival time is stored as an exact integer, not in Lua's default
	// exponent notation, which would round it to whole tenths of a second.
	tat, err := client.Get(t.Context(), s.clients["a"].rateKey).Result()
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	if strings.Trim(tat, "0123456789") != "" || len(tat) < 16 {
		t.Errorf("stored arrival time = %q, want microseconds as an integer", tat)
	}
	keys, err := scanKeys(t.Context(), client, prefix)
	if err != nil {
		t.Fatalf("SCAN: %v", err)
	}
	// Releasing the last slot removes the empty set.
	if len(keys) != 1 || !strings.HasSuffix(keys[0], "{a}:rate") {
		t.Errorf("keys after release = %v, want only the rate key", keys)
	}
}

func TestSharedConcurrentAcquire(t *testing.T) {
	client, prefix := testRedis(t)
	const burst, goroutines = 10, 50
	// MaxConcurrent is never the limit, so only the rate decides.
	limits := map[string]Limits{"a": {RequestsPerMinute: 1, Burst: burst, MaxConcurrent: goroutines}}
	other := redis.NewClient(client.Options())
	t.Cleanup(func() { other.Close() })
	limiters := []*Shared{newShared(t, client, prefix, limits), newShared(t, other, prefix, limits)}

	var (
		mu       sync.Mutex
		admitted int
		wg       sync.WaitGroup
	)
	start := make(chan struct{})
	for i := range goroutines {
		wg.Go(func() {
			<-start
			release, err := limiters[i%2].Acquire(t.Context(), "a")
			if err != nil {
				if !errors.Is(err, ErrLimitExceeded) {
					t.Errorf("Acquire: %v", err)
				}
				return
			}
			release()
			mu.Lock()
			admitted++
			mu.Unlock()
		})
	}
	close(start)
	wg.Wait()
	// One request a minute: exactly the burst passes across both
	// instances, however the calls interleave.
	if admitted != burst {
		t.Errorf("admitted %d, want %d", admitted, burst)
	}
}

// activeLeases returns the number of leases held for clientID, expired or
// not.
func activeLeases(t *testing.T, client *redis.Client, s *Shared, clientID string) int64 {
	t.Helper()
	n, err := client.ZCard(t.Context(), s.clients[clientID].activeKey).Result()
	if err != nil {
		t.Fatalf("ZCARD: %v", err)
	}
	return n
}

func TestSharedConcurrencyLimitAcrossInstances(t *testing.T) {
	client, prefix := testRedis(t)
	limits := map[string]Limits{
		"a": {RequestsPerMinute: MaxLimit, Burst: MaxLimit, MaxConcurrent: 2},
		"b": {RequestsPerMinute: MaxLimit, Burst: MaxLimit, MaxConcurrent: 1},
	}
	other := redis.NewClient(client.Options())
	t.Cleanup(func() { other.Close() })
	first := newShared(t, client, prefix, limits)
	second := newShared(t, other, prefix, limits)

	r1, err := first.Acquire(t.Context(), "a")
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	r2, err := second.Acquire(t.Context(), "a")
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	// Both slots are taken, whichever instance asks.
	if got := sharedRejectedBy(t, first, "a", ConcurrentRequests); got != 0 {
		t.Errorf("RetryAfter = %v, want 0 for the concurrency limit", got)
	}
	sharedRejectedBy(t, second, "a", ConcurrentRequests)
	// Another client has its own slots.
	sharedAcquire(t, second, "b")

	// A slot released on one instance is free on the other.
	r1()
	r3, err := second.Acquire(t.Context(), "a")
	if err != nil {
		t.Fatalf("Acquire after release: %v", err)
	}
	r2()
	r3()
	if n := activeLeases(t, client, first, "a"); n != 0 {
		t.Errorf("%d leases held after every release, want 0", n)
	}
}

func TestSharedReleaseIsIdempotent(t *testing.T) {
	client, prefix := testRedis(t)
	s := newShared(t, client, prefix, map[string]Limits{"a": {RequestsPerMinute: MaxLimit, Burst: MaxLimit, MaxConcurrent: 2}})

	r1, err := s.Acquire(t.Context(), "a")
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	r2, err := s.Acquire(t.Context(), "a")
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	r1()
	r1()
	// The second request still holds its slot.
	if n := activeLeases(t, client, s, "a"); n != 1 {
		t.Errorf("%d leases held, want 1", n)
	}
	r2()
}

func TestSharedReleaseAfterCancellation(t *testing.T) {
	client, prefix := testRedis(t)
	s := newShared(t, client, prefix, map[string]Limits{"a": {RequestsPerMinute: MaxLimit, Burst: MaxLimit, MaxConcurrent: 1}})

	ctx, cancel := context.WithCancel(t.Context())
	release, err := s.Acquire(ctx, "a")
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	// The client went away before the request finished.
	cancel()
	release()
	if n := activeLeases(t, client, s, "a"); n != 0 {
		t.Errorf("%d leases held after release, want 0", n)
	}
}

func TestSharedUnreleasedLeaseExpires(t *testing.T) {
	client, prefix := testRedis(t)
	const lease = 200 * time.Millisecond
	s := newSharedWith(t, client, map[string]Limits{"a": {RequestsPerMinute: MaxLimit, Burst: MaxLimit, MaxConcurrent: 1}},
		SharedOptions{KeyPrefix: prefix, Timeout: testTimeout, Lease: lease})

	// Never released, as if the instance holding it had crashed.
	if _, err := s.Acquire(t.Context(), "a"); err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	sharedRejectedBy(t, s, "a", ConcurrentRequests)

	// Once the lease has expired, the slot is free.
	time.Sleep(lease)
	sharedAcquire(t, s, "a")
}

func TestSharedConcurrencyRejectionConsumesNoRate(t *testing.T) {
	client, prefix := testRedis(t)
	s := newShared(t, client, prefix, map[string]Limits{"a": {RequestsPerMinute: 1, Burst: 2, MaxConcurrent: 1}})

	release, err := s.Acquire(t.Context(), "a")
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	for range 3 {
		sharedRejectedBy(t, s, "a", ConcurrentRequests)
	}
	release()
	// The second token of the burst is still there.
	sharedAcquire(t, s, "a")
	sharedRejected(t, s, "a")
}

func TestSharedRateRejectionHoldsNoSlot(t *testing.T) {
	client, prefix := testRedis(t)
	s := newShared(t, client, prefix, map[string]Limits{"a": {RequestsPerMinute: 1, Burst: 1, MaxConcurrent: 5}})

	release, err := s.Acquire(t.Context(), "a")
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	defer release()
	for range 3 {
		sharedRejected(t, s, "a")
	}
	if n := activeLeases(t, client, s, "a"); n != 1 {
		t.Errorf("%d leases held, want only the admitted request's", n)
	}
}

func TestSharedReleaseIsBounded(t *testing.T) {
	client, prefix := testRedis(t)
	s := newSharedWith(t, client, map[string]Limits{"a": {RequestsPerMinute: MaxLimit, Burst: MaxLimit, MaxConcurrent: 1}},
		SharedOptions{KeyPrefix: prefix, Timeout: 100 * time.Millisecond, Lease: testLease})
	release, err := s.Acquire(t.Context(), "a")
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}

	// Redis stops answering before the request finishes.
	hanging := redis.NewClient(&redis.Options{Addr: hangingRedis(t), ContextTimeoutEnabled: true})
	t.Cleanup(func() { hanging.Close() })
	s.store = hanging

	done := make(chan struct{})
	go func() {
		release()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("release did not return within 5s against an unresponsive Redis")
	}
}

func TestSharedConcurrentSlotsNeverExceeded(t *testing.T) {
	client, prefix := testRedis(t)
	const maxConcurrent, goroutines = 3, 40
	limits := map[string]Limits{"a": {RequestsPerMinute: MaxLimit, Burst: MaxLimit, MaxConcurrent: maxConcurrent}}
	other := redis.NewClient(client.Options())
	t.Cleanup(func() { other.Close() })
	limiters := []*Shared{newShared(t, client, prefix, limits), newShared(t, other, prefix, limits)}

	var (
		mu            sync.Mutex
		inFlight, top int
		admitted      int
		wg            sync.WaitGroup
	)
	start := make(chan struct{})
	for i := range goroutines {
		wg.Go(func() {
			<-start
			release, err := limiters[i%2].Acquire(t.Context(), "a")
			if err != nil {
				if !errors.Is(err, ErrLimitExceeded) {
					t.Errorf("Acquire: %v", err)
				}
				return
			}
			mu.Lock()
			inFlight++
			admitted++
			top = max(top, inFlight)
			mu.Unlock()

			mu.Lock()
			inFlight--
			mu.Unlock()
			release()
		})
	}
	close(start)
	wg.Wait()
	if top > maxConcurrent {
		t.Errorf("%d requests in flight at once, want at most %d", top, maxConcurrent)
	}
	if admitted == 0 {
		t.Error("no request admitted")
	}
	if n := activeLeases(t, client, limiters[0], "a"); n != 0 {
		t.Errorf("%d leases held after every release, want 0", n)
	}
}
