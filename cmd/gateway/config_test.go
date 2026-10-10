package main

import (
	"encoding/hex"
	"io/fs"
	"maps"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/ipekutku/llm-gateway/internal/auth"
	"github.com/ipekutku/llm-gateway/internal/breaker"
	"github.com/ipekutku/llm-gateway/internal/provider/anthropic"
	"github.com/ipekutku/llm-gateway/internal/provider/openai"
	"github.com/ipekutku/llm-gateway/internal/ratelimit"
	"github.com/ipekutku/llm-gateway/internal/retry"
	"github.com/ipekutku/llm-gateway/internal/usage"
)

// env returns a getenv func backed by vars.
func env(vars map[string]string) func(string) string {
	return func(name string) string { return vars[name] }
}

// files returns a readFile func backed by contents, keyed by path.
func files(contents map[string]string) func(string) ([]byte, error) {
	return func(path string) ([]byte, error) {
		data, ok := contents[path]
		if !ok {
			return nil, &fs.PathError{Op: "open", Path: path, Err: fs.ErrNotExist}
		}
		return []byte(data), nil
	}
}

const (
	openaiKey    = "sk-openai-secret-value"
	anthropicKey = "sk-ant-secret-value"

	// clientKey is the gateway API key of the test client, team-a.
	clientKey       = "gw-test-client-key"
	clientsPath     = "/etc/gateway/clients.json"
	pricingPath     = "/etc/gateway/prices.json"
	testDatabaseURL = "postgres://gateway:synthetic-password@127.0.0.1/gateway"
)

// hashHex returns the hex SHA-256 hash of key, as written in a clients file.
func hashHex(key string) string {
	h := auth.HashKey(key)
	return hex.EncodeToString(h[:])
}

// testClientsFile enables team-a with clientKey and the default limits.
var testClientsFile = `{"clients": [{"id": "team-a", "key_sha256": "` + hashHex(clientKey) + `"}]}`

var (
	testPricing, _ = usage.NewPricing(nil)
	testClients    = []auth.Client{{ID: "team-a", KeyHash: auth.HashKey(clientKey)}}
	defaultLimits  = ratelimit.Limits{RequestsPerMinute: defaultRequestsPerMinute, Burst: defaultBurst, MaxConcurrent: defaultMaxConcurrent}
	testRateLimits = map[string]ratelimit.Limits{"team-a": defaultLimits}
)

// load is loadConfig with GATEWAY_CLIENTS_FILE pointing at
// testClientsFile, unless vars sets it.
func load(vars map[string]string) (config, error) {
	vars = maps.Clone(vars)
	if _, ok := vars[clientsFileVar]; !ok {
		if vars == nil {
			vars = map[string]string{}
		}
		vars[clientsFileVar] = clientsPath
	}
	if _, ok := vars[databaseURLVar]; !ok {
		vars[databaseURLVar] = testDatabaseURL
	}
	if _, ok := vars[pricingFileVar]; !ok {
		vars[pricingFileVar] = pricingPath
	}
	return loadConfig(env(vars), files(map[string]string{clientsPath: testClientsFile, pricingPath: `{"prices":[]}`}))
}

