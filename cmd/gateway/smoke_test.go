//go:build smoke

// The smoke test calls the real provider APIs with real keys, so it costs
// money and is excluded from normal test runs and CI. Run it with
// `make smoke` after setting OPENAI_MODEL and OPENAI_API_KEY, or
// ANTHROPIC_MODEL and ANTHROPIC_API_KEY, or both.

package main

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"
)

// smokeVars are the only environment variables the smoke test reads, so
// other gateway settings in the environment cannot change what it checks.
var smokeVars = []string{"OPENAI_MODEL", "OPENAI_API_KEY", "ANTHROPIC_MODEL", "ANTHROPIC_API_KEY"}

func TestSmoke(t *testing.T) {
	// Two gateway clients with fresh random keys: one for the real calls,
	// and one with room for a single request, to check the rate limit
	// without calling a provider.
	key, limitedKey := rand.Text(), rand.Text()
	clients := `{"clients": [
		{"id": "smoke", "key_sha256": "` + hashHex(key) + `"},
		{"id": "smoke-limited", "key_sha256": "` + hashHex(limitedKey) + `", "requests_per_minute": 1, "burst": 1}
	]}`
	vars := map[string]string{clientsFileVar: "clients.json"}
	for _, name := range smokeVars {
		vars[name] = os.Getenv(name)
	}
	cfg, err := loadConfig(env(vars), files(map[string]string{"clients.json": clients}))
	if err != nil {
		t.Fatalf("configuration: %v\nSet OPENAI_MODEL and OPENAI_API_KEY, ANTHROPIC_MODEL and ANTHROPIC_API_KEY, or both.", err)
	}
	h, err := newHandler(cfg, nil, slog.New(slog.NewTextHandler(t.Output(), nil)))
	if err != nil {
		t.Fatalf("newHandler() error = %v", err)
	}
	gw := httptest.NewServer(h)
	t.Cleanup(gw.Close)

	var models []string
	if cfg.OpenAI != nil {
		models = append(models, cfg.OpenAI.Model)
	}
	if cfg.Anthropic != nil {
		models = append(models, cfg.Anthropic.Model)
	}

	for _, model := range models {
		t.Run(model+"/completion", func(t *testing.T) {
			r := smokeChat(t, gw.URL, key, model, "Reply with the single word: hi.", 1024)
			r.wantStatus(t, http.StatusOK)
			if r.FinishReason != "stop" && r.FinishReason != "length" {
				t.Errorf("finish_reason = %q, want stop or length", r.FinishReason)
			}
			if r.FinishReason == "stop" && strings.TrimSpace(r.Content) == "" {
				t.Error("finished with stop but returned no text")
			}
			r.checkUsage(t)
			t.Logf("model %s answered as %q: finish_reason=%s, usage=%d+%d tokens",
				model, r.Model, r.FinishReason, r.Usage.PromptTokens, r.Usage.CompletionTokens)
		})
		t.Run(model+"/length", func(t *testing.T) {
			r := smokeChat(t, gw.URL, key, model, "Count from 1 to 200, separated by spaces.", 16)
			r.wantStatus(t, http.StatusOK)
			if r.FinishReason != "length" {
				t.Errorf("finish_reason = %q, want length", r.FinishReason)
			}
			r.checkUsage(t)
		})
	}

	t.Run("unknown key", func(t *testing.T) {
		smokeChat(t, gw.URL, "not-a-gateway-key", models[0], "hi", 16).wantError(t, http.StatusUnauthorized, "invalid_api_key")
	})
	t.Run("missing key", func(t *testing.T) {
		smokeChat(t, gw.URL, "", models[0], "hi", 16).wantError(t, http.StatusUnauthorized, "missing_api_key")
	})
	t.Run("rate limit", func(t *testing.T) {
		// An invalid request uses the client's one request without
		// reaching a provider; the next one is over the limit.
		smokeChat(t, gw.URL, limitedKey, models[0], "", 16).wantError(t, http.StatusBadRequest, "invalid_request")
		r := smokeChat(t, gw.URL, limitedKey, models[0], "hi", 16)
		r.wantError(t, http.StatusTooManyRequests, "rate_limit_exceeded")
		if r.retryAfter == "" {
			t.Error("Retry-After header missing")
		}
	})
}

// smokeResult is a gateway response, flattened for the checks above.
type smokeResult struct {
	status     int
	retryAfter string
	raw        []byte

	Model        string
	Content      string
	FinishReason string
	Usage        struct {
		PromptTokens     int `json:"prompt_tokens"`
		CompletionTokens int `json:"completion_tokens"`
	}
	ErrorCode string
}

// smokeChat sends one chat request with the gateway key, or none if key is
// empty. An empty content makes the request invalid.
func smokeChat(t *testing.T, url, key, model, content string, maxTokens int) smokeResult {
	t.Helper()
	body, _ := json.Marshal(map[string]any{
		"model":      model,
		"messages":   []map[string]string{{"role": "user", "content": content}},
		"max_tokens": maxTokens,
	})
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url+"/v1/chat/completions", strings.NewReader(string(body)))
	if err != nil {
		t.Fatalf("NewRequest() error = %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST error = %v", err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read response: %v", err)
	}

	var wire struct {
		Model   string `json:"model"`
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
			FinishReason string `json:"finish_reason"`
		} `json:"choices"`
		Usage json.RawMessage `json:"usage"`
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(raw, &wire); err != nil {
		t.Fatalf("status %d, response is not JSON: %v", resp.StatusCode, err)
	}
	r := smokeResult{status: resp.StatusCode, retryAfter: resp.Header.Get("Retry-After"), raw: raw, Model: wire.Model, ErrorCode: wire.Error.Code}
	if len(wire.Choices) > 0 {
		r.Content, r.FinishReason = wire.Choices[0].Message.Content, wire.Choices[0].FinishReason
	}
	if len(wire.Usage) > 0 {
		_ = json.Unmarshal(wire.Usage, &r.Usage)
	}
	return r
}

func (r smokeResult) wantStatus(t *testing.T, status int) {
	t.Helper()
	if r.status != status {
		// The gateway's error envelope never contains secrets or upstream
		// bodies; the gateway log above has the details.
		t.Fatalf("status = %d, want %d: %s", r.status, status, r.raw)
	}
}

func (r smokeResult) wantError(t *testing.T, status int, code string) {
	t.Helper()
	r.wantStatus(t, status)
	if r.ErrorCode != code {
		t.Errorf("error code = %q, want %q", r.ErrorCode, code)
	}
}

func (r smokeResult) checkUsage(t *testing.T) {
	t.Helper()
	if r.Usage.PromptTokens <= 0 || r.Usage.CompletionTokens <= 0 {
		t.Errorf("usage = %+v, want positive token counts", r.Usage)
	}
}
