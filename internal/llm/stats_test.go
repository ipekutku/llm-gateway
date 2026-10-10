package llm

import (
	"context"
	"sync"
	"testing"
)

func TestStatsTravelsInContext(t *testing.T) {
	ctx, s := WithStats(context.Background())
	child, cancel := context.WithCancel(ctx)
	defer cancel()

	if StatsFrom(child) != s {
		t.Fatal("StatsFrom() did not return the Stats of a parent context")
	}
	StatsFrom(child).AddRetry()
	StatsFrom(child).SetFallback()
	if s.Retries() != 1 || !s.Fallback() {
		t.Errorf("Retries() = %d, Fallback() = %v; want 1, true", s.Retries(), s.Fallback())
	}
}

func TestStatsWithoutContextStatsIsNoOp(t *testing.T) {
	s := StatsFrom(context.Background())
	if s != nil {
		t.Fatalf("StatsFrom() = %v, want nil", s)
	}
	s.AddRetry()
	s.SetFallback()
	if s.Retries() != 0 || s.Fallback() {
		t.Errorf("nil Stats reports Retries() = %d, Fallback() = %v", s.Retries(), s.Fallback())
	}
}

func TestStatsConcurrentUpdates(t *testing.T) {
	_, s := WithStats(context.Background())
	var wg sync.WaitGroup
	for range 50 {
		wg.Go(func() {
			s.AddRetry()
			s.SetFallback()
			_ = s.Retries()
		})
	}
	wg.Wait()
	if s.Retries() != 50 || !s.Fallback() {
		t.Errorf("Retries() = %d, Fallback() = %v; want 50, true", s.Retries(), s.Fallback())
	}
}
