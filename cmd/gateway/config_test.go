package main

import (
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/ipekutku/llm-gateway/internal/provider/anthropic"
	"github.com/ipekutku/llm-gateway/internal/provider/openai"
	"github.com/ipekutku/llm-gateway/internal/retry"
)

// env returns a getenv func backed by vars.
func env(vars map[string]string) func(string) string {
	return func(name string) string { return vars[name] }
}

const (
	openaiKey    = "sk-openai-secret-value"
	anthropicKey = "sk-ant-secret-value"
)

func TestLoadConfig(t *testing.T) {
	openaiCfg := &providerConfig{Model: "gpt-4o", APIKey: openaiKey, BaseURL: openai.DefaultBaseURL}
	anthropicCfg := &providerConfig{Model: "claude-opus-5-5", APIKey: anthropicKey, BaseURL: anthropic.DefaultBaseURL}

	// withDefaults fills in the default timeouts and retry policy.
	withDefaults := func(c config) config {
		c.UpstreamTimeout, c.ConnectTimeout = defaultUpstreamTimeout, defaultConnectTimeout
		c.Retry = defaultRetry
		return c
	}

	tests := []struct {
		name string
		vars map[string]string
		want config
	}{
		{
			name: "openai only",
			vars: map[string]string{"OPENAI_MODEL": "gpt-4o", "OPENAI_API_KEY": openaiKey},
			want: withDefaults(config{Addr: defaultAddr, OpenAI: openaiCfg}),
		},
		{
			name: "anthropic only",
			vars: map[string]string{"ANTHROPIC_MODEL": "claude-opus-5-5", "ANTHROPIC_API_KEY": anthropicKey},
			want: withDefaults(config{Addr: defaultAddr, Anthropic: anthropicCfg}),
		},
		{
			name: "both providers and custom address",
			vars: map[string]string{
				"OPENAI_MODEL": "gpt-4o", "OPENAI_API_KEY": openaiKey,
				"ANTHROPIC_MODEL": "claude-opus-5-5", "ANTHROPIC_API_KEY": anthropicKey,
				"GATEWAY_ADDR": ":9090",
			},
			want: withDefaults(config{Addr: ":9090", OpenAI: openaiCfg, Anthropic: anthropicCfg}),
		},
		{
			name: "blank variables count as absent",
			vars: map[string]string{
				"OPENAI_MODEL": "gpt-4o", "OPENAI_API_KEY": openaiKey,
				"ANTHROPIC_MODEL": " ", "ANTHROPIC_API_KEY": "",
				"GATEWAY_ADDR": "  ",
			},
			want: withDefaults(config{Addr: defaultAddr, OpenAI: openaiCfg}),
		},
		{
			name: "model forwarded unchanged",
			vars: map[string]string{"OPENAI_MODEL": " gpt-4o ", "OPENAI_API_KEY": openaiKey},
			want: withDefaults(config{Addr: defaultAddr, OpenAI: &providerConfig{Model: " gpt-4o ", APIKey: openaiKey, BaseURL: openai.DefaultBaseURL}}),
		},
		{
			name: "custom timeouts",
			vars: map[string]string{
				"OPENAI_MODEL": "gpt-4o", "OPENAI_API_KEY": openaiKey,
				"GATEWAY_UPSTREAM_TIMEOUT": "45s", "GATEWAY_UPSTREAM_CONNECT_TIMEOUT": " 1500ms ",
			},
			want: config{Addr: defaultAddr, OpenAI: openaiCfg, UpstreamTimeout: 45 * time.Second, ConnectTimeout: 1500 * time.Millisecond, Retry: defaultRetry},
		},
		{
			name: "custom retry policy",
			vars: map[string]string{
				"OPENAI_MODEL": "gpt-4o", "OPENAI_API_KEY": openaiKey,
				"GATEWAY_RETRY_MAX_ATTEMPTS": " 5 ", "GATEWAY_RETRY_BASE_DELAY": "200ms", "GATEWAY_RETRY_MAX_DELAY": "200ms",
			},
			want: withDefaults(config{Addr: defaultAddr, OpenAI: openaiCfg}).withRetry(retry.Policy{MaxAttempts: 5, BaseDelay: 200 * time.Millisecond, MaxDelay: 200 * time.Millisecond}),
		},
		{
			name: "retries disabled",
			vars: map[string]string{
				"OPENAI_MODEL": "gpt-4o", "OPENAI_API_KEY": openaiKey,
				"GATEWAY_RETRY_MAX_ATTEMPTS": "1",
			},
			want: withDefaults(config{Addr: defaultAddr, OpenAI: openaiCfg}).withRetry(retry.Policy{MaxAttempts: 1, BaseDelay: defaultRetryBaseDelay, MaxDelay: defaultRetryMaxDelay}),
		},
		{
			name: "blank timeouts use defaults",
			vars: map[string]string{
				"OPENAI_MODEL": "gpt-4o", "OPENAI_API_KEY": openaiKey,
				"GATEWAY_UPSTREAM_TIMEOUT": " ", "GATEWAY_UPSTREAM_CONNECT_TIMEOUT": "",
			},
			want: withDefaults(config{Addr: defaultAddr, OpenAI: openaiCfg}),
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := loadConfig(env(tt.vars))
			if err != nil {
				t.Fatalf("loadConfig() error = %v", err)
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("loadConfig() = %+v, want %+v", got, tt.want)
			}
		})
	}
}

var defaultRetry = retry.Policy{
	MaxAttempts: defaultRetryMaxAttempts,
	BaseDelay:   defaultRetryBaseDelay,
	MaxDelay:    defaultRetryMaxDelay,
}