func TestLoadConfig(t *testing.T) {
	openaiCfg := &providerConfig{Model: "gpt-4o", APIKey: openaiKey, BaseURL: openai.DefaultBaseURL}
	anthropicCfg := &providerConfig{Model: "claude-opus-5-5", APIKey: anthropicKey, BaseURL: anthropic.DefaultBaseURL}

	// withDefaults fills in the default timeouts, retry policy, and
	// breaker settings.
	withDefaults := func(c config) config {
		c.DatabaseURL, c.Pricing = testDatabaseURL, testPricing
		c.UpstreamTimeout, c.ConnectTimeout = defaultUpstreamTimeout, defaultConnectTimeout
		c.ProviderTimeout = defaultUpstreamTimeout / 2
		c.Retry = defaultRetry
		c.Breaker = breaker.Settings{Failures: defaultBreakerFailures, Cooldown: defaultBreakerCooldown}
		c.Clients, c.RateLimits = testClients, testRateLimits
		if c.MetricsAddr == "" {
			c.MetricsAddr = defaultMetricsAddr
		}
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
			name: "both providers and custom addresses",
			vars: map[string]string{
				"OPENAI_MODEL": "gpt-4o", "OPENAI_API_KEY": openaiKey,
				"ANTHROPIC_MODEL": "claude-opus-5-5", "ANTHROPIC_API_KEY": anthropicKey,
				"GATEWAY_ADDR": ":9090", "GATEWAY_METRICS_ADDR": "127.0.0.1:9191",
			},
			want: withDefaults(config{Addr: ":9090", MetricsAddr: "127.0.0.1:9191", OpenAI: openaiCfg, Anthropic: anthropicCfg}),
		},
		{
			name: "blank variables count as absent",
			vars: map[string]string{
				"OPENAI_MODEL": "gpt-4o", "OPENAI_API_KEY": openaiKey,
				"ANTHROPIC_MODEL": " ", "ANTHROPIC_API_KEY": "",
				"GATEWAY_ADDR": "  ", "GATEWAY_METRICS_ADDR": " ",
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
			want: config{
				DatabaseURL: testDatabaseURL, Pricing: testPricing,
				Addr: defaultAddr, MetricsAddr: defaultMetricsAddr, OpenAI: openaiCfg,
				UpstreamTimeout: 45 * time.Second, ConnectTimeout: 1500 * time.Millisecond,
				ProviderTimeout: 22500 * time.Millisecond, // half the upstream timeout
				Retry:           defaultRetry,
				Breaker:         breaker.Settings{Failures: defaultBreakerFailures, Cooldown: defaultBreakerCooldown},
				Clients:         testClients,
				RateLimits:      testRateLimits,
			},
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
			name: "fallbacks both ways with custom provider timeout and breaker",
			vars: map[string]string{
				"OPENAI_MODEL": "gpt-4o", "OPENAI_API_KEY": openaiKey, "OPENAI_FALLBACK": "anthropic",
				"ANTHROPIC_MODEL": "claude-opus-5-5", "ANTHROPIC_API_KEY": anthropicKey, "ANTHROPIC_FALLBACK": " openai ",
				"GATEWAY_PROVIDER_TIMEOUT": "90s",
				"GATEWAY_BREAKER_FAILURES": "2", "GATEWAY_BREAKER_COOLDOWN": "1m",
			},
			want: func() config {
				c := withDefaults(config{
					Addr:      defaultAddr,
					OpenAI:    &providerConfig{Model: "gpt-4o", APIKey: openaiKey, BaseURL: openai.DefaultBaseURL, FallbackTo: "anthropic"},
					Anthropic: &providerConfig{Model: "claude-opus-5-5", APIKey: anthropicKey, BaseURL: anthropic.DefaultBaseURL, FallbackTo: "openai"},
				})
				c.ProviderTimeout = 90 * time.Second
				c.Breaker = breaker.Settings{Failures: 2, Cooldown: time.Minute}
				return c
			}(),
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
			got, err := load(tt.vars)
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
		{
			name: "fallback to itself",
			vars: map[string]string{
				"OPENAI_MODEL": "gpt-4o", "OPENAI_API_KEY": openaiKey, "OPENAI_FALLBACK": "openai",
			},
			want: []string{"OPENAI_FALLBACK must be anthropic"},
		},
		{
			name: "unknown fallback",
			vars: map[string]string{
				"ANTHROPIC_MODEL": "claude-opus-5-5", "ANTHROPIC_API_KEY": anthropicKey, "ANTHROPIC_FALLBACK": "OpenAI",
			},
			want: []string{"ANTHROPIC_FALLBACK must be openai"},
		},
		{
			name: "fallback to an unconfigured provider",
			vars: map[string]string{
				"OPENAI_MODEL": "gpt-4o", "OPENAI_API_KEY": openaiKey, "OPENAI_FALLBACK": "anthropic",
			},
			want: []string{"OPENAI_FALLBACK=anthropic requires ANTHROPIC_MODEL and ANTHROPIC_API_KEY"},
		},
		{
			name: "fallback on an unconfigured provider",
			vars: map[string]string{
				"ANTHROPIC_MODEL": "claude-opus-5-5", "ANTHROPIC_API_KEY": anthropicKey, "OPENAI_FALLBACK": "anthropic",
			},
			want: []string{"OPENAI_FALLBACK is set but OPENAI_MODEL and OPENAI_API_KEY are missing"},
		},
		{
			name: "provider timeout not below upstream timeout",
			vars: map[string]string{
				"OPENAI_MODEL": "gpt-4o", "OPENAI_API_KEY": openaiKey,
				"GATEWAY_UPSTREAM_TIMEOUT": "60s", "GATEWAY_PROVIDER_TIMEOUT": "60s",
			},
			want: []string{"GATEWAY_PROVIDER_TIMEOUT must be less than GATEWAY_UPSTREAM_TIMEOUT"},
		},
		{
			name: "invalid breaker settings",
			vars: map[string]string{
				"OPENAI_MODEL": "gpt-4o", "OPENAI_API_KEY": openaiKey,
				"GATEWAY_BREAKER_FAILURES": "0", "GATEWAY_BREAKER_COOLDOWN": "forever",
				"GATEWAY_PROVIDER_TIMEOUT": "-5s",
			},
			want: []string{
				"GATEWAY_BREAKER_FAILURES must be an integer from 1 to 100",
				"GATEWAY_BREAKER_COOLDOWN must be a positive duration",
				"GATEWAY_PROVIDER_TIMEOUT must be a positive duration",
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := load(tt.vars)
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

func TestLoadClients(t *testing.T) {
	file := `{
		"clients": [
			{"id": "team-a", "key_sha256": "` + hashHex("key-a") + `"},
			{"id": "team-b", "key_sha256": "` + strings.ToUpper(hashHex("key-b")) + `",
			 "requests_per_minute": 10, "burst": 2, "max_concurrent": 10},
			{"id": "team-old", "key_sha256": "` + hashHex("key-old") + `", "disabled": true}
		]
	}`
	clients, limits, err := loadClients(env(map[string]string{clientsFileVar: " " + clientsPath + " "}), files(map[string]string{clientsPath: file}))
	if err != nil {
		t.Fatalf("loadClients() error = %v", err)
	}
	wantClients := []auth.Client{
		{ID: "team-a", KeyHash: auth.HashKey("key-a")},
		{ID: "team-b", KeyHash: auth.HashKey("key-b")},
		{ID: "team-old", KeyHash: auth.HashKey("key-old"), Disabled: true},
	}
	if !reflect.DeepEqual(clients, wantClients) {
		t.Errorf("clients = %+v, want %+v", clients, wantClients)
	}
	wantLimits := map[string]ratelimit.Limits{
		"team-a":   defaultLimits,
		"team-b":   {RequestsPerMinute: 10, Burst: 2, MaxConcurrent: 10},
		"team-old": defaultLimits,
	}
	if !reflect.DeepEqual(limits, wantLimits) {
		t.Errorf("limits = %+v, want %+v", limits, wantLimits)
	}
}

func TestLoadClientsErrors(t *testing.T) {
	hashA, hashB := hashHex("key-a"), hashHex("key-b")
	tests := []struct {
		name string
		file string // absent from the file system if empty
		want []string
	}{
		{name: "missing file", want: []string{"GATEWAY_CLIENTS_FILE", "file does not exist"}},
		{name: "malformed JSON", file: `{"clients": [`, want: []string{"GATEWAY_CLIENTS_FILE: invalid JSON"}},
		{name: "trailing data", file: `{"clients": []} {}`, want: []string{"data after the top-level object"}},
		{name: "unknown field", file: `{"clients": [{"id": "team-a", "key": "plain-secret-key"}]}`, want: []string{`unknown field "key"`}},
		{name: "wrong type", file: `{"clients": [{"id": "team-a", "key_sha256": "` + hashA + `", "burst": "10"}]}`, want: []string{"invalid JSON"}},
		{name: "no clients", file: `{"clients": []}`, want: []string{"no enabled client"}},
		{name: "only disabled clients", file: `{"clients": [{"id": "team-a", "key_sha256": "` + hashA + `", "disabled": true}]}`, want: []string{"no enabled client"}},
		{name: "missing hash", file: `{"clients": [{"id": "team-a"}]}`, want: []string{`clients[0] ("team-a"): key_sha256 must be 64 hexadecimal characters`}},
		{name: "short hash", file: `{"clients": [{"id": "team-a", "key_sha256": "` + hashA[:62] + `"}]}`, want: []string{"key_sha256 must be 64 hexadecimal characters"}},
		{name: "non-hex hash", file: `{"clients": [{"id": "team-a", "key_sha256": "` + strings.Repeat("z", 64) + `"}]}`, want: []string{"key_sha256 must be 64 hexadecimal characters"}},
		{
			name: "limits out of range reported together",
			file: `{"clients": [{"id": "team-a", "key_sha256": "` + hashA + `", "requests_per_minute": 0, "burst": -1, "max_concurrent": 1000001}]}`,
			want: []string{
				"requests_per_minute must be an integer from 1 to 1000000",
				"burst must be an integer from 1 to 1000000",
				"max_concurrent must be an integer from 1 to 1000000",
			},
		},
		{name: "invalid ID", file: `{"clients": [{"id": "team a", "key_sha256": "` + hashA + `"}]}`, want: []string{"GATEWAY_CLIENTS_FILE: auth: client ID"}},
		{
			name: "duplicate ID",
			file: `{"clients": [{"id": "team-a", "key_sha256": "` + hashA + `"}, {"id": "team-a", "key_sha256": "` + hashB + `"}]}`,
			want: []string{"duplicate client ID"},
		},
		{
			name: "shared key",
			file: `{"clients": [{"id": "team-a", "key_sha256": "` + hashA + `"}, {"id": "team-b", "key_sha256": "` + hashA + `"}]}`,
			want: []string{"same key"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			contents := map[string]string{}
			if tt.file != "" {
				contents[clientsPath] = tt.file
			}
			_, _, err := loadClients(env(map[string]string{clientsFileVar: clientsPath}), files(contents))
			if err == nil {
				t.Fatal("loadClients() error = nil, want error")
			}
			for _, want := range tt.want {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("error %q does not contain %q", err, want)
				}
			}
			for _, secret := range []string{hashA, hashB, "plain-secret-key"} {
				if strings.Contains(strings.ToLower(err.Error()), secret) {
					t.Errorf("error exposes file contents: %q", err)
				}
			}
		})
	}
}

func TestLoadConfigRequiresClientsFile(t *testing.T) {
	_, err := loadConfig(env(map[string]string{"OPENAI_MODEL": "gpt-4o", "OPENAI_API_KEY": openaiKey, clientsFileVar: " "}), files(nil))
	if err == nil || !strings.Contains(err.Error(), "GATEWAY_CLIENTS_FILE is required") {
		t.Errorf("loadConfig() error = %v, want GATEWAY_CLIENTS_FILE is required", err)
	}
}

func TestLoadConfigReportsClientErrorsWithOthers(t *testing.T) {
	_, err := loadConfig(env(map[string]string{"OPENAI_MODEL": "gpt-4o", clientsFileVar: clientsPath}), files(map[string]string{clientsPath: `{"clients": []}`}))
	if err == nil {
		t.Fatal("loadConfig() error = nil, want error")
	}
	for _, want := range []string{"OPENAI_API_KEY is missing", "no enabled client"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not contain %q", err, want)
		}
	}
}
