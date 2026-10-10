package main

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"

	"github.com/ipekutku/llm-gateway/internal/auth"
	"github.com/ipekutku/llm-gateway/internal/breaker"
	"github.com/ipekutku/llm-gateway/internal/provider/anthropic"
	"github.com/ipekutku/llm-gateway/internal/provider/openai"
	"github.com/ipekutku/llm-gateway/internal/ratelimit"
	"github.com/ipekutku/llm-gateway/internal/retry"
	"github.com/ipekutku/llm-gateway/internal/usage"
)

const (
	// defaultAddr is the listen address when GATEWAY_ADDR is unset.
	defaultAddr = "127.0.0.1:8080"

	// defaultMetricsAddr is the metrics listen address when
	// GATEWAY_METRICS_ADDR is unset.
	defaultMetricsAddr = "127.0.0.1:9464"

	// defaultUpstreamTimeout bounds all upstream work for one request when
	// GATEWAY_UPSTREAM_TIMEOUT is unset.
	defaultUpstreamTimeout = 120 * time.Second

	// defaultConnectTimeout bounds the TCP dial and, separately, the TLS
	// handshake to an upstream when GATEWAY_UPSTREAM_CONNECT_TIMEOUT is unset.
	defaultConnectTimeout = 10 * time.Second

	// Retry defaults: up to two retries, waiting at most 0.5s and then 1s
	// before them unless the upstream asks for longer.
	defaultRetryMaxAttempts = 3
	defaultRetryBaseDelay   = 500 * time.Millisecond
	defaultRetryMaxDelay    = 8 * time.Second

	// maxRetryAttempts bounds GATEWAY_RETRY_MAX_ATTEMPTS.
	maxRetryAttempts = 10

	// Circuit breaker defaults: a provider is skipped for 30s after five
	// consecutive failed requests.
	defaultBreakerFailures = 5
	defaultBreakerCooldown = 30 * time.Second

	// maxBreakerFailures bounds GATEWAY_BREAKER_FAILURES.
	maxBreakerFailures = 100

	// clientsFileVar names the file of gateway clients.
	clientsFileVar = "GATEWAY_CLIENTS_FILE"
	databaseURLVar = "GATEWAY_DATABASE_URL"
	pricingFileVar = "GATEWAY_PRICING_FILE"

	// Rate limit defaults for a client whose entry omits them: one request
	// per second sustained, bursts of up to 10, and 5 at a time.
	defaultRequestsPerMinute = 60
	defaultBurst             = 10
	defaultMaxConcurrent     = 5
)

// config is the gateway's startup configuration.
type config struct {
	Addr string
	// MetricsAddr is the separate listen address serving /metrics.
	MetricsAddr string
	DatabaseURL string
	Pricing     *usage.Pricing
	// UpstreamTimeout bounds all upstream work for one request.
	UpstreamTimeout time.Duration
	// ConnectTimeout bounds establishing an upstream connection.
	ConnectTimeout time.Duration
	// ProviderTimeout bounds a primary provider that has a fallback, so the
	// fallback has time left. It is less than UpstreamTimeout.
	ProviderTimeout time.Duration
	// Retry is applied to each provider.
	Retry retry.Policy
	// Breaker configures each provider's circuit breaker.
	Breaker breaker.Settings
	// OpenAI and Anthropic are nil when the provider is disabled.
	OpenAI    *providerConfig
	Anthropic *providerConfig
	// Clients are the gateway clients, valid for auth.New.
	Clients []auth.Client
	// RateLimits holds the limits of every client in Clients, by ID.
	RateLimits map[string]ratelimit.Limits
}

// providerConfig enables one provider for exactly one model.
type providerConfig struct {
	Model   string
	APIKey  string
	BaseURL string
	// FallbackTo names the provider whose model serves this provider's
	// failed requests, or is empty for no fallback.
	FallbackTo string
}

