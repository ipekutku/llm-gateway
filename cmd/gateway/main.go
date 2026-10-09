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
	"github.com/ipekutku/llm-gateway/internal/provider/anthropic"
	"github.com/ipekutku/llm-gateway/internal/provider/openai"
	"github.com/ipekutku/llm-gateway/internal/ratelimit"
	"github.com/ipekutku/llm-gateway/internal/retry"
	"github.com/ipekutku/llm-gateway/internal/routing"
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

	// idleConnTimeout is how long an unused upstream connection stays in
	// the pool.
	idleConnTimeout = 90 * time.Second
)

func main() {
	logger := slog.New(slog.NewTextHandler(os.Stderr, nil))

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if err := run(ctx, os.Getenv, logger); err != nil {
		logger.Error("gateway failed", slog.Any("error", err))
		os.Exit(1)
	}
}

// run loads the configuration, starts the server, and serves until ctx is
// canceled.
func run(ctx context.Context, getenv func(string) string, logger *slog.Logger) error {
	cfg, err := loadConfig(getenv, os.ReadFile)
	if err != nil {
		return fmt.Errorf("configuration: %w", err)
	}
	handler, err := newHandler(cfg, nil, logger)
	if err != nil {
		return err
	}

	ln, err := net.Listen("tcp", cfg.Addr)
	if err != nil {
		return fmt.Errorf("listen: %w", err)
	}
	attrs := []any{slog.String("addr", ln.Addr().String())}
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

	err = serve(ctx, newServer(handler, logger), ln, shutdownTimeout)
	logger.Info("gateway stopped")
	return err
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
// newUpstreamClient.
func newHandler(cfg config, httpClient *http.Client, logger *slog.Logger) (http.Handler, error) {
	if httpClient == nil {
		httpClient = newUpstreamClient(cfg)
	}

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
		wrapped, err := resilient(openai.ProviderName, c, cfg, logger)
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
		wrapped, err := resilient(anthropic.ProviderName, c, cfg, logger)
		if err != nil {
			return nil, err
		}
		providers[anthropic.ProviderName] = enabled{p, wrapped}
	}

	// A fallback reuses the other provider's wrapped client, so each
	// provider has exactly one breaker whichever route reaches it.
	routes := make(map[string]routing.Route, len(providers))
	for _, e := range providers {
		route := routing.Route{Provider: e.provider}
		if e.cfg.FallbackTo != "" {
			f := providers[e.cfg.FallbackTo]
			route.Fallback = &routing.Fallback{Model: f.cfg.Model, Provider: f.provider, PrimaryTimeout: cfg.ProviderTimeout}
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
	return httpapi.New(router, authenticator, limiter, cfg.UpstreamTimeout, logger)
}

// resilient wraps a provider client with the retry policy and, above it,
// a circuit breaker, so the breaker sees one outcome per request.
func resilient(name string, p llm.Provider, cfg config, logger *slog.Logger) (llm.Provider, error) {
	r, err := retry.New(p, cfg.Retry, logger)
	if err != nil {
		return nil, err
	}
	return breaker.New(name, r, cfg.Breaker, logger)
}

// newUpstreamClient returns the HTTP client shared by all provider
// adapters. Its transport bounds connection setup; the per-request upstream
// timeout is applied through the request context by the handler.
func newUpstreamClient(cfg config) *http.Client {
	t := http.DefaultTransport.(*http.Transport).Clone()
	t.DialContext = (&net.Dialer{Timeout: cfg.ConnectTimeout, KeepAlive: 30 * time.Second}).DialContext
	t.TLSHandshakeTimeout = cfg.ConnectTimeout
	t.IdleConnTimeout = idleConnTimeout
	return &http.Client{Transport: t}
}

// serve runs srv on ln until ctx is canceled, then shuts down gracefully.
// If in-flight requests have not finished within timeout, it closes the
// server, which cancels their contexts, and returns an error.
func serve(ctx context.Context, srv *http.Server, ln net.Listener, timeout time.Duration) error {
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
