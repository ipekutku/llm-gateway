package main

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/ipekutku/llm-gateway/internal/provider/anthropic"
	"github.com/ipekutku/llm-gateway/internal/provider/openai"
)

const (
	// defaultAddr is the listen address when GATEWAY_ADDR is unset.
	defaultAddr = "127.0.0.1:8080"

	// defaultUpstreamTimeout bounds all upstream work for one request when
	// GATEWAY_UPSTREAM_TIMEOUT is unset.
	defaultUpstreamTimeout = 120 * time.Second

	// defaultConnectTimeout bounds the TCP dial and, separately, the TLS
	// handshake to an upstream when GATEWAY_UPSTREAM_CONNECT_TIMEOUT is unset.
	defaultConnectTimeout = 10 * time.Second
)

// config is the gateway's startup configuration.
type config struct {
	Addr string
	// UpstreamTimeout bounds all upstream work for one request.
	UpstreamTimeout time.Duration
	// ConnectTimeout bounds establishing an upstream connection.
	ConnectTimeout time.Duration
	// OpenAI and Anthropic are nil when the provider is disabled.
	OpenAI    *providerConfig
	Anthropic *providerConfig
}

// providerConfig enables one provider for exactly one model.
type providerConfig struct {
	Model   string
	APIKey  string
	BaseURL string
}

// loadConfig reads the configuration from getenv, normally os.Getenv. A
// variable that is unset or blank counts as absent. Errors name the
// offending variables but never include their values.
func loadConfig(getenv func(string) string) (config, error) {
	cfg := config{Addr: defaultAddr}
	if addr := getenv("GATEWAY_ADDR"); strings.TrimSpace(addr) != "" {
		cfg.Addr = addr
	}

	var errs []error
	var err error
	if cfg.UpstreamTimeout, err = loadDuration(getenv, "GATEWAY_UPSTREAM_TIMEOUT", defaultUpstreamTimeout); err != nil {
		errs = append(errs, err)
	}
	if cfg.ConnectTimeout, err = loadDuration(getenv, "GATEWAY_UPSTREAM_CONNECT_TIMEOUT", defaultConnectTimeout); err != nil {
		errs = append(errs, err)
	}
	if cfg.OpenAI, err = loadProvider(getenv, "OPENAI_MODEL", "OPENAI_API_KEY", openai.DefaultBaseURL); err != nil {
		errs = append(errs, err)
	}
	if cfg.Anthropic, err = loadProvider(getenv, "ANTHROPIC_MODEL", "ANTHROPIC_API_KEY", anthropic.DefaultBaseURL); err != nil {
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
