// Command gateway runs the LLM gateway HTTP server.
//
// Configuration is read from the environment; see the README.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/ipekutku/llm-gateway/internal/auth"
	"github.com/ipekutku/llm-gateway/internal/breaker"
	"github.com/ipekutku/llm-gateway/internal/httpapi"
	"github.com/ipekutku/llm-gateway/internal/llm"
	"github.com/ipekutku/llm-gateway/internal/metrics"
	"github.com/ipekutku/llm-gateway/internal/postgres"
	"github.com/ipekutku/llm-gateway/internal/provider/anthropic"
	"github.com/ipekutku/llm-gateway/internal/provider/openai"
	"github.com/ipekutku/llm-gateway/internal/ratelimit"
	"github.com/ipekutku/llm-gateway/internal/retry"
	"github.com/ipekutku/llm-gateway/internal/routing"
	"github.com/ipekutku/llm-gateway/internal/tracing"
	"github.com/ipekutku/llm-gateway/internal/usage"
	"go.opentelemetry.io/otel/trace"
)

const (
	// readHeaderTimeout bounds how long a client may take to send request
	// headers.
	readHeaderTimeout = 5 * time.Second

	// serverIdleTimeout bounds how long a client's keep-alive connection
	// may stay open between requests.
	serverIdleTimeout = 2 * time.Minute

	// shutdownTimeout bounds graceful shutdown. In-flight requests still
	// running afterwards are cut off.
	shutdownTimeout = 5 * time.Second

	// metricsWriteTimeout bounds writing one metrics response.
	metricsWriteTimeout = 10 * time.Second

	// idleConnTimeout is how long an unused upstream connection stays in
	// the pool.
	idleConnTimeout = 90 * time.Second
)

