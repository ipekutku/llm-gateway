package main

import (
	"context"
	"crypto/rand"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/ipekutku/llm-gateway/internal/ratelimit"
	"github.com/redis/go-redis/v9"
)

const (
	testRedisURLVar = "GATEWAY_TEST_REDIS_URL"
	// redisPassword marks a Redis password that must never be logged or
	// returned in an error.
	redisPassword = "synthetic-redis-password"
)

// testRedis returns the options of the Redis named by
// GATEWAY_TEST_REDIS_URL and a key prefix of the test's own, whose keys are
// deleted when the test ends. Without the variable the test is skipped,
// except in CI, where a missing Redis must not pass silently.
func testRedis(t *testing.T) (*redis.Options, string) {
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
	prefix := "llm-gateway-test:" + rand.Text() + ":"
	t.Cleanup(func() {
		client := redis.NewClient(opts)
		defer client.Close()
		ctx, cancel := context.WithTimeout(context.Background(), guard)
		defer cancel()
		iter := client.Scan(ctx, 0, prefix+"*", 100).Iterator()
		var keys []string
		for iter.Next(ctx) {
			keys = append(keys, iter.Val())
		}
		err := iter.Err()
		if err == nil && len(keys) > 0 {
			err = client.Del(ctx, keys...).Err()
		}
		if err != nil {
			t.Errorf("delete test keys: %v", err)
		}
	})
	return opts, prefix
}

func TestLoadRedis(t *testing.T) {
	opts, err := loadRedis(env(nil))
	if err != nil || opts != nil {
		t.Errorf("absent: %v, %v; want nil", opts, err)
	}

	opts, err = loadRedis(env(map[string]string{redisURLVar: "redis://:" + redisPassword + "@redis.internal:6380/2"}))
	if err != nil {
		t.Fatalf("redis URL: %v", err)
	}
	if opts.Addr != "redis.internal:6380" || opts.Password != redisPassword || opts.DB != 2 || opts.TLSConfig != nil {
		t.Errorf("options = addr %s, db %d, TLS %v", opts.Addr, opts.DB, opts.TLSConfig != nil)
	}

	opts, err = loadRedis(env(map[string]string{redisURLVar: "rediss://redis.internal:6379"}))
	if err != nil || opts.TLSConfig == nil {
		t.Errorf("rediss URL: TLS %v, %v; want TLS", opts != nil && opts.TLSConfig != nil, err)
	}
}

func TestLoadRedisErrorsDoNotRevealTheURL(t *testing.T) {
	for name, url := range map[string]string{
		"other scheme":   "http://user:" + redisPassword + "@redis.internal:6379",
		"invalid DB":     "redis://:" + redisPassword + "@redis.internal:6379/" + redisPassword,
		"invalid option": "redis://:" + redisPassword + "@redis.internal:6379/0?dial_timeout=" + redisPassword,
	} {
		t.Run(name, func(t *testing.T) {
			_, err := load(map[string]string{"OPENAI_MODEL": "gpt-4o", "OPENAI_API_KEY": openaiKey, redisURLVar: url})
			if err == nil || !strings.Contains(err.Error(), redisURLVar) {
				t.Fatalf("error = %v, want one naming %s", err, redisURLVar)
			}
			if strings.Contains(err.Error(), redisPassword) || strings.Contains(err.Error(), "redis.internal") {
				t.Errorf("error reveals the URL: %v", err)
			}
		})
	}
}

func TestRedisClientOptionsOverrideTheURL(t *testing.T) {
	parsed, err := redis.ParseURL("redis://127.0.0.1:6379/0?max_retries=5")
	if err != nil {
		t.Fatal(err)
	}
	got := redisClientOptions(parsed)
	if !got.ContextTimeoutEnabled || got.MaxRetries != -1 || got.DialerRetries != 1 {
		t.Errorf("options = context timeouts %v, max retries %d, dial retries %d", got.ContextTimeoutEnabled, got.MaxRetries, got.DialerRetries)
	}
	if parsed.MaxRetries != 5 {
		t.Error("redisClientOptions modified the parsed options")
	}
}