// loadConfig reads the configuration from getenv, normally os.Getenv, and
// the clients file through readFile, normally os.ReadFile. A variable that
// is unset or blank counts as absent. Errors name the offending variables
// but never include their values.
func loadConfig(getenv func(string) string, readFile func(string) ([]byte, error)) (config, error) {
	cfg := config{Addr: defaultAddr, MetricsAddr: defaultMetricsAddr}
	if addr := getenv("GATEWAY_ADDR"); strings.TrimSpace(addr) != "" {
		cfg.Addr = addr
	}
	if addr := getenv("GATEWAY_METRICS_ADDR"); strings.TrimSpace(addr) != "" {
		cfg.MetricsAddr = addr
	}

	var errs []error
	var err error
	if cfg.DatabaseURL, err = loadDatabaseURL(getenv); err != nil {
		errs = append(errs, err)
	}
	if cfg.Pricing, err = loadPricing(getenv, readFile); err != nil {
		errs = append(errs, err)
	}
	if cfg.UpstreamTimeout, err = loadDuration(getenv, "GATEWAY_UPSTREAM_TIMEOUT", defaultUpstreamTimeout); err != nil {
		errs = append(errs, err)
	}
	if cfg.ConnectTimeout, err = loadDuration(getenv, "GATEWAY_UPSTREAM_CONNECT_TIMEOUT", defaultConnectTimeout); err != nil {
		errs = append(errs, err)
	}
	if cfg.Retry, err = loadRetry(getenv); err != nil {
		errs = append(errs, err)
	}
	if cfg.Breaker.Failures, err = loadInt(getenv, "GATEWAY_BREAKER_FAILURES", defaultBreakerFailures, maxBreakerFailures); err != nil {
		errs = append(errs, err)
	}
	if cfg.Breaker.Cooldown, err = loadDuration(getenv, "GATEWAY_BREAKER_COOLDOWN", defaultBreakerCooldown); err != nil {
		errs = append(errs, err)
	}
	// The default leaves half of the request's time for a fallback. An
	// explicit value is checked against the upstream timeout below.
	providerTimeoutSet := strings.TrimSpace(getenv("GATEWAY_PROVIDER_TIMEOUT")) != ""
	if cfg.ProviderTimeout, err = loadDuration(getenv, "GATEWAY_PROVIDER_TIMEOUT", cfg.UpstreamTimeout/2); err != nil {
		errs = append(errs, err)
	}
	if cfg.OpenAI, err = loadProvider(getenv, "OPENAI_MODEL", "OPENAI_API_KEY", openai.DefaultBaseURL); err != nil {
		errs = append(errs, err)
	}
	if cfg.Anthropic, err = loadProvider(getenv, "ANTHROPIC_MODEL", "ANTHROPIC_API_KEY", anthropic.DefaultBaseURL); err != nil {
		errs = append(errs, err)
	}
	if cfg.Clients, cfg.RateLimits, err = loadClients(getenv, readFile); err != nil {
		errs = append(errs, err)
	}
	if len(errs) > 0 {
		return config{}, errors.Join(errs...)
	}
	if providerTimeoutSet && cfg.ProviderTimeout >= cfg.UpstreamTimeout {
		errs = append(errs, errors.New("GATEWAY_PROVIDER_TIMEOUT must be less than GATEWAY_UPSTREAM_TIMEOUT"))
	}
	if err := loadFallback(getenv, cfg.OpenAI, "OPENAI", cfg.Anthropic, "ANTHROPIC", anthropic.ProviderName); err != nil {
		errs = append(errs, err)
	}
	if err := loadFallback(getenv, cfg.Anthropic, "ANTHROPIC", cfg.OpenAI, "OPENAI", openai.ProviderName); err != nil {
		errs = append(errs, err)
	}
	if len(errs) > 0 {
		return config{}, errors.Join(errs...)
	}

	switch {
	case cfg.OpenAI == nil && cfg.Anthropic == nil:
		return config{}, errors.New("no provider configured: set OPENAI_MODEL and OPENAI_API_KEY, or ANTHROPIC_MODEL and ANTHROPIC_API_KEY")
	case cfg.OpenAI != nil && cfg.Anthropic != nil && cfg.OpenAI.Model == cfg.Anthropic.Model:
		return config{}, errors.New("OPENAI_MODEL and ANTHROPIC_MODEL must be different")
	}
	return cfg, nil
}

