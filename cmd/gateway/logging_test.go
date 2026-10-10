package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ipekutku/llm-gateway/internal/httpapi"
)

func TestNewLoggerFormats(t *testing.T) {
	for _, tc := range []struct {
		value string
		want  string
	}{
		{"", "level=INFO msg=hello\n"},
		{"text", "level=INFO msg=hello\n"},
		{" json ", `"msg":"hello"`},
	} {
		var out bytes.Buffer
		logger, err := newLogger(&out, env(map[string]string{logFormatVar: tc.value}))
		if err != nil {
			t.Fatalf("%s=%q: newLogger() error = %v", logFormatVar, tc.value, err)
		}
		logger.Info("hello")
		if !strings.Contains(out.String(), tc.want) {
			t.Errorf("%s=%q: output %q does not contain %q", logFormatVar, tc.value, out.String(), tc.want)
		}
	}
}

func TestNewLoggerRejectsUnknownFormat(t *testing.T) {
	for _, value := range []string{"JSON", "logfmt", "yaml"} {
		_, err := newLogger(&bytes.Buffer{}, env(map[string]string{logFormatVar: value}))
		if err == nil || !strings.Contains(err.Error(), logFormatVar) {
			t.Errorf("%s=%q: newLogger() error = %v, want one naming %s", logFormatVar, value, err, logFormatVar)
		}
	}
}

func TestRequestPathLogsCarryRequestContext(t *testing.T) {
	// Every attempt fails to connect, so the retry layer logs each retry and
	// the handler logs the failure. Every line must be a JSON object naming
	// the request and the client.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("Listen() error = %v", err)
	}
	refused := "http://" + ln.Addr().String()
	ln.Close()

	cfg := gatewayConfig(newUpstream(t, "/v1/chat/completions", reply(http.StatusOK, openaiReply)),
		newUpstream(t, "/v1/messages", reply(http.StatusOK, anthropicReply)))
	cfg.OpenAI.BaseURL = refused
	var logs syncBuffer
	logger, err := newLogger(&logs, env(map[string]string{logFormatVar: "json"}))
	if err != nil {
		t.Fatalf("newLogger() error = %v", err)
	}
	h, err := newHandler(cfg, nil, discardRecorder{}, logger)
	if err != nil {
		t.Fatalf("newHandler() error = %v", err)
	}
	gw := httptest.NewServer(h)
	t.Cleanup(gw.Close)

	resp, _, err := postChat(t, context.Background(), gw.URL, chatBody("gpt-4o"))
	if err != nil {
		t.Fatalf("POST error = %v", err)
	}
	id := resp.Header.Get(httpapi.RequestIDHeader)

	retries := 0
	lines := 0
	for line := range strings.Lines(logs.String()) {
		lines++
		var m map[string]any
		if err := json.Unmarshal([]byte(line), &m); err != nil {
			t.Fatalf("log line is not a JSON object: %v\n%s", err, line)
		}
		if m["request_id"] != id || m["client_id"] != "team-a" {
			t.Errorf("log line lacks request_id %s or client_id team-a: %s", id, line)
		}
		if m["msg"] == "upstream attempt failed, retrying" {
			retries++
		}
	}
	if retries != fastRetry.MaxAttempts-1 || lines != retries+1 {
		t.Errorf("got %d lines with %d retries, want %d retries and one failure:\n%s", lines, retries, fastRetry.MaxAttempts-1, logs.String())
	}
}

// loggedGateway is gatewayWith, logging JSON to the returned buffer.
func loggedGateway(t *testing.T, oa, an *upstream, edit func(*config)) (*httptest.Server, *syncBuffer) {
	t.Helper()
	cfg := gatewayConfig(oa, an)
	edit(&cfg)
	logs := new(syncBuffer)
	logger, err := newLogger(logs, env(map[string]string{logFormatVar: "json"}))
	if err != nil {
		t.Fatalf("newLogger() error = %v", err)
	}
	h, err := newHandler(cfg, nil, discardRecorder{}, logger)
	if err != nil {
		t.Fatalf("newHandler() error = %v", err)
	}
	gw := httptest.NewServer(h)
	t.Cleanup(gw.Close)
	return gw, logs
}

