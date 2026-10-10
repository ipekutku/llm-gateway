//go:build performance

package main

import (
	"context"
	"log/slog"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ipekutku/llm-gateway/internal/ratelimit"
	"github.com/redis/go-redis/v9"
)

// Burst probe, not a throughput benchmark: 500 simultaneous admissions,
// then 500 simultaneous releases, repeated five times. It uses production
// client options and the healthy fallback wrapper, with test-owned keys.
// Stats report pool semaphore waits, not every possible dial/network wait.
func TestRedisPoolConcurrentMeasurement(t *testing.T) {
	opts, prefix := testRedis(t)
	client := redis.NewClient(redisClientOptions(opts))
	defer client.Close()
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	limits := map[string]ratelimit.Limits{"probe": {RequestsPerMinute: 1000000, Burst: 1000000, MaxConcurrent: 500}}
	var acquireNS, releaseNS, acquireCount, releaseCount atomic.Int64
	shared, err := ratelimit.NewShared(client, limits, ratelimit.SharedOptions{
		KeyPrefix: prefix, Timeout: redisCallTimeout, Lease: redisLease(defaultUpstreamTimeout),
		OnCall: func(op string, elapsed time.Duration, err error) {
			if err != nil {
				t.Errorf("Redis %s failed", op)
			}
			if op == "acquire" {
				acquireNS.Add(int64(elapsed))
				acquireCount.Add(1)
			} else {
				releaseNS.Add(int64(elapsed))
				releaseCount.Add(1)
			}
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	local, err := ratelimit.New(limits)
	if err != nil {
		t.Fatal(err)
	}
	f, err := ratelimit.NewFallback(shared, local, ratelimit.FallbackSettings{RetryInterval: redisRetryInterval}, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	// Load scripts without pre-growing the connection pool.
	release, err := f.Acquire(ctx, "probe")
	if err != nil {
		t.Fatal(err)
	}
	release()
	for wave := 1; wave <= 5; wave++ {
		before := client.PoolStats()
		a0, r0, ac0, rc0 := acquireNS.Load(), releaseNS.Load(), acquireCount.Load(), releaseCount.Load()
		start, admitted, releaseAll, done := make(chan struct{}), make(chan struct{}, 500), make(chan struct{}), make(chan struct{}, 500)
		for range 500 {
			go func() {
				<-start
				release, err := f.Acquire(ctx, "probe")
				if err != nil {
					t.Error("admission failed")
				}
				admitted <- struct{}{}
				select {
				case <-releaseAll:
				case <-ctx.Done():
				}
				if err == nil {
					release()
				}
				done <- struct{}{}
			}()
		}
		close(start)
		for range 500 {
			<-admitted
		}
		atAdmission := client.PoolStats()
		close(releaseAll)
		for range 500 {
			<-done
		}
		after := client.PoolStats()
		if f.Mode() != ratelimit.SharedMode {
			t.Fatal("probe degraded to local limits")
		}
		if acquireCount.Load()-ac0 != 500 || releaseCount.Load()-rc0 != 500 {
			t.Fatal("missing Redis calls")
		}
		t.Logf("wave=%d base_pool=%d max_active=%d connections_admitted=%d connections_end=%d waits=%d wait_ms=%.3f timeouts=%d mean_acquire_ms=%.3f mean_release_ms=%.3f", wave,
			client.Options().PoolSize, client.Options().MaxActiveConns, atAdmission.TotalConns, after.TotalConns,
			after.WaitCount-before.WaitCount, float64(after.WaitDurationNs-before.WaitDurationNs)/1e6, after.Timeouts-before.Timeouts,
			float64(acquireNS.Load()-a0)/500/1e6, float64(releaseNS.Load()-r0)/500/1e6)
	}
}
