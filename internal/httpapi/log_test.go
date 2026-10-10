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
	h, err := New(p, testAuthenticator(t), limiter, testTimeout, testAccounting(), logger)
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
