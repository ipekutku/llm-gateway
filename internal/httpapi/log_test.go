package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"strings"
	"testing"

	"github.com/ipekutku/llm-gateway/internal/llm"
	"github.com/ipekutku/llm-gateway/internal/ratelimit"
)

// jsonLines parses logs as one JSON object per line.
func jsonLines(t *testing.T, logs *bytes.Buffer) []map[string]any {
	t.Helper()
	var lines []map[string]any
	for line := range strings.Lines(logs.String()) {
		var m map[string]any
		if err := json.Unmarshal([]byte(line), &m); err != nil {
			t.Fatalf("log line is not a JSON object: %v\n%s", err, line)
		}
		lines = append(lines, m)
	}
	return lines
}

func TestLogHandlerAddsIDsToEveryRequestLog(t *testing.T) {
	var logs bytes.Buffer
	logger := slog.New(NewLogHandler(slog.NewJSONHandler(&logs, nil)))
	// The provider logs through the same logger with the request's context,
	// as the retry and breaker layers do, then fails, so the handler logs
	// too. A logger built with With must keep adding the IDs.
	p := providerFunc(func(ctx context.Context, _ llm.ChatRequest) (llm.ChatResponse, error) {
		logger.With(slog.String("component", "test")).InfoContext(ctx, "provider attempt")
		return llm.ChatResponse{}, &llm.ProviderError{Provider: "openai", StatusCode: http.StatusServiceUnavailable}
	})
	limiter := testLimiter(t, map[string]ratelimit.Limits{"team-a": generous})
	h, err := New(p, testAuthenticator(t), limiter, testTimeout, testAccounting(), nil, logger)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}

	rec := post(t, h, validBody)

	lines := jsonLines(t, &logs)
	if len(lines) != 2 {
		t.Fatalf("got %d log lines, want 2:\n%s", len(lines), logs.String())
	}
	for _, line := range lines {
		if line["request_id"] != rec.Header().Get(RequestIDHeader) || line["client_id"] != "team-a" {
			t.Errorf("log line %v lacks request_id %s or client_id team-a", line, rec.Header().Get(RequestIDHeader))
		}
	}
	// A wrapped logger passed to New is not wrapped again, so the IDs are
	// not duplicated.
	for line := range strings.Lines(logs.String()) {
		if n := strings.Count(line, `"request_id"`); n != 1 {
			t.Errorf("request_id appears %d times in %s", n, line)
		}
	}
}

func TestLogHandlerOmitsClientBeforeAuthentication(t *testing.T) {
	h, logs := newHandler(t, failingProvider(nil))
	r := strings.NewReader(validBody)
	req, _ := http.NewRequest(http.MethodPost, ChatCompletionsPath, r)
	req.Header.Set("Authorization", "Bearer unknown-key")
	rec := serveRaw(h, req)

	if want := "request_id=" + rec.Header().Get(RequestIDHeader); !strings.Contains(logs.String(), want) {
		t.Errorf("401 log does not contain %s:\n%s", want, logs)
	}
	if strings.Contains(logs.String(), "client_id") {
		t.Errorf("401 log names a client:\n%s", logs)
	}
}

func TestLogHandlerPassesOtherRecordsUnchanged(t *testing.T) {
	var logs bytes.Buffer
	logger := slog.New(NewLogHandler(slog.NewTextHandler(&logs, nil)))
	logger.InfoContext(context.Background(), "startup", slog.String("addr", "127.0.0.1:8080"))
	logger.Info("no context")

	if strings.Contains(logs.String(), "request_id") || strings.Contains(logs.String(), "client_id") {
		t.Errorf("records without a request context gained IDs:\n%s", logs.String())
	}
	if !strings.Contains(logs.String(), "addr=127.0.0.1:8080") {
		t.Errorf("record attributes were lost:\n%s", logs.String())
	}
}

