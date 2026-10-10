package ratelimit

import (
	"context"
	"log/slog"
	"math"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
)

type benchmarkLimiter interface {
	Acquire(context.Context, string) (func(), error)
}

// benchmarkLocal keeps the real clock and admission/release path. Only
// the bucket tolerance is widened before timing: calibration can run far
// more calls than MaxLimit, and we want admissions rather than rejections.
func benchmarkLocal(b *testing.B, count int) (*Limiter, []string) {
	b.Helper()
	limits := make(map[string]Limits, count)
	ids := make([]string, count)
	for i := range ids {
		ids[i] = "bench-" + strconv.Itoa(i)
		limits[ids[i]] = Limits{RequestsPerMinute: MaxLimit, Burst: MaxLimit, MaxConcurrent: MaxLimit}
	}
	l, err := New(limits)
	if err != nil {
		b.Fatal(err)
	}
	for _, c := range l.clients {
		if int64(b.N) > math.MaxInt64/int64(c.interval) {
			b.Fatal("benchmark iteration count overflows bucket tolerance")
		}
		c.tolerance = max(c.tolerance, time.Duration(b.N)*c.interval)
	}
	return l, ids
}

// benchmarkFallback stays in local mode between probes. Constructing the
// Redis client does not dial; this fixture never calls the shared limiter.
func benchmarkFallback(b *testing.B, local *Limiter, ids []string) *Fallback {
	b.Helper()
	client := redis.NewClient(&redis.Options{Addr: "127.0.0.1:1"})
	b.Cleanup(func() { client.Close() })
	limits := make(map[string]Limits, len(ids))
	for _, id := range ids {
		limits[id] = local.clients[id].limits
	}
	shared, err := NewShared(client, limits, SharedOptions{Timeout: time.Second, Lease: time.Minute})
	if err != nil {
		b.Fatal(err)
	}
	f, err := NewFallback(shared, local, FallbackSettings{RetryInterval: time.Second, StartLocal: true}, slog.New(slog.DiscardHandler))
	if err != nil {
		b.Fatal(err)
	}
	now := time.Now()
	f.now = func() time.Time { return now }
	f.probeAt = now.Add(time.Second)
	return f
}

func BenchmarkLimiter(b *testing.B) {
	for _, tc := range []struct {
		name     string
		clients  int
		parallel bool
	}{
		{"serial/hot_client", 1, false},
		{"serial/many_clients", 256, false},
		{"parallel/hot_client", 1, true},
		{"parallel/many_clients", 256, true},
	} {
		for _, fallback := range []bool{false, true} {
			name := "local"
			if fallback {
				name = "fallback_local"
			}
			b.Run(tc.name+"/"+name, func(b *testing.B) {
				local, ids := benchmarkLocal(b, tc.clients)
				var limiter benchmarkLimiter = local
				if fallback {
					limiter = benchmarkFallback(b, local, ids)
				}
				ctx := context.Background()
				b.ReportAllocs()
				b.ResetTimer()
				if tc.parallel {
					var worker atomic.Uint64
					b.RunParallel(func(pb *testing.PB) {
						// Workers cycle over all clients with different starts;
						// there is no per-call shared counter in the fixture.
						i := int(worker.Add(1)-1) % len(ids)
						for pb.Next() {
							release, err := limiter.Acquire(ctx, ids[i])
							if err != nil {
								b.Error(err)
								return
							}
							release()
							i = (i + 1) % len(ids)
						}
					})
				} else {
					for i := 0; i < b.N; i++ {
						release, err := limiter.Acquire(ctx, ids[i%len(ids)])
						if err != nil {
							b.Fatal(err)
						}
						release()
					}
				}
			})
		}
	}
}