func main() {
	logger, err := newLogger(os.Stderr, os.Getenv)
	if err != nil {
		slog.New(slog.NewTextHandler(os.Stderr, nil)).Error("gateway failed", slog.Any("error", fmt.Errorf("configuration: %w", err)))
		os.Exit(1)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if err := execute(ctx, os.Args[1:], os.Getenv, logger); err != nil {
		logger.Error("gateway failed", slog.Any("error", err))
		os.Exit(1)
	}
}

// run loads the configuration, starts the server, and serves until ctx is
// canceled.
func run(ctx context.Context, getenv func(string) string, logger *slog.Logger) error {
	return runWith(ctx, getenv, os.ReadFile, func(ctx context.Context, url string, tp trace.TracerProvider) (accountingStore, error) {
		return postgres.Open(ctx, url, tp)
	}, logger)
}

type accountingStore interface {
	usage.RecordStore
	CheckSchema(context.Context) error
	Close()
}

// runWith injects filesystem and database access for startup/lifecycle tests.
func runWith(ctx context.Context, getenv func(string) string, readFile func(string) ([]byte, error), openStore func(context.Context, string, trace.TracerProvider) (accountingStore, error), logger *slog.Logger) (result error) {
	cfg, err := loadConfig(getenv, readFile)
	if err != nil {
		return fmt.Errorf("configuration: %w", err)
	}
	warnPricingGaps(cfg, logger)
	// Deferred first, so it runs last: after the database pool closes, the
	// spans of the final usage writes are flushed.
	traces, stopTracing, err := startTracing(ctx, cfg.Tracing, logger)
	if err != nil {
		return err
	}
	defer func() { result = errors.Join(result, stopTracing()) }()
	startupCtx, cancel := context.WithTimeout(ctx, databaseStartupTimeout)
	store, err := openStore(startupCtx, cfg.DatabaseURL, traces)
	if err != nil {
		cancel()
		return fmt.Errorf("database: %w", err)
	}
	defer store.Close()
	err = store.CheckSchema(startupCtx)
	cancel()
	if err != nil {
		return fmt.Errorf("database schema: %w", err)
	}
	recorder, err := usage.NewRecorder(store, logger, usage.DefaultRecorderOptions())
	if err != nil {
		return err
	}
	var active *activeHandlers
	defer func() {
		drainCtx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
		defer cancel()
		if active != nil {
			result = errors.Join(result, active.stop(drainCtx))
		}
		closeErr := recorder.Close(drainCtx)
		result = errors.Join(result, closeErr)
		if closeErr == nil {
			logger.Info("usage recorder stopped")
		}
	}()
	m, err := metrics.New(configuredModels(cfg))
	if err != nil {
		return err
	}
	handler, err := newHandler(cfg, nil, recorder, m, traces, logger)
	if err != nil {
		return err
	}
	active = &activeHandlers{next: handler}

	ln, err := net.Listen("tcp", cfg.Addr)
	if err != nil {
		return fmt.Errorf("listen: %w", err)
	}
	metricsLn, err := net.Listen("tcp", cfg.MetricsAddr)
	if err != nil {
		ln.Close()
		return fmt.Errorf("metrics listen: %w", err)
	}
	attrs := []any{slog.String("addr", ln.Addr().String()), slog.String("metrics_addr", metricsLn.Addr().String())}
	if p := cfg.OpenAI; p != nil {
		attrs = append(attrs, slog.String("openai_model", p.Model))
		if p.FallbackTo != "" {
			attrs = append(attrs, slog.String("openai_fallback", p.FallbackTo))
		}
	}
	if p := cfg.Anthropic; p != nil {
		attrs = append(attrs, slog.String("anthropic_model", p.Model))
		if p.FallbackTo != "" {
			attrs = append(attrs, slog.String("anthropic_fallback", p.FallbackTo))
		}
	}
	attrs = append(attrs,
		slog.Duration("upstream_timeout", cfg.UpstreamTimeout),
		slog.Duration("upstream_connect_timeout", cfg.ConnectTimeout),
		slog.Int("retry_max_attempts", cfg.Retry.MaxAttempts),
		slog.Duration("retry_base_delay", cfg.Retry.BaseDelay),
		slog.Duration("retry_max_delay", cfg.Retry.MaxDelay),
		slog.Int("breaker_failures", cfg.Breaker.Failures),
		slog.Duration("breaker_cooldown", cfg.Breaker.Cooldown),
		slog.Bool("tracing", cfg.Tracing),
	)
	if (cfg.OpenAI != nil && cfg.OpenAI.FallbackTo != "") || (cfg.Anthropic != nil && cfg.Anthropic.FallbackTo != "") {
		attrs = append(attrs, slog.Duration("provider_timeout", cfg.ProviderTimeout))
	}
	disabled := 0
	for _, c := range cfg.Clients {
		if c.Disabled {
			disabled++
		}
	}
	attrs = append(attrs, slog.Int("clients", len(cfg.Clients)-disabled), slog.Int("disabled_clients", disabled))
	logger.Info("gateway listening", attrs...)

	// Both servers shut down when ctx is canceled, and either one failing
	// stops the other.
	ctx, stopServers := context.WithCancel(ctx)
	defer stopServers()
	metricsDone := make(chan error, 1)
	go func() {
		err := serve(ctx, newMetricsServer(m.Handler(logger), logger), metricsLn, shutdownTimeout)
		stopServers()
		metricsDone <- err
	}()
	err = serve(ctx, newServer(active, logger), ln, shutdownTimeout)
	stopServers()
	if metricsErr := <-metricsDone; metricsErr != nil {
		err = errors.Join(err, fmt.Errorf("metrics: %w", metricsErr))
	}
	logger.Info("HTTP servers stopped")
	return err
}

// configuredModels returns the configured model names, the only model
// label values of the metrics.
func configuredModels(cfg config) []string {
	var models []string
	for _, p := range []*providerConfig{cfg.OpenAI, cfg.Anthropic} {
		if p != nil {
			models = append(models, p.Model)
		}
	}
	return models
}

// newMetricsServer returns the server for the metrics listener. It serves
// GET /metrics without authentication, so its address must be reachable
// only by the metrics scraper.
func newMetricsServer(metricsHandler http.Handler, logger *slog.Logger) *http.Server {
	mux := http.NewServeMux()
	mux.Handle("GET /metrics", metricsHandler)
	return &http.Server{
		Handler:           mux,
		ReadHeaderTimeout: readHeaderTimeout,
		ReadTimeout:       readHeaderTimeout,
		WriteTimeout:      metricsWriteTimeout,
		IdleTimeout:       serverIdleTimeout,
		ErrorLog:          slog.NewLogLogger(logger.Handler(), slog.LevelWarn),
	}
}

// newServer returns the gateway's HTTP server. Request bodies are bounded
// by the handler; there is deliberately no ReadTimeout or WriteTimeout,
// because they would also cut off the long wait for the upstream.
func newServer(handler http.Handler, logger *slog.Logger) *http.Server {
	return &http.Server{
		Handler:           handler,
		ReadHeaderTimeout: readHeaderTimeout,
		IdleTimeout:       serverIdleTimeout,
		ErrorLog:          slog.NewLogLogger(logger.Handler(), slog.LevelWarn),
	}
}

// newHandler builds the provider clients, router, client authentication,
// rate limiter, and HTTP handler for cfg. Each provider client is wrapped
// with retries and, above them, a circuit breaker; the router's fallback
// sits above both. A nil httpClient uses a client built by
// newUpstreamClient, a nil m records no metrics, and a nil tp creates no
// spans.
func newHandler(cfg config, httpClient *http.Client, recorder httpapi.UsageRecorder, m *metrics.Metrics, tp trace.TracerProvider, logger *slog.Logger) (http.Handler, error) {
	if httpClient == nil {
		httpClient = newUpstreamClient(cfg)
	}
	t := tracing.New(tp)

	type enabled struct {
		cfg      *providerConfig
		provider llm.Provider
	}
	providers := make(map[string]enabled)
	if p := cfg.OpenAI; p != nil {
		c, err := openai.New(p.APIKey, p.BaseURL, httpClient)
		if err != nil {
			return nil, err
		}
		wrapped, err := resilient(openai.ProviderName, c, cfg, m, t, logger)
		if err != nil {
			return nil, err
		}
		providers[openai.ProviderName] = enabled{p, wrapped}
	}
	if p := cfg.Anthropic; p != nil {
		c, err := anthropic.New(p.APIKey, p.BaseURL, httpClient)
		if err != nil {
			return nil, err
		}
		wrapped, err := resilient(anthropic.ProviderName, c, cfg, m, t, logger)
		if err != nil {
			return nil, err
		}
		providers[anthropic.ProviderName] = enabled{p, wrapped}
	}

	// A fallback reuses the other provider's wrapped client, so each
	// provider has exactly one breaker whichever route reaches it.
	routes := make(map[string]routing.Route, len(providers))
	for name, e := range providers {
		route := routing.Route{Provider: e.provider}
		if to := e.cfg.FallbackTo; to != "" {
			f := providers[to]
			m.AddFallbackRoute(name, to)
			route.Fallback = &routing.Fallback{
				Model: f.cfg.Model, Provider: f.provider, PrimaryTimeout: cfg.ProviderTimeout,
				OnFallback: func() { m.ObserveFallback(name, to) },
			}
		}
		routes[e.cfg.Model] = route
	}

	router, err := routing.New(routes, logger)
	if err != nil {
		return nil, err
	}
	authenticator, err := auth.New(cfg.Clients)
	if err != nil {
		return nil, err
	}
	limiter, err := ratelimit.New(cfg.RateLimits)
	if err != nil {
		return nil, err
	}
	models := make(map[string]string, len(providers))
	for name, e := range providers {
		models[name] = e.cfg.Model
	}
	h, err := httpapi.New(t.Route(router), authenticator, limiter, cfg.UpstreamTimeout, httpapi.Accounting{Recorder: recorder, Pricing: cfg.Pricing, Models: models}, m, logger)
	if err != nil {
		return nil, err
	}
	return t.Handler(h), nil
}

// resilient wraps a provider client with the retry policy and, above it,
// a circuit breaker, so the breaker sees one outcome per request. The client
// itself is instrumented, so m counts every attempt and t traces each one;
// t's provider span covers the breaker and all attempts.
func resilient(name string, p llm.Provider, cfg config, m *metrics.Metrics, t *tracing.Tracer, logger *slog.Logger) (llm.Provider, error) {
	policy := cfg.Retry
	policy.OnRetry = func() { m.ObserveRetry(name) }
	r, err := retry.New(m.Instrument(name, t.Attempt(name, p)), policy, logger)
	if err != nil {
		return nil, err
	}
	settings := cfg.Breaker
	settings.OnStateChange = func(s breaker.State) { m.SetCircuitState(name, s) }
	b, err := breaker.New(name, r, settings, logger)
	if err != nil {
		return nil, err
	}
	return t.Provider(name, b), nil
}

// newUpstreamClient returns the HTTP client shared by all provider
// adapters. Its transport bounds connection setup; the per-request upstream
// timeout is applied through the request context by the handler.
func newUpstreamClient(cfg config) *http.Client {
	t := http.DefaultTransport.(*http.Transport).Clone()
	t.DialContext = (&net.Dialer{Timeout: cfg.ConnectTimeout, KeepAlive: 30 * time.Second}).DialContext
	t.TLSHandshakeTimeout = cfg.ConnectTimeout
	t.IdleConnTimeout = idleConnTimeout
	return &http.Client{
		Transport: t,
		// Provider destinations are fixed. Following a redirect could send
		// API keys (including Anthropic's X-Api-Key) and prompts elsewhere.
		// Return the 3xx to the adapter as an upstream failure instead.
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
}

// serve runs srv on ln until ctx is canceled, then shuts down gracefully.
// If in-flight requests have not finished within timeout, it closes the
// server, which cancels their contexts, and returns an error.
func serve(ctx context.Context, srv *http.Server, ln net.Listener, timeout time.Duration) error {
	// A serve failure must also cancel active handlers before accounting drains.
	defer func() { _ = srv.Close() }()
	serveErr := make(chan error, 1)
	go func() { serveErr <- srv.Serve(ln) }()

	select {
	case err := <-serveErr:
		return fmt.Errorf("serve: %w", err)
	case <-ctx.Done():
	}

	// ctx is already canceled, so shutdown gets a fresh, bounded context.
	shutdownCtx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	shutdownErr := srv.Shutdown(shutdownCtx)
	if shutdownErr != nil {
		_ = srv.Close()
		shutdownErr = fmt.Errorf("graceful shutdown: %w", shutdownErr)
	}

	if err := <-serveErr; !errors.Is(err, http.ErrServerClosed) {
		return errors.Join(shutdownErr, fmt.Errorf("serve: %w", err))
	}
	return shutdownErr
}
