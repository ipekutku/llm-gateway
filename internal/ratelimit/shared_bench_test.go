package ratelimit

import (
	"context"
	"testing"
	"time"
)

func BenchmarkShared(b *testing.B) {
	client, prefix := testRedis(b)
	s, err := NewShared(client, map[string]Limits{
		"bench": {RequestsPerMinute: MaxLimit, Burst: MaxLimit, MaxConcurrent: MaxLimit},
	}, SharedOptions{
		KeyPrefix: prefix, Timeout: 250 * time.Millisecond, Lease: 3 * time.Minute,
		OnCall: func(_ string, _ time.Duration, err error) {
			if err != nil {
				b.Error("Redis call failed")
			}
		},
	})
	if err != nil {
		b.Fatal(err)
	}
	ctx := context.Background()
	// Warm the pool and both Lua scripts before timing the two round trips.
	release, err := s.Acquire(ctx, "bench")
	if err != nil {
		b.Fatal("Redis admission failed")
	}
	release()
	b.ReportAllocs()
	for b.Loop() {
		release, err := s.Acquire(ctx, "bench")
		if err != nil {
			b.Fatal("Redis admission failed")
		}
		release()
	}
}
