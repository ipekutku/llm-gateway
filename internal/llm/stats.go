package llm

import (
	"context"
	"sync/atomic"
)

// Stats records the upstream activity of one request, so the layer that
// reports the request's outcome can include what the layers below it did.
// It travels in the request's context; see WithStats. It is safe for
// concurrent use, and its methods do nothing on a nil *Stats, so a layer can
// update the Stats of a context that has none.
type Stats struct {
	retries  atomic.Int64
	fallback atomic.Bool
}

type statsKey struct{}

// WithStats returns a copy of ctx carrying a new Stats, and that Stats.
func WithStats(ctx context.Context) (context.Context, *Stats) {
	s := new(Stats)
	return context.WithValue(ctx, statsKey{}, s), s
}

// StatsFrom returns the Stats carried by ctx, or nil if there is none.
func StatsFrom(ctx context.Context) *Stats {
	s, _ := ctx.Value(statsKey{}).(*Stats)
	return s
}

// AddRetry records that an upstream call was attempted again.
func (s *Stats) AddRetry() {
	if s != nil {
		s.retries.Add(1)
	}
}

// SetFallback records that the request was sent to a fallback provider.
func (s *Stats) SetFallback() {
	if s != nil {
		s.fallback.Store(true)
	}
}

// Retries returns the number of repeated upstream attempts, over all
// providers the request was sent to.
func (s *Stats) Retries() int {
	if s == nil {
		return 0
	}
	return int(s.retries.Load())
}

// Fallback reports whether the request was sent to a fallback provider.
func (s *Stats) Fallback() bool {
	return s != nil && s.fallback.Load()
}