// outcomeLines returns the "request completed" lines in logs.
func outcomeLines(t *testing.T, logs *syncBuffer) []map[string]any {
	t.Helper()
	var lines []map[string]any
	for line := range strings.Lines(logs.String()) {
		var m map[string]any
		if err := json.Unmarshal([]byte(line), &m); err != nil {
			t.Fatalf("log line is not a JSON object: %v\n%s", err, line)
		}
		if m["msg"] == "request completed" {
			lines = append(lines, m)
		}
	}
	return lines
}

func TestRequestPathLogsOneOutcomePerRequest(t *testing.T) {
	retries := float64(fastRetry.MaxAttempts - 1)
	for _, tt := range []struct {
		name     string
		oaStatus int
		fallback bool // configure OpenAI → Anthropic
		want     map[string]any
	}{
		{"success", http.StatusOK, false, map[string]any{
			"status": 200.0, "provider": "openai", "retry_count": 0.0, "fallback": false, "level": "INFO"}},
		{"upstream failure", http.StatusInternalServerError, false, map[string]any{
			"status": 502.0, "code": "upstream_error", "provider": "openai", "upstream_status": 500.0, "retry_count": 0.0, "fallback": false, "level": "ERROR"}},
		{"retry exhaustion", http.StatusServiceUnavailable, false, map[string]any{
			"status": 502.0, "code": "upstream_error", "provider": "openai", "upstream_status": 503.0, "retry_count": retries, "fallback": false}},
		{"fallback success", http.StatusServiceUnavailable, true, map[string]any{
			"status": 200.0, "provider": "anthropic", "retry_count": retries, "fallback": true, "level": "INFO"}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			oa := newUpstream(t, "/v1/chat/completions", reply(tt.oaStatus, openaiReply))
			an := newUpstream(t, "/v1/messages", reply(http.StatusOK, anthropicReply))
			gw, logs := loggedGateway(t, oa, an, func(c *config) {
				if tt.fallback {
					c.OpenAI.FallbackTo = "anthropic"
				}
			})

			resp, _, err := postChat(t, context.Background(), gw.URL, chatBody("gpt-4o"))
			if err != nil {
				t.Fatalf("POST error = %v", err)
			}

			lines := outcomeLines(t, logs)
			if len(lines) != 1 {
				t.Fatalf("got %d outcome lines, want 1:\n%s", len(lines), logs.String())
			}
			got := lines[0]
			if got["request_id"] != resp.Header.Get(httpapi.RequestIDHeader) || got["client_id"] != "team-a" || got["model"] != "gpt-4o" {
				t.Errorf("outcome line does not identify the request: %v", got)
			}
			for key, want := range tt.want {
				if got[key] != want {
					t.Errorf("%s = %v, want %v", key, got[key], want)
				}
			}
			if strings.Contains(logs.String(), "Be brief.") || strings.Contains(logs.String(), clientKey) {
				t.Errorf("logs contain prompt content or the gateway key:\n%s", logs.String())
			}
		})
	}
}

func TestRequestPathLogsClientCancellationOnce(t *testing.T) {
	slow := newBlockingHandler(t, "")
	oa := newUpstream(t, "/v1/chat/completions", slow.ServeHTTP)
	an := newUpstream(t, "/v1/messages", reply(http.StatusOK, anthropicReply))
	gw, logs := loggedGateway(t, oa, an, func(*config) {})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _, _ = postChat(t, ctx, gw.URL, chatBody("gpt-4o"))
	}()
	waitFor(t, slow.started, "upstream to receive the request")
	cancel()
	waitFor(t, slow.canceled, "cancellation to reach the upstream")
	waitFor(t, done, "the client to return")
	gw.Close() // Waits for the handler to finish logging.

	lines := outcomeLines(t, logs)
	if len(lines) != 1 {
		t.Fatalf("got %d outcome lines, want 1:\n%s", len(lines), logs.String())
	}
	if lines[0]["status"] != 499.0 || lines[0]["code"] != "client_closed" || lines[0]["level"] != "INFO" {
		t.Errorf("outcome = %v, want an info line with status 499 client_closed", lines[0])
	}
}
