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