func TestNewLimiterWithoutRedisIsLocal(t *testing.T) {
	limiter, closeLimiter, err := newLimiter(t.Context(), config{RateLimits: testRateLimits, UpstreamTimeout: defaultUpstreamTimeout}, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	defer closeLimiter()
	if _, ok := limiter.(*ratelimit.Limiter); !ok {
		t.Errorf("limiter = %T, want *ratelimit.Limiter", limiter)
	}
}

func TestNewLimiterStartsLocalWhenRedisIsUnreachable(t *testing.T) {
	// Nothing listens on port 1.
	cfg := config{RateLimits: testRateLimits, UpstreamTimeout: defaultUpstreamTimeout, Redis: &redis.Options{Addr: "127.0.0.1:1", Password: redisPassword}}
	var logs syncBuffer
	logger := slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug}))

	limiter, closeLimiter, err := newLimiter(t.Context(), cfg, logger)
	if err != nil {
		t.Fatalf("newLimiter: %v", err)
	}
	defer closeLimiter()
	f, ok := limiter.(*ratelimit.Fallback)
	if !ok {
		t.Fatalf("limiter = %T, want *ratelimit.Fallback", limiter)
	}
	if f.Mode() != ratelimit.LocalMode {
		t.Errorf("mode = %s, want local", f.Mode())
	}
	// Requests are still limited, locally.
	release, err := limiter.Acquire(t.Context(), "team-a")
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	release()

	if !strings.Contains(logs.String(), "level=WARN msg=\"rate limit store unreachable at startup, using local limits\"") {
		t.Errorf("no startup warning:\n%s", logs.String())
	}
	// go-redis's own lines go to the gateway's logger at debug level.
	if !strings.Contains(logs.String(), "level=DEBUG msg=\"redis client\"") {
		t.Errorf("go-redis log lines not routed to the logger:\n%s", logs.String())
	}
	if strings.Contains(logs.String(), redisPassword) {
		t.Errorf("log reveals the Redis password:\n%s", logs.String())
	}
}

func TestRequestPathSharedRateLimits(t *testing.T) {
	opts, prefix := testRedis(t)
	oa := newUpstream(t, "/v1/chat/completions", reply(http.StatusOK, openaiReply))
	an := newUpstream(t, "/v1/messages", reply(http.StatusOK, anthropicReply))
	cfg := gatewayConfig(oa, an)
	cfg.Redis, cfg.RedisKeyPrefix = opts, prefix
	cfg.RateLimits = map[string]ratelimit.Limits{"team-a": {RequestsPerMinute: 1, Burst: 1, MaxConcurrent: 5}}

	limiter, closeLimiter, err := newLimiter(t.Context(), cfg, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatalf("newLimiter: %v", err)
	}
	defer closeLimiter()
	if f, ok := limiter.(*ratelimit.Fallback); !ok || f.Mode() != ratelimit.SharedMode {
		t.Fatalf("limiter = %T, want a Fallback in shared mode", limiter)
	}
	h, err := newHandlerWithLimiter(cfg, nil, limiter, discardRecorder{}, nil, nil, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatalf("newHandlerWithLimiter: %v", err)
	}
	gw := httptest.NewServer(h)
	defer gw.Close()

	resp, _, err := postChat(t, t.Context(), gw.URL, chatBody("gpt-4o"))
	if err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("first request: %v, %v; want 200", resp, err)
	}
	resp, data, err := postChat(t, t.Context(), gw.URL, chatBody("gpt-4o"))
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusTooManyRequests || errorCode(t, data) != "rate_limit_exceeded" || resp.Header.Get("Retry-After") != "60" {
		t.Errorf("second request: %d %s, Retry-After %q; want 429 rate_limit_exceeded, 60", resp.StatusCode, data, resp.Header.Get("Retry-After"))
	}

	// The bucket is in Redis, and the finished request released its slot
	// there.
	client := redis.NewClient(opts)
	defer client.Close()
	if n, err := client.Exists(t.Context(), prefix+"{team-a}:rate").Result(); err != nil || n != 1 {
		t.Errorf("rate key exists = %d, %v; want 1", n, err)
	}
	if n, err := client.Exists(t.Context(), prefix+"{team-a}:active").Result(); err != nil || n != 0 {
		t.Errorf("lease set exists = %d, %v; want 0 after release", n, err)
	}
}

func TestRunWithUnreachableRedis(t *testing.T) {
	var logs syncBuffer
	ctx, cancel := context.WithCancel(context.Background())
	result := make(chan error, 1)
	go func() {
		result <- runWithFakeDatabase(ctx, map[string]string{
			"ANTHROPIC_MODEL": "claude-opus-5-5", "ANTHROPIC_API_KEY": anthropicKey,
			"GATEWAY_ADDR": "127.0.0.1:0", "GATEWAY_METRICS_ADDR": "127.0.0.1:0", clientsFileVar: writeClientsFile(t),
			redisURLVar: "redis://:" + redisPassword + "@127.0.0.1:1/0",
		}, slog.New(slog.NewTextHandler(&logs, nil)))
	}()

	deadline := time.After(guard)
	for !strings.Contains(logs.String(), "gateway listening") {
		select {
		case err := <-result:
			t.Fatalf("run() returned early: %v", err)
		case <-deadline:
			t.Fatal("timed out waiting for the gateway to listen")
		case <-time.After(10 * time.Millisecond):
		}
	}
	cancel()
	select {
	case err := <-result:
		if err != nil {
			t.Errorf("run() error = %v, want nil", err)
		}
	case <-time.After(guard):
		t.Fatal("timed out waiting for run to return")
	}
	for _, want := range []string{"rate limit store unreachable at startup", "rate_limits=shared"} {
		if !strings.Contains(logs.String(), want) {
			t.Errorf("log does not contain %q:\n%s", want, logs.String())
		}
	}
	if strings.Contains(logs.String(), redisPassword) {
		t.Errorf("log reveals the Redis password:\n%s", logs.String())
	}
}