// loadProvider returns nil if both variables are absent, and an error if
// only one is set.
func loadProvider(getenv func(string) string, modelVar, keyVar, baseURL string) (*providerConfig, error) {
	model, key := getenv(modelVar), getenv(keyVar)
	hasModel, hasKey := strings.TrimSpace(model) != "", strings.TrimSpace(key) != ""
	switch {
	case !hasModel && !hasKey:
		return nil, nil
	case !hasModel:
		return nil, fmt.Errorf("%s is set but %s is missing", keyVar, modelVar)
	case !hasKey:
		return nil, fmt.Errorf("%s is set but %s is missing", modelVar, keyVar)
	}
	return &providerConfig{Model: model, APIKey: key, BaseURL: baseURL}, nil
}

// loadFallback reads <prefix>_FALLBACK for provider p, whose only possible
// fallback is the other provider. It sets p.FallbackTo when the fallback is
// valid and enabled.
func loadFallback(getenv func(string) string, p *providerConfig, prefix string, other *providerConfig, otherPrefix, otherName string) error {
	varName := prefix + "_FALLBACK"
	v := strings.TrimSpace(getenv(varName))
	switch {
	case v == "":
		return nil
	case v != otherName:
		return fmt.Errorf("%s must be %s", varName, otherName)
	case p == nil:
		return fmt.Errorf("%s is set but %s_MODEL and %s_API_KEY are missing", varName, prefix, prefix)
	case other == nil:
		return fmt.Errorf("%s=%s requires %s_MODEL and %s_API_KEY", varName, otherName, otherPrefix, otherPrefix)
	}
	p.FallbackTo = otherName
	return nil
}

// clientsFile is the format of the GATEWAY_CLIENTS_FILE file.
type clientsFile struct {
	Clients []clientEntry `json:"clients"`
}

type clientEntry struct {
	ID string `json:"id"`
	// KeySHA256 is the hex SHA-256 hash of the client's API key.
	KeySHA256 string `json:"key_sha256"`
	Disabled  bool   `json:"disabled"`
	// Limits are optional; nil means the default.
	RequestsPerMinute *int `json:"requests_per_minute"`
	Burst             *int `json:"burst"`
	MaxConcurrent     *int `json:"max_concurrent"`
}

