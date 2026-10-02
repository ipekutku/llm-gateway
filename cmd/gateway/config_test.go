package main

import (
	"reflect"
	"strings"
	"testing"

	"github.com/ipekutku/llm-gateway/internal/provider/anthropic"
	"github.com/ipekutku/llm-gateway/internal/provider/openai"
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

	tests := []struct {
		name string
		vars map[string]string
		want config
	}{
		{
			name: "openai only",
			vars: map[string]string{"OPENAI_MODEL": "gpt-4o", "OPENAI_API_KEY": openaiKey},
			want: config{Addr: defaultAddr, OpenAI: openaiCfg},
		},
		{
			name: "anthropic only",
			vars: map[string]string{"ANTHROPIC_MODEL": "claude-opus-5-5", "ANTHROPIC_API_KEY": anthropicKey},
			want: config{Addr: defaultAddr, Anthropic: anthropicCfg},
		},
		{
			name: "both providers and custom address",
			vars: map[string]string{
				"OPENAI_MODEL": "gpt-4o", "OPENAI_API_KEY": openaiKey,
				"ANTHROPIC_MODEL": "claude-opus-5-5", "ANTHROPIC_API_KEY": anthropicKey,
				"GATEWAY_ADDR": ":9090",
			},
			want: config{Addr: ":9090", OpenAI: openaiCfg, Anthropic: anthropicCfg},
		},
		{
			name: "blank variables count as absent",
			vars: map[string]string{
				"OPENAI_MODEL": "gpt-4o", "OPENAI_API_KEY": openaiKey,
				"ANTHROPIC_MODEL": " ", "ANTHROPIC_API_KEY": "",
				"GATEWAY_ADDR": "  ",
			},
			want: config{Addr: defaultAddr, OpenAI: openaiCfg},
		},
		{
			name: "model forwarded unchanged",
			vars: map[string]string{"OPENAI_MODEL": " gpt-4o ", "OPENAI_API_KEY": openaiKey},
			want: config{Addr: defaultAddr, OpenAI: &providerConfig{Model: " gpt-4o ", APIKey: openaiKey, BaseURL: openai.DefaultBaseURL}},
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
