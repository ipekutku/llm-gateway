package ratelimit

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/redis/go-redis/v9"
)

// DefaultKeyPrefix starts every key a Shared limiter writes, unless
// SharedOptions.KeyPrefix replaces it.
const DefaultKeyPrefix = "llm-gateway:ratelimit:"

// SharedOptions configures a Shared limiter.
type SharedOptions struct {
	// KeyPrefix starts every key. Empty means DefaultKeyPrefix. Instances
	// that share limits must use the same prefix.
	KeyPrefix string
	// Timeout bounds each call to Redis, within the request context's own
	// deadline. It must be positive.
	Timeout time.Duration
	// Lease is how long an admitted request holds its concurrency slot if
	// it is never released, for example because its instance crashed. It
	// must be positive, and longer than any request can run; otherwise a
	// slot is freed while its request is still in progress.
	Lease time.Duration
}

// Shared enforces each client's request rate and concurrency in Redis, so
// every gateway instance using the same Redis and key prefix shares one
// bucket and one set of concurrency slots per client. It applies the same
// limits as Limiter and is safe for concurrent use.
//
// Instances must be configured with the same limits: each sends its own
// limits with every call, and Redis applies whichever it receives.
type Shared struct {
	store   redis.Scripter
	timeout time.Duration
	lease   int64 // microseconds
	// clients is built by NewShared and never modified afterwards.
	clients map[string]sharedClient
}

type sharedClient struct {
	rateKey       string
	activeKey     string
	interval      int64 // microseconds to earn one request
	tolerance     int64 // (Burst-1) intervals, in microseconds
	maxConcurrent int
}

// NewShared returns a Shared limiter for the clients in limits, keyed by
// client ID, storing state through store, normally a *redis.Client. It
// does not contact Redis. A client without state in Redis has a full
// bucket and no requests in progress.
func NewShared(store redis.Scripter, limits map[string]Limits, opts SharedOptions) (*Shared, error) {
	switch {
	case store == nil:
		return nil, errors.New("ratelimit: nil store")
	case opts.Timeout <= 0:
		return nil, errors.New("ratelimit: timeout must be positive")
	case opts.Lease < time.Millisecond:
		return nil, errors.New("ratelimit: lease must be at least 1ms")
	}
	if err := validate(limits); err != nil {
		return nil, err
	}
	prefix := opts.KeyPrefix
	if prefix == "" {
		prefix = DefaultKeyPrefix
	}
	s := &Shared{store: store, timeout: opts.Timeout, lease: opts.Lease.Microseconds(), clients: make(map[string]sharedClient, len(limits))}
	for id, lim := range limits {
		// Truncated to whole microseconds, the resolution of Redis TIME: a
		// rate that does not divide a minute evenly is exceeded by under
		// 1µs per request.
		interval := (time.Minute / time.Duration(lim.RequestsPerMinute)).Microseconds()
		// The braces are a Redis Cluster hash tag: every key of one client
		// maps to the same slot, so one script can use them all.
		key := prefix + "{" + id + "}:"
		s.clients[id] = sharedClient{
			rateKey:       key + "rate",
			activeKey:     key + "active",
			interval:      interval,
			tolerance:     int64(lim.Burst-1) * interval,
			maxConcurrent: lim.MaxConcurrent,
		}
	}
	return s, nil
}

// Replies of acquireScript.
const (
	admitted           = 0
	rejectedRate       = 1
	rejectedConcurrent = 2
)