// loadClients reads the clients file named by GATEWAY_CLIENTS_FILE. The
// file is required, must be a single JSON object without unknown fields,
// and must enable at least one client. Errors name the file variable and
// the client, never a key hash.
func loadClients(getenv func(string) string, readFile func(string) ([]byte, error)) ([]auth.Client, map[string]ratelimit.Limits, error) {
	path := strings.TrimSpace(getenv(clientsFileVar))
	if path == "" {
		return nil, nil, fmt.Errorf("%s is required: gateway clients authenticate with API keys listed there", clientsFileVar)
	}
	data, err := readFile(path)
	if err != nil {
		return nil, nil, fmt.Errorf("%s: %w", clientsFileVar, err)
	}

	var file clientsFile
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&file); err != nil {
		return nil, nil, fmt.Errorf("%s: invalid JSON: %w", clientsFileVar, err)
	}
	if _, err := dec.Token(); err != io.EOF {
		return nil, nil, fmt.Errorf("%s: invalid JSON: data after the top-level object", clientsFileVar)
	}

	clients := make([]auth.Client, 0, len(file.Clients))
	limits := make(map[string]ratelimit.Limits, len(file.Clients))
	var errs []error
	enabled := 0
	for i, e := range file.Clients {
		name := fmt.Sprintf("%s: clients[%d] (%q)", clientsFileVar, i, e.ID)
		hash, err := hex.DecodeString(e.KeySHA256)
		if err != nil || len(hash) != len(auth.Client{}.KeyHash) {
			errs = append(errs, fmt.Errorf("%s: key_sha256 must be 64 hexadecimal characters", name))
			continue
		}
		var lim ratelimit.Limits
		var limErrs []error
		for _, f := range []struct {
			field string
			value *int
			def   int
			dst   *int
		}{
			{"requests_per_minute", e.RequestsPerMinute, defaultRequestsPerMinute, &lim.RequestsPerMinute},
			{"burst", e.Burst, defaultBurst, &lim.Burst},
			{"max_concurrent", e.MaxConcurrent, defaultMaxConcurrent, &lim.MaxConcurrent},
		} {
			switch {
			case f.value == nil:
				*f.dst = f.def
			case *f.value < 1 || *f.value > ratelimit.MaxLimit:
				limErrs = append(limErrs, fmt.Errorf("%s: %s must be an integer from 1 to %d", name, f.field, ratelimit.MaxLimit))
			default:
				*f.dst = *f.value
			}
		}
		if len(limErrs) > 0 {
			errs = append(errs, limErrs...)
			continue
		}
		c := auth.Client{ID: e.ID, Disabled: e.Disabled}
		copy(c.KeyHash[:], hash)
		clients = append(clients, c)
		limits[e.ID] = lim
		if !e.Disabled {
			enabled++
		}
	}
	if len(errs) > 0 {
		return nil, nil, errors.Join(errs...)
	}
	if enabled == 0 {
		return nil, nil, fmt.Errorf("%s: no enabled client", clientsFileVar)
	}
	// Client IDs, duplicates, and shared keys are checked by the packages
	// that use them.
	if _, err := auth.New(clients); err != nil {
		return nil, nil, fmt.Errorf("%s: %w", clientsFileVar, err)
	}
	if _, err := ratelimit.New(limits); err != nil {
		return nil, nil, fmt.Errorf("%s: %w", clientsFileVar, err)
	}
	return clients, limits, nil
}

// loadDuration parses a positive Go duration such as "90s", returning def
// if the variable is absent.
func loadDuration(getenv func(string) string, name string, def time.Duration) (time.Duration, error) {
	v := strings.TrimSpace(getenv(name))
	if v == "" {
		return def, nil
	}
	d, err := time.ParseDuration(v)
	if err != nil || d <= 0 {
		return 0, fmt.Errorf("%s must be a positive duration such as 30s", name)
	}
	return d, nil
}

// loadRetry reads the retry policy. MaxDelay must not be less than
// BaseDelay, which is checked only when both are valid.
func loadRetry(getenv func(string) string) (retry.Policy, error) {
	var p retry.Policy
	var errs []error
	var err error
	if p.MaxAttempts, err = loadInt(getenv, "GATEWAY_RETRY_MAX_ATTEMPTS", defaultRetryMaxAttempts, maxRetryAttempts); err != nil {
		errs = append(errs, err)
	}
	baseDelay, baseErr := loadDuration(getenv, "GATEWAY_RETRY_BASE_DELAY", defaultRetryBaseDelay)
	maxDelay, maxErr := loadDuration(getenv, "GATEWAY_RETRY_MAX_DELAY", defaultRetryMaxDelay)
	switch {
	case baseErr != nil || maxErr != nil:
		errs = append(errs, baseErr, maxErr)
	case maxDelay < baseDelay:
		errs = append(errs, errors.New("GATEWAY_RETRY_MAX_DELAY must not be less than GATEWAY_RETRY_BASE_DELAY"))
	}
	if err := errors.Join(errs...); err != nil {
		return retry.Policy{}, err
	}
	p.BaseDelay, p.MaxDelay = baseDelay, maxDelay
	return p, nil
}

// loadInt parses an integer from 1 to max, returning def if the variable
// is absent.
func loadInt(getenv func(string) string, name string, def, max int) (int, error) {
	v := strings.TrimSpace(getenv(name))
	if v == "" {
		return def, nil
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < 1 || n > max {
		return 0, fmt.Errorf("%s must be an integer from 1 to %d", name, max)
	}
	return n, nil
}
