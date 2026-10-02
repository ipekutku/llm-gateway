package main

import (
	"errors"
	"fmt"
	"strings"

	"github.com/ipekutku/llm-gateway/internal/provider/anthropic"
	"github.com/ipekutku/llm-gateway/internal/provider/openai"
)

// defaultAddr is the listen address when GATEWAY_ADDR is unset.
const defaultAddr = "127.0.0.1:8080"

// config is the gateway's startup configuration.
type config struct {
	Addr string
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