// acquireScript admits one request atomically under both limits, checked
// in the same order as Limiter: concurrency, then the request rate. It
// returns {admitted, 0}, {rejectedConcurrent, 0}, or {rejectedRate, retry
// after in microseconds}. A rejection changes nothing except removing
// expired leases.
//
// KEYS[1] holds the bucket's theoretical arrival time (tat) in
// microseconds, as Limiter keeps it in memory. KEYS[2] is a sorted set of
// the leases of requests in progress, each scored by its expiry time.
//
// The clock is Redis TIME, so instances whose clocks differ still agree.
// Lua numbers are doubles, which hold integers exactly up to 2^53; Unix
// time in microseconds stays far below that. Numbers are formatted with
// %.0f because Lua's default formatting would write them in exponent
// notation. Both keys expire once they would be empty or full again, so
// idle and removed clients leave nothing behind.
var acquireScript = redis.NewScript(`
local time = redis.call('TIME')
local now = tonumber(time[1]) * 1000000 + tonumber(time[2])
local interval = tonumber(ARGV[1])
local tolerance = tonumber(ARGV[2])
local max_concurrent = tonumber(ARGV[3])
local lease = tonumber(ARGV[4])

redis.call('ZREMRANGEBYSCORE', KEYS[2], '-inf', string.format('%.0f', now))
if redis.call('ZCARD', KEYS[2]) >= max_concurrent then
  return {2, 0}
end

local tat = tonumber(redis.call('GET', KEYS[1]))
if tat == nil or tat < now then
  tat = now
end
local allow_at = tat - tolerance
if now < allow_at then
  return {1, allow_at - now}
end

tat = tat + interval
redis.call('SET', KEYS[1], string.format('%.0f', tat), 'PX', string.format('%.0f', math.ceil((tat - now) / 1000)))
redis.call('ZADD', KEYS[2], string.format('%.0f', now + lease), ARGV[5])
redis.call('PEXPIRE', KEYS[2], string.format('%.0f', math.ceil(lease / 1000)))
return {0, 0}
`)

// releaseScript removes one lease. A script rather than a plain ZREM keeps
// the store interface to running scripts.
var releaseScript = redis.NewScript(`return redis.call('ZREM', KEYS[1], ARGV[1])`)

// Acquire admits one request for clientID, or rejects it with an *Error if
// either limit is exceeded. A rejected request consumes nothing.
//
// On success the caller must call release exactly once when the request
// has finished, to free its concurrency slot. Further calls do nothing.
// Release removes the slot in Redis within the timeout, even if ctx has
// been canceled by then; if that fails, the slot stays held until its
// lease expires.
//
// An unconfigured client returns an error wrapping ErrUnknownClient
// without contacting Redis. If Redis fails or does not answer within the
// timeout, Acquire returns an error that is not an *Error; when the
// context ended first, it wraps the context's error.
func (s *Shared) Acquire(ctx context.Context, clientID string) (release func(), err error) {
	c, ok := s.clients[clientID]
	if !ok {
		return nil, fmt.Errorf("ratelimit: client %s: %w", clientID, ErrUnknownClient)
	}
	callCtx, cancel := context.WithTimeout(ctx, s.timeout)
	defer cancel()

	token := rand.Text()
	res, err := acquireScript.Run(callCtx, s.store, []string{c.rateKey, c.activeKey},
		c.interval, c.tolerance, c.maxConcurrent, s.lease, token).Int64Slice()
	if err == nil && len(res) != 2 {
		err = fmt.Errorf("unexpected reply of %d values", len(res))
	}
	if err != nil {
		return nil, fmt.Errorf("ratelimit: client %s: shared limits: %w", clientID, contextCause(callCtx, err))
	}
	switch res[0] {
	case admitted:
	case rejectedConcurrent:
		return nil, &Error{ClientID: clientID, Limit: ConcurrentRequests}
	case rejectedRate:
		return nil, &Error{ClientID: clientID, Limit: RequestRate, RetryAfter: time.Duration(res[1]) * time.Microsecond}
	default:
		return nil, fmt.Errorf("ratelimit: client %s: shared limits: unexpected reply %d", clientID, res[0])
	}

	// The request may have been canceled by the time it is released, but
	// its slot must still be freed.
	releaseCtx := context.WithoutCancel(ctx)
	var once sync.Once
	return func() {
		once.Do(func() {
			ctx, cancel := context.WithTimeout(releaseCtx, s.timeout)
			defer cancel()
			_ = releaseScript.Run(ctx, s.store, []string{c.activeKey}, token).Err()
		})
	}, nil
}

// contextCause adds ctx's error to err if ctx has ended and err does not
// already wrap it. The client may report a timeout as a network error; the
// context says whether the deadline or a cancellation ended the call.
func contextCause(ctx context.Context, err error) error {
	if cerr := ctx.Err(); cerr != nil && !errors.Is(err, cerr) {
		return fmt.Errorf("%w: %w", cerr, err)
	}
	return err
}
