// Package ratelimit limits how much of the gateway each client can use.
//
// Every client has two independent limits: a request rate, enforced as a
// token bucket, and a cap on concurrent requests. A request that exceeds
// either is rejected at once; the limiter never queues or waits.
//
// A Limiter keeps its state per process for a fixed set of clients known
// at construction, so memory does not grow with traffic. Several gateway
// instances each enforce their own limits. A Shared limiter keeps the
// request rate in Redis instead, so instances share it.
package ratelimit

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"
)

// ErrLimitExceeded reports that a request was rejected by a client's
// limits. Rejections are *Error values that match it with errors.Is.
var ErrLimitExceeded = errors.New("rate limit exceeded")

// ErrUnknownClient reports a client that has no limits configured.
var ErrUnknownClient = errors.New("unknown client")

// Limit names the limit that rejected a request.
type Limit string

const (
	// RequestRate is the RequestsPerMinute and Burst limit.
	RequestRate Limit = "request_rate"
	// ConcurrentRequests is the MaxConcurrent limit.
	ConcurrentRequests Limit = "concurrent_requests"
)

// MaxLimit bounds every value in Limits. It keeps the rate arithmetic far
// from overflow and is far above any realistic per-client limit.
const MaxLimit = 1_000_000

// Limits are one client's limits. All values must be from 1 to MaxLimit.
type Limits struct {
	// RequestsPerMinute is the sustained request rate.
	RequestsPerMinute int
	// Burst is how many requests may arrive at once after a quiet period:
	// the token bucket's capacity.
	Burst int
	// MaxConcurrent is how many of the client's requests may be in
	// progress at the same time.
	MaxConcurrent int
}

// Error reports a rejected request.
type Error struct {
	ClientID string
	Limit    Limit
	// RetryAfter is how long until the request rate would admit a request.
	// It is 0 for ConcurrentRequests, which frees up when another request
	// finishes rather than after a known time.
	RetryAfter time.Duration
}

func (e *Error) Error() string {
	return fmt.Sprintf("client %s: %s: %s", e.ClientID, ErrLimitExceeded, e.Limit)
}

// Is reports whether target is ErrLimitExceeded.
func (e *Error) Is(target error) bool {
	return target == ErrLimitExceeded
}

// Limiter enforces per-client limits. It is safe for concurrent use.
type Limiter struct {
	// clients is built by New and never modified afterwards; each client's
	// state has its own lock, so clients do not contend with each other.
	clients map[string]*client
	now     func() time.Time // replaced in tests
}

// client is one client's state. The token bucket is kept in its
// equivalent GCRA form: instead of a token count refilled over time, it
// stores tat, the time at which the bucket would be full again. Each
// admitted request moves tat one interval later, and a request is admitted
// while tat is at most Burst-1 intervals in the future. This needs only
// integer time arithmetic, so refills and RetryAfter are exact.
type client struct {
	limits    Limits
	interval  time.Duration // time to earn one request
	tolerance time.Duration // (Burst-1) intervals

	mu     sync.Mutex
	tat    time.Time // theoretical arrival time; zero means a full bucket
	active int       // requests in progress
}

// New returns a Limiter for the clients in limits, keyed by client ID.
// Every bucket starts full.
func New(limits map[string]Limits) (*Limiter, error) {
	if err := validate(limits); err != nil {
		return nil, err
	}
	l := &Limiter{clients: make(map[string]*client, len(limits)), now: time.Now}
	for id, lim := range limits {
		// Truncated to whole nanoseconds: a rate that does not divide a
		// minute evenly is exceeded by under 1ns per request.
		interval := time.Minute / time.Duration(lim.RequestsPerMinute)
		l.clients[id] = &client{limits: lim, interval: interval, tolerance: time.Duration(lim.Burst-1) * interval}
	}
	return l, nil
}

// validate checks the limits given to New and NewShared.
func validate(limits map[string]Limits) error {
	if len(limits) == 0 {
		return errors.New("ratelimit: no clients")
	}
	for id, lim := range limits {
		switch {
		case strings.TrimSpace(id) == "":
			return errors.New("ratelimit: blank client ID")
		case !inRange(lim.RequestsPerMinute):
			return fmt.Errorf("ratelimit: client %s: requests per minute must be from 1 to %d", id, MaxLimit)
		case !inRange(lim.Burst):
			return fmt.Errorf("ratelimit: client %s: burst must be from 1 to %d", id, MaxLimit)
		case !inRange(lim.MaxConcurrent):
			return fmt.Errorf("ratelimit: client %s: max concurrent must be from 1 to %d", id, MaxLimit)
		}
	}
	return nil
}

// Acquire admits one request for clientID, or rejects it with an *Error if
// either limit is exceeded. A rejected request consumes nothing.
//
// On success the caller must call release exactly once when the request
// has finished, to free its concurrency slot. Further calls do nothing.
// An unconfigured client returns an error wrapping ErrUnknownClient.
//
// The context is unused: the limiter keeps its state in memory and never
// waits. It is accepted so the limiter satisfies interfaces whose
// implementations may do I/O.
func (l *Limiter) Acquire(_ context.Context, clientID string) (release func(), err error) {
	c, ok := l.clients[clientID]
	if !ok {
		return nil, fmt.Errorf("ratelimit: client %s: %w", clientID, ErrUnknownClient)
	}

	c.mu.Lock()
	defer c.mu.Unlock()
	now := l.now()
	tat := c.tat
	if tat.Before(now) {
		tat = now
	}
	allowAt := tat.Add(-c.tolerance)
	switch {
	case c.active >= c.limits.MaxConcurrent:
		return nil, &Error{ClientID: clientID, Limit: ConcurrentRequests}
	case now.Before(allowAt):
		return nil, &Error{ClientID: clientID, Limit: RequestRate, RetryAfter: allowAt.Sub(now)}
	}
	c.tat = tat.Add(c.interval)
	c.active++

	var once sync.Once
	return func() {
		once.Do(func() {
			c.mu.Lock()
			c.active--
			c.mu.Unlock()
		})
	}, nil
}

func inRange(n int) bool {
	return 1 <= n && n <= MaxLimit
}
