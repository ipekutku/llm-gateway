package main

import (
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ipekutku/llm-gateway/internal/httpapi"
	"github.com/ipekutku/llm-gateway/internal/metrics"
	"github.com/ipekutku/llm-gateway/internal/ratelimit"
	"github.com/redis/go-redis/v9"
)

// redisProxy forwards TCP connections to Redis until it is cut, so a test
// can take Redis away from some gateway instances and bring it back.
type redisProxy struct {
	ln     net.Listener
	target string
	wg     sync.WaitGroup

	mu    sync.Mutex
	down  bool
	conns []net.Conn
}

func newRedisProxy(t *testing.T, target string) *redisProxy {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	p := &redisProxy{ln: ln, target: target}
	p.wg.Go(p.accept)
	t.Cleanup(func() {
		ln.Close()
		p.cut()
		p.wg.Wait()
	})
	return p
}

func (p *redisProxy) addr() string { return p.ln.Addr().String() }

func (p *redisProxy) accept() {
	for {
		conn, err := p.ln.Accept()
		if err != nil {
			return
		}
		p.mu.Lock()
		down := p.down
		p.mu.Unlock()
		if down {
			// Connections are accepted and dropped at once, like a Redis
			// that has gone away.
			conn.Close()
			continue
		}
		upstream, err := net.Dial("tcp", p.target)
		if err != nil {
			conn.Close()
			continue
		}
		p.mu.Lock()
		p.conns = append(p.conns, conn, upstream)
		p.mu.Unlock()
		p.wg.Go(func() { _, _ = io.Copy(upstream, conn); upstream.Close() })
		p.wg.Go(func() { _, _ = io.Copy(conn, upstream); conn.Close() })
	}
}

// cut closes every open connection and drops new ones until restore.
func (p *redisProxy) cut() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.down = true
	for _, c := range p.conns {
		c.Close()
	}
	p.conns = nil
}

func (p *redisProxy) restore() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.down = false
}

// instance is one complete gateway, as one replica of a deployment would
// run it: its own Redis client, limiter, metrics, and handler.
type instance struct {
	url     string
	limiter httpapi.Limiter
	m       *metrics.Metrics
	logs    *syncBuffer
	close   func() error
}

// newInstance starts a gateway for cfg whose Redis is at addr.
func newInstance(t *testing.T, cfg config, opts *redis.Options, addr string) *instance {
	t.Helper()
	o := *opts
	o.Addr = addr
	cfg.Redis = &o
	m, err := metrics.New(configuredModels(cfg))
	if err != nil {
		t.Fatal(err)
	}
	logs := new(syncBuffer)
	logger := slog.New(slog.NewTextHandler(logs, nil))
	limiter, closeLimiter, err := newLimiter(t.Context(), cfg, m, logger)
	if err != nil {
		t.Fatalf("newLimiter: %v", err)
	}
	h, err := newHandlerWithLimiter(cfg, nil, limiter, discardRecorder{}, m, nil, logger)
	if err != nil {
		t.Fatalf("newHandlerWithLimiter: %v", err)
	}
	gw := httptest.NewServer(h)
	t.Cleanup(func() {
		gw.Close()
		_ = closeLimiter()
	})
	return &instance{url: gw.URL, limiter: limiter, m: m, logs: logs, close: closeLimiter}
}

// multiInstanceConfig is the configuration of every instance in a test: the
// clients of withClients with limits, and Redis keys under prefix.
func multiInstanceConfig(oa, an *upstream, limits ratelimit.Limits, prefix string) config {
	cfg := gatewayConfig(oa, an)
	withClients(limits)(&cfg)
	cfg.RedisKeyPrefix = prefix
	return cfg
}

// wantStatus sends a chat request with key to gw and checks the response
// status and, for errors, its code.
func (gw *instance) wantStatus(t *testing.T, key string, status int, code string) {
	t.Helper()
	resp, data, err := postChatAs(t, t.Context(), gw.url, "Bearer "+key, chatBody("gpt-4o"))
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	if resp.StatusCode != status || (code != "" && errorCode(t, data) != code) {
		t.Fatalf("status = %d (%s), want %d %s", resp.StatusCode, data, status, code)
	}
}

func okUpstreams(t *testing.T) (*upstream, *upstream) {
	t.Helper()
	return newUpstream(t, "/v1/chat/completions", reply(http.StatusOK, openaiReply)),
		newUpstream(t, "/v1/messages", reply(http.StatusOK, anthropicReply))
}

func TestMultiInstanceSharedRateLimits(t *testing.T) {
	opts, prefix := testRedis(t)
	oa, an := okUpstreams(t)
	cfg := multiInstanceConfig(oa, an, ratelimit.Limits{RequestsPerMinute: 1, Burst: 2, MaxConcurrent: 10}, prefix)
	a := newInstance(t, cfg, opts, opts.Addr)
	b := newInstance(t, cfg, opts, opts.Addr)

	// team-a's burst of two is shared: one request on each instance uses
	// it up for both.
	a.wantStatus(t, clientKey, http.StatusOK, "")
	b.wantStatus(t, clientKey, http.StatusOK, "")
	a.wantStatus(t, clientKey, http.StatusTooManyRequests, "rate_limit_exceeded")
	b.wantStatus(t, clientKey, http.StatusTooManyRequests, "rate_limit_exceeded")

	// team-b has its own bucket, shared too.
	b.wantStatus(t, teamBKey, http.StatusOK, "")
	a.wantStatus(t, teamBKey, http.StatusOK, "")
	b.wantStatus(t, teamBKey, http.StatusTooManyRequests, "rate_limit_exceeded")
}