func TestLogHandlerRespectsLevel(t *testing.T) {
	var logs bytes.Buffer
	logger := slog.New(NewLogHandler(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelWarn})))
	logger.Info("hidden")
	if logs.Len() != 0 {
		t.Errorf("a record below the level was logged:\n%s", logs.String())
	}
}

// newJSONHandler returns a handler for p that logs JSON to the returned
// buffer.
func newJSONHandler(t *testing.T, p llm.Provider) (http.Handler, *bytes.Buffer) {
	t.Helper()
	var logs bytes.Buffer
	limiter := testLimiter(t, map[string]ratelimit.Limits{"team-a": generous})
	h, err := New(p, testAuthenticator(t), limiter, testTimeout, testAccounting(), nil, slog.New(slog.NewJSONHandler(&logs, nil)))
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	return h, &logs
}

func TestOutcomeLogReportsUpstreamActivity(t *testing.T) {
	const prompt, completion = "private prompt marker", "private completion marker"
	// The provider reports activity as the retry and routing layers do.
	h, logs := newJSONHandler(t, providerFunc(func(ctx context.Context, req llm.ChatRequest) (llm.ChatResponse, error) {
		llm.StatsFrom(ctx).AddRetry()
		llm.StatsFrom(ctx).AddRetry()
		llm.StatsFrom(ctx).SetFallback()
		return llm.ChatResponse{
			Provider:     "anthropic",
			Model:        "model-b",
			Message:      llm.Message{Role: llm.RoleAssistant, Content: completion},
			FinishReason: llm.FinishReasonStop,
		}, nil
	}))

	rec := post(t, h, `{"model":"model-a","messages":[{"role":"user","content":"`+prompt+`"}]}`)

	lines := jsonLines(t, logs)
	if len(lines) != 1 {
		t.Fatalf("got %d log lines, want 1:\n%s", len(lines), logs)
	}
	got := lines[0]
	for key, want := range map[string]any{
		"level":       "INFO",
		"msg":         "request completed",
		"request_id":  rec.Header().Get(RequestIDHeader),
		"client_id":   "team-a",
		"model":       "model-a",
		"provider":    "anthropic",
		"status":      float64(http.StatusOK),
		"retry_count": float64(2),
		"fallback":    true,
	} {
		if got[key] != want {
			t.Errorf("%s = %v, want %v", key, got[key], want)
		}
	}
	if latency, ok := got["latency"].(float64); !ok || latency <= 0 {
		t.Errorf("latency = %v, want a positive duration", got["latency"])
	}
	for _, key := range []string{"code", "error", "upstream_status"} {
		if _, ok := got[key]; ok {
			t.Errorf("successful request logged %s = %v", key, got[key])
		}
	}
	if strings.Contains(logs.String(), prompt) || strings.Contains(logs.String(), completion) {
		t.Errorf("log contains prompt or completion content:\n%s", logs)
	}
}

func TestOutcomeLogLevels(t *testing.T) {
	for _, tt := range []struct {
		name  string
		err   error
		level string
	}{
		{"upstream rejected", &llm.ProviderError{Provider: "openai", StatusCode: http.StatusBadRequest}, "WARN"},
		{"upstream failed", &llm.ProviderError{Provider: "openai", StatusCode: http.StatusServiceUnavailable}, "ERROR"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			h, logs := newJSONHandler(t, failingProvider(tt.err))
			post(t, h, validBody)

			lines := jsonLines(t, logs)
			if len(lines) != 1 || lines[0]["msg"] != "request completed" || lines[0]["level"] != tt.level {
				t.Errorf("logs = %v, want one request completed line at %s", lines, tt.level)
			}
		})
	}
}

func TestOutcomeLogNotWrittenForRejectedRequests(t *testing.T) {
	h, logs := newHandler(t, okProvider)
	post(t, h, `{"model":"model-a","messages":[]}`)

	if strings.Contains(logs.String(), "request completed") || !strings.Contains(logs.String(), "chat completion failed") {
		t.Errorf("a validation failure must log only its rejection:\n%s", logs)
	}
}
