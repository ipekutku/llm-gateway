package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/ipekutku/llm-gateway/internal/postgres"
	"github.com/ipekutku/llm-gateway/internal/usage"
)

const databaseStartupTimeout = 10 * time.Second
const migrationTimeout = time.Minute

func loadDatabaseURL(getenv func(string) string) (string, error) {
	url := strings.TrimSpace(getenv(databaseURLVar))
	if url == "" {
		return "", fmt.Errorf("%s is required", databaseURLVar)
	}
	return url, nil
}

type pricingEntry struct {
	Provider   string  `json:"provider"`
	Model      string  `json:"model"`
	Input      *string `json:"input"`
	CacheRead  *string `json:"cache_read"`
	CacheWrite *string `json:"cache_write"`
	Output     *string `json:"output"`
}

// Preserve filesystem causes without printing a path or other file settings.
type pricingFileError struct{ cause error }

func (e *pricingFileError) Error() string { return pricingFileVar + ": cannot read pricing file" }
func (e *pricingFileError) Unwrap() error { return e.cause }

// loadPricing keeps decimal prices as strings until ParseRate converts them
// to exact integers. Empty prices are valid: unpriced models have NULL costs.
func loadPricing(getenv func(string) string, readFile func(string) ([]byte, error)) (*usage.Pricing, error) {
	path := strings.TrimSpace(getenv(pricingFileVar))
	if path == "" {
		return nil, fmt.Errorf("%s is required", pricingFileVar)
	}
	data, err := readFile(path)
	if err != nil {
		return nil, &pricingFileError{cause: err}
	}
	var file struct {
		Prices []pricingEntry `json:"prices"`
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&file); err != nil {
		return nil, fmt.Errorf("%s: invalid JSON", pricingFileVar)
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("%s: data after the top-level object", pricingFileVar)
	}
	if file.Prices == nil {
		return nil, fmt.Errorf("%s: prices must be an array", pricingFileVar)
	}
	prices := make(map[usage.Model]usage.Price, len(file.Prices))
	for i, entry := range file.Prices {
		model := usage.Model{Provider: entry.Provider, Model: entry.Model}
		if strings.TrimSpace(model.Provider) == "" || strings.TrimSpace(model.Model) == "" {
			return nil, fmt.Errorf("%s: prices[%d] needs provider and model", pricingFileVar, i)
		}
		if _, exists := prices[model]; exists {
			return nil, fmt.Errorf("%s: prices[%d] duplicates a provider/model price", pricingFileVar, i)
		}
		var price usage.Price
		for _, field := range []struct {
			name string
			raw  *string
			rate *usage.Rate
		}{
			{"input", entry.Input, &price.Input},
			{"cache_read", entry.CacheRead, &price.CacheRead},
			{"cache_write", entry.CacheWrite, &price.CacheWrite},
			{"output", entry.Output, &price.Output},
		} {
			if field.raw == nil {
				return nil, fmt.Errorf("%s: prices[%d].%s is required", pricingFileVar, i, field.name)
			}
			rate, err := usage.ParseRate(*field.raw)
			if err != nil {
				return nil, fmt.Errorf("%s: prices[%d].%s is not a valid price", pricingFileVar, i, field.name)
			}
			*field.rate = rate
		}
		prices[model] = price
	}
	return usage.NewPricing(prices)
}

// migrate needs only database configuration and never calls a provider.
func migrate(ctx context.Context, getenv func(string) string, logger *slog.Logger) error {
	url, err := loadDatabaseURL(getenv)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, migrationTimeout)
	defer cancel()
	store, err := postgres.Open(ctx, url)
	if err != nil {
		return err
	}
	defer store.Close()
	n, err := store.Migrate(ctx)
	if err != nil {
		return err
	}
	logger.Info("database migrated", slog.Int("applied", n))
	return nil
}

func execute(ctx context.Context, args []string, getenv func(string) string, logger *slog.Logger) error {
	if len(args) == 0 {
		return run(ctx, getenv, logger)
	}
	if len(args) == 1 && args[0] == "migrate" {
		return migrate(ctx, getenv, logger)
	}
	return errors.New("usage: gateway [migrate]")
}

// activeHandlers lets canceled handlers finish enqueueing their records after
// forced HTTP shutdown, before the recorder is closed. The mutex prevents an
// Add after stop has begun waiting on an empty WaitGroup.
type activeHandlers struct {
	next    http.Handler
	mu      sync.Mutex
	wg      sync.WaitGroup
	closing bool
}

func (a *activeHandlers) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	a.mu.Lock()
	if a.closing {
		a.mu.Unlock()
		return // The HTTP server has already closed the connection.
	}
	a.wg.Add(1)
	a.mu.Unlock()
	defer a.wg.Done()
	a.next.ServeHTTP(w, r)
}

func (a *activeHandlers) stop(ctx context.Context) error {
	a.mu.Lock()
	a.closing = true
	a.mu.Unlock()
	done := make(chan struct{})
	go func() { a.wg.Wait(); close(done) }()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return fmt.Errorf("waiting for canceled handlers: %w", ctx.Err())
	}
}