func TestMultiInstanceSharedConcurrencyLimits(t *testing.T) {
	opts, prefix := testRedis(t)
	entered := make(chan struct{}, 10)
	unblock := make(chan struct{})
	oa := newUpstream(t, "/v1/chat/completions", func(w http.ResponseWriter, r *http.Request) {
		entered <- struct{}{}
		<-unblock
		_, _ = io.WriteString(w, openaiReply)
	})
	an := newUpstream(t, "/v1/messages", reply(http.StatusOK, anthropicReply))
	cfg := multiInstanceConfig(oa, an, ratelimit.Limits{RequestsPerMinute: ratelimit.MaxLimit, Burst: ratelimit.MaxLimit, MaxConcurrent: 1}, prefix)
	a := newInstance(t, cfg, opts, opts.Addr)
	b := newInstance(t, cfg, opts, opts.Addr)

	// team-a's only slot is taken by a request in progress on instance a.
	done := make(chan int, 1)
	go func() {
		resp, _, err := postChatAs(t, t.Context(), a.url, "Bearer "+clientKey, chatBody("gpt-4o"))
		if err != nil {
			done <- 0
			return
		}
		done <- resp.StatusCode
	}()
	select {
	case <-entered:
	case <-time.After(guard):
		t.Fatal("the first request did not reach the upstream")
	}
	b.wantStatus(t, clientKey, http.StatusTooManyRequests, "concurrency_limit_exceeded")

	// Once it finishes, its slot is free on every instance.
	close(unblock)
	if status := <-done; status != http.StatusOK {
		t.Fatalf("first request status = %d, want 200", status)
	}
	b.wantStatus(t, clientKey, http.StatusOK, "")
}

func TestMultiInstanceRedisOutageAndRecovery(t *testing.T) {
	opts, prefix := testRedis(t)
	saved := redisRetryInterval
	redisRetryInterval = 100 * time.Millisecond
	t.Cleanup(func() { redisRetryInterval = saved })
	oa, an := okUpstreams(t)
	cfg := multiInstanceConfig(oa, an, ratelimit.Limits{RequestsPerMinute: 1, Burst: 1, MaxConcurrent: 10}, prefix)
	proxy := newRedisProxy(t, opts.Addr)
	a := newInstance(t, cfg, opts, proxy.addr())
	b := newInstance(t, cfg, opts, proxy.addr())

	// Shared: team-a's one request is used on a, so b rejects.
	a.wantStatus(t, clientKey, http.StatusOK, "")
	b.wantStatus(t, clientKey, http.StatusTooManyRequests, "rate_limit_exceeded")

	// Redis goes away. Requests keep being served, each instance applying
	// team-a's limits on its own: one request on each.
	proxy.cut()
	for _, gw := range []*instance{a, b} {
		gw.wantStatus(t, clientKey, http.StatusOK, "")
		gw.wantStatus(t, clientKey, http.StatusTooManyRequests, "rate_limit_exceeded")
		assertMetrics(t, gw.m, "gateway_ratelimit_store_up 0")
		if n := strings.Count(gw.logs.String(), "rate limit store unavailable, using local limits"); n != 1 {
			t.Errorf("%d outage warnings, want 1:\n%s", n, gw.logs.String())
		}
	}

	// Redis is back. After the retry interval, the next request on each
	// instance returns it to shared limits. team-b's request on a uses the
	// shared bucket, so b rejects the next one, which b's own limits, never
	// used for team-b, would have admitted.
	proxy.restore()
	time.Sleep(redisRetryInterval)
	a.wantStatus(t, teamBKey, http.StatusOK, "")
	b.wantStatus(t, teamBKey, http.StatusTooManyRequests, "rate_limit_exceeded")
	for _, gw := range []*instance{a, b} {
		assertMetrics(t, gw.m, "gateway_ratelimit_store_up 1")
		if !strings.Contains(gw.logs.String(), "rate limit store recovered, using shared limits") {
			t.Errorf("recovery not logged:\n%s", gw.logs.String())
		}
	}
}

func TestMultiInstanceCrashedInstanceSlotsExpire(t *testing.T) {
	opts, prefix := testRedis(t)
	const lease = 300 * time.Millisecond
	saved := redisLease
	redisLease = func(time.Duration) time.Duration { return lease }
	t.Cleanup(func() { redisLease = saved })
	oa, an := okUpstreams(t)
	cfg := multiInstanceConfig(oa, an, ratelimit.Limits{RequestsPerMinute: ratelimit.MaxLimit, Burst: ratelimit.MaxLimit, MaxConcurrent: 1}, prefix)
	a := newInstance(t, cfg, opts, opts.Addr)
	b := newInstance(t, cfg, opts, opts.Addr)

	// Instance a admits a request and crashes before releasing its slot.
	if _, err := a.limiter.Acquire(t.Context(), "team-a"); err != nil {
		t.Fatalf("Acquire on a: %v", err)
	}
	_ = a.close()
	b.wantStatus(t, clientKey, http.StatusTooManyRequests, "concurrency_limit_exceeded")

	// The slot frees itself when its lease expires.
	time.Sleep(lease)
	b.wantStatus(t, clientKey, http.StatusOK, "")
}
