package main

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/ipekutku/llm-gateway/internal/httpapi"
	"github.com/ipekutku/llm-gateway/internal/ratelimit"
	"github.com/redis/go-redis/v9"
)

const (
	redisURLVar = "GATEWAY_REDIS_URL"

	// redisCallTimeout bounds each rate-limit call to Redis. A request
	// waits at most this long before the local limits decide it.
	redisCallTimeout = 250 * time.Millisecond

	// redisRetryInterval is how long local limits are used after a Redis
	// failure before Redis is tried again.
	redisRetryInterval = time.Second

	// redisStartupTimeout bounds the startup check that Redis answers.
	redisStartupTimeout = 2 * time.Second

	// leaseMargin is added to the longest a request can be read and served
	// upstream to give the lease of its concurrency slot: time to write the
	// response and finish the handler.
	leaseMargin = 30 * time.Second
)

// loadRedis reads GATEWAY_REDIS_URL. It returns nil if the variable is
// absent. Errors never include the URL, which may contain a password.
func loadRedis(getenv func(string) string) (*redis.Options, error) {
	v := strings.TrimSpace(getenv(redisURLVar))
	if v == "" {
		return nil, nil
	}
	if !strings.HasPrefix(v, "redis://") && !strings.HasPrefix(v, "rediss://") {
		return nil, fmt.Errorf("%s must be a redis:// or rediss:// URL", redisURLVar)
	}
	// go-redis's parse errors can quote parts of the URL.
	opts, err := redis.ParseURL(v)
	if err != nil {
		return nil, fmt.Errorf("%s is not a valid Redis URL", redisURLVar)
	}
	return opts, nil
}

// newLimiter returns the rate limiter for cfg and a function that closes
// what it opened. Without Redis it is the in-process limiter. With Redis
// it is a ratelimit.Fallback: limits shared through Redis while Redis
// answers, and the in-process limits while it does not. A Redis that does
// not answer at startup is logged and does not stop the gateway; the
// Fallback starts with local limits and probes Redis on the first request.
func newLimiter(ctx context.Context, cfg config, logger *slog.Logger) (httpapi.Limiter, func() error, error) {
	local, err := ratelimit.New(cfg.RateLimits)
	if err != nil {
		return nil, nil, err
	}
	if cfg.Redis == nil {
		return local, func() error { return nil }, nil
	}
	redis.SetLogger(redisLogger{logger})
	client := redis.NewClient(redisClientOptions(cfg.Redis))
	shared, err := ratelimit.NewShared(client, cfg.RateLimits, ratelimit.SharedOptions{
		KeyPrefix: cfg.RedisKeyPrefix,
		Timeout:   redisCallTimeout,
		Lease:     httpapi.BodyReadTimeout + cfg.UpstreamTimeout + leaseMargin,
	})
	if err != nil {
		client.Close()
		return nil, nil, err
	}

	pingCtx, cancel := context.WithTimeout(ctx, redisStartupTimeout)
	pingErr := client.Ping(pingCtx).Err()
	cancel()
	if pingErr != nil {
		logger.Warn("rate limit store unreachable at startup, using local limits", slog.String("error", pingErr.Error()))
	}
	f, err := ratelimit.NewFallback(shared, local, ratelimit.FallbackSettings{
		RetryInterval: redisRetryInterval,
		StartLocal:    pingErr != nil,
	}, logger)
	if err != nil {
		client.Close()
		return nil, nil, err
	}
	return f, client.Close, nil
}

// redisClientOptions returns a copy of opts, parsed from the URL, with the
// settings the limiter relies on. They replace any given in the URL.
func redisClientOptions(opts *redis.Options) *redis.Options {
	o := *opts
	// Deadlines from the context interrupt blocked reads and writes, so
	// a Redis that stops answering costs at most redisCallTimeout.
	o.ContextTimeoutEnabled = true
	// No command retries: a script whose reply was lost may have run, and
	// running it again would count the request twice. The Fallback deals
	// with failures instead.
	o.MaxRetries = -1
	// One dial attempt: the default retries would delay the fallback to
	// local limits.
	o.DialerRetries = 1
	return &o
}

// redisLogger sends go-redis's own log lines, such as failed dials, to
// the gateway's logger at debug level, instead of unstructured lines on
// stderr. The Fallback already logs Redis failures once per outage.
type redisLogger struct{ log *slog.Logger }

func (l redisLogger) Printf(ctx context.Context, format string, v ...any) {
	l.log.DebugContext(ctx, "redis client", slog.String("detail", fmt.Sprintf(format, v...)))
}
