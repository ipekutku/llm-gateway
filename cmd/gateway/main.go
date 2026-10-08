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

	"github.com/ipekutku/llm-gateway/internal/httpapi"
	"github.com/ipekutku/llm-gateway/internal/llm"
	"github.com/ipekutku/llm-gateway/internal/provider/anthropic"
	"github.com/ipekutku/llm-gateway/internal/provider/openai"
	"github.com/ipekutku/llm-gateway/internal/retry"
	"github.com/ipekutku/llm-gateway/internal/routing"
)

const (
	// readHeaderTimeout bounds how long a client may take to send request
	// headers.
	readHeaderTimeout = 5 * time.Second

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
	cfg, err := loadConfig(getenv)
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
	if cfg.OpenAI != nil {
		attrs = append(attrs, slog.String("openai_model", cfg.OpenAI.Model))
	}
	if cfg.Anthropic != nil {
		attrs = append(attrs, slog.String("anthropic_model", cfg.Anthropic.Model))
	}
	attrs = append(attrs,
		slog.Duration("upstream_timeout", cfg.UpstreamTimeout),
		slog.Duration("upstream_connect_timeout", cfg.ConnectTimeout),
		slog.Int("retry_max_attempts", cfg.Retry.MaxAttempts),
		slog.Duration("retry_base_delay", cfg.Retry.BaseDelay),
		slog.Duration("retry_max_delay", cfg.Retry.MaxDelay),
	)
	logger.Info("gateway listening", attrs...)

	srv := &http.Server{
		Handler:           handler,
		ReadHeaderTimeout: readHeaderTimeout,
		ErrorLog:          slog.NewLogLogger(logger.Handler(), slog.LevelWarn),
	}
	err = serve(ctx, srv, ln, shutdownTimeout)
	logger.Info("gateway stopped")
	return err
}

// newHandler builds the provider clients, router, and HTTP handler for cfg.
// Each provider client is wrapped with the retry policy, below the router.
// A nil httpClient uses a client built by newUpstreamClient.
func newHandler(cfg config, httpClient *http.Client, logger *slog.Logger) (http.Handler, error) {
	if httpClient == nil {
		httpClient = newUpstreamClient(cfg)
	}
	routes := make(map[string]llm.Provider)
	if p := cfg.OpenAI; p != nil {
		c, err := openai.New(p.APIKey, p.BaseURL, httpClient)
		if err != nil {
			return nil, err
		}
		if routes[p.Model], err = retry.New(c, cfg.Retry, logger); err != nil {
			return nil, err
		}
	}
	if p := cfg.Anthropic; p != nil {
		c, err := anthropic.New(p.APIKey, p.BaseURL, httpClient)
		if err != nil {
			return nil, err
		}
		if routes[p.Model], err = retry.New(c, cfg.Retry, logger); err != nil {
			return nil, err
		}
	}

	router, err := routing.New(routes)
	if err != nil {
		return nil, err
	}
	return httpapi.New(router, cfg.UpstreamTimeout, logger)
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
