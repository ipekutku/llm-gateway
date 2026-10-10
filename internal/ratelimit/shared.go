package ratelimit

import (
	"context"
	"errors"
	"fmt"
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
}

// Shared enforces each client's request rate in Redis, so every gateway
// instance using the same Redis and key prefix shares one bucket per
// client. It applies the same token bucket as Limiter, in the same GCRA
// form, and is safe for concurrent use.
//
// Shared does not enforce MaxConcurrent yet; it is validated but not
// applied.
//
// Instances must be configured with the same limits: each sends its own
// limits with every call, and Redis applies whichever it receives.
type Shared struct {
	store   redis.Scripter
	timeout time.Duration
	// clients is built by NewShared and never modified afterwards.
	clients map[string]sharedClient
}

type sharedClient struct {
	rateKey   string
	interval  int64 // microseconds to earn one request
	tolerance int64 // (Burst-1) intervals, in microseconds
}

// NewShared returns a Shared limiter for the clients in limits, keyed by
// client ID, storing state through store, normally a *redis.Client. It
// does not contact Redis. A client without state in Redis has a full
// bucket.
func NewShared(store redis.Scripter, limits map[string]Limits, opts SharedOptions) (*Shared, error) {
	switch {
	case store == nil:
		return nil, errors.New("ratelimit: nil store")
	case opts.Timeout <= 0:
		return nil, errors.New("ratelimit: timeout must be positive")
	}
	if err := validate(limits); err != nil {
		return nil, err
	}
	prefix := opts.KeyPrefix
	if prefix == "" {
		prefix = DefaultKeyPrefix
	}
	s := &Shared{store: store, timeout: opts.Timeout, clients: make(map[string]sharedClient, len(limits))}
	for id, lim := range limits {
		// Truncated to whole microseconds, the resolution of Redis TIME: a
		// rate that does not divide a minute evenly is exceeded by under
		// 1µs per request.
		interval := (time.Minute / time.Duration(lim.RequestsPerMinute)).Microseconds()
		s.clients[id] = sharedClient{
			// The braces are a Redis Cluster hash tag: every key of one
			// client maps to the same slot, so one script can use them all.
			rateKey:   prefix + "{" + id + "}:rate",
			interval:  interval,
			tolerance: int64(lim.Burst-1) * interval,
		}
	}
	return s, nil
}

// rateScript applies one request to a client's bucket atomically. It keeps
// the bucket's theoretical arrival time (tat) in microseconds, as Limiter
// does in memory, and returns {1, 0} if the request is admitted or
// {0, retry after in microseconds} if not. A rejection writes nothing.
//
// The clock is Redis TIME, so instances whose clocks differ still agree.
// Lua numbers are doubles, which hold integers exactly up to 2^53; Unix
// time in microseconds stays far below that. Numbers are formatted with
// %.0f because Lua's default formatting would write them in exponent
// notation. The key expires when the bucket would be full again, so idle
// and removed clients leave nothing behind.
var rateScript = redis.NewScript(`
local time = redis.call('TIME')
local now = tonumber(time[1]) * 1000000 + tonumber(time[2])
local interval = tonumber(ARGV[1])
local tolerance = tonumber(ARGV[2])
local tat = tonumber(redis.call('GET', KEYS[1]))
if tat == nil or tat < now then
  tat = now
end
local allow_at = tat - tolerance
if now < allow_at then
  return {0, allow_at - now}
end
tat = tat + interval
redis.call('SET', KEYS[1], string.format('%.0f', tat), 'PX', string.format('%.0f', math.ceil((tat - now) / 1000)))
return {1, 0}
`)

// Acquire admits one request for clientID, or rejects it with an *Error if
// its request rate is exceeded. A rejected request consumes nothing. The
// returned release function does nothing yet, but callers must call it
// once when the request has finished, as with Limiter.
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
	ctx, cancel := context.WithTimeout(ctx, s.timeout)
	defer cancel()

	res, err := rateScript.Run(ctx, s.store, []string{c.rateKey}, c.interval, c.tolerance).Int64Slice()
	if err == nil && len(res) != 2 {
		err = fmt.Errorf("unexpected reply of %d values", len(res))
	}
	if err != nil {
		// The client may report a timeout as a network error; the context
		// says whether the deadline or a cancellation ended the call.
		if cerr := ctx.Err(); cerr != nil && !errors.Is(err, cerr) {
			err = fmt.Errorf("%w: %w", cerr, err)
		}
		return nil, fmt.Errorf("ratelimit: client %s: shared limits: %w", clientID, err)
	}
	if res[0] == 0 {
		return nil, &Error{ClientID: clientID, Limit: RequestRate, RetryAfter: time.Duration(res[1]) * time.Microsecond}
	}
	return func() {}, nil
}