func (c config) withRetry(p retry.Policy) config {
	c.Retry = p
	return c
}

func TestLoadConfigErrors(t *testing.T) {
	tests := []struct {
		name string
		vars map[string]string
		want []string
	}{
		{
			name: "no providers",
			vars: map[string]string{"GATEWAY_ADDR": ":9090"},
			want: []string{"no provider configured"},
		},
		{
			name: "openai key without model",
			vars: map[string]string{"OPENAI_API_KEY": openaiKey},
			want: []string{"OPENAI_API_KEY is set but OPENAI_MODEL is missing"},
		},
		{
			name: "openai model without key",
			vars: map[string]string{"OPENAI_MODEL": "gpt-4o"},
			want: []string{"OPENAI_MODEL is set but OPENAI_API_KEY is missing"},
		},
		{
			name: "anthropic key without model",
			vars: map[string]string{"ANTHROPIC_API_KEY": anthropicKey},
			want: []string{"ANTHROPIC_API_KEY is set but ANTHROPIC_MODEL is missing"},
		},
		{
			name: "partial anthropic next to a complete openai",
			vars: map[string]string{
				"OPENAI_MODEL": "gpt-4o", "OPENAI_API_KEY": openaiKey,
				"ANTHROPIC_MODEL": "claude-opus-5-5",
			},
			want: []string{"ANTHROPIC_MODEL is set but ANTHROPIC_API_KEY is missing"},
		},
		{
			name: "both partial",
			vars: map[string]string{"OPENAI_MODEL": "gpt-4o", "ANTHROPIC_API_KEY": anthropicKey},
			want: []string{
				"OPENAI_MODEL is set but OPENAI_API_KEY is missing",
				"ANTHROPIC_API_KEY is set but ANTHROPIC_MODEL is missing",
			},
		},
		{
			name: "duplicate model",
			vars: map[string]string{
				"OPENAI_MODEL": "same-model", "OPENAI_API_KEY": openaiKey,
				"ANTHROPIC_MODEL": "same-model", "ANTHROPIC_API_KEY": anthropicKey,
			},
			want: []string{"OPENAI_MODEL and ANTHROPIC_MODEL must be different"},
		},
		{
			name: "unparsable upstream timeout",
			vars: map[string]string{
				"OPENAI_MODEL": "gpt-4o", "OPENAI_API_KEY": openaiKey,
				"GATEWAY_UPSTREAM_TIMEOUT": "120",
			},
			want: []string{"GATEWAY_UPSTREAM_TIMEOUT must be a positive duration"},
		},
		{
			name: "non-positive timeouts reported with other errors",
			vars: map[string]string{
				"OPENAI_MODEL":             "gpt-4o",
				"GATEWAY_UPSTREAM_TIMEOUT": "0s", "GATEWAY_UPSTREAM_CONNECT_TIMEOUT": "-1s",
			},
			want: []string{
				"GATEWAY_UPSTREAM_TIMEOUT must be a positive duration",
				"GATEWAY_UPSTREAM_CONNECT_TIMEOUT must be a positive duration",
				"OPENAI_MODEL is set but OPENAI_API_KEY is missing",
			},
		},
		{
			name: "retry attempts out of range",
			vars: map[string]string{
				"OPENAI_MODEL": "gpt-4o", "OPENAI_API_KEY": openaiKey,
				"GATEWAY_RETRY_MAX_ATTEMPTS": "11",
			},
			want: []string{"GATEWAY_RETRY_MAX_ATTEMPTS must be an integer from 1 to 10"},
		},
		{
			name: "zero retry attempts",
			vars: map[string]string{
				"OPENAI_MODEL": "gpt-4o", "OPENAI_API_KEY": openaiKey,
				"GATEWAY_RETRY_MAX_ATTEMPTS": "0",
			},
			want: []string{"GATEWAY_RETRY_MAX_ATTEMPTS must be an integer from 1 to 10"},
		},
		{
			name: "invalid retry settings reported together",
			vars: map[string]string{
				"OPENAI_MODEL": "gpt-4o", "OPENAI_API_KEY": openaiKey,
				"GATEWAY_RETRY_MAX_ATTEMPTS": "three", "GATEWAY_RETRY_BASE_DELAY": "0s", "GATEWAY_RETRY_MAX_DELAY": "soon",
			},
			want: []string{
				"GATEWAY_RETRY_MAX_ATTEMPTS must be an integer from 1 to 10",
				"GATEWAY_RETRY_BASE_DELAY must be a positive duration",
				"GATEWAY_RETRY_MAX_DELAY must be a positive duration",
			},
		},
		{
			name: "retry max delay below base delay",
			vars: map[string]string{
				"OPENAI_MODEL": "gpt-4o", "OPENAI_API_KEY": openaiKey,
				"GATEWAY_RETRY_BASE_DELAY": "2s", "GATEWAY_RETRY_MAX_DELAY": "1s",
			},
			want: []string{"GATEWAY_RETRY_MAX_DELAY must not be less than GATEWAY_RETRY_BASE_DELAY"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := loadConfig(env(tt.vars))
			if err == nil {
				t.Fatal("loadConfig() error = nil, want error")
			}
			for _, want := range tt.want {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("error %q does not contain %q", err, want)
				}
			}
			for _, secret := range []string{openaiKey, anthropicKey} {
				if strings.Contains(err.Error(), secret) {
					t.Errorf("error exposes a secret: %q", err)
				}
			}
		})
	}
}
