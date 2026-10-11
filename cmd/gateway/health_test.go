package main

import (
	"bufio"
	"context"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"go.opentelemetry.io/otel/trace"
)

func TestProbes(t *testing.T) {
	serving := context.Background()
	stopped, cancel := context.WithCancel(context.Background())
	cancel()

	const passed = http.StatusTeapot
	tests := []struct {
		name         string
		ctx          context.Context
		method, path string
		status       int
		body         string
	}{
		{"liveness", serving, http.MethodGet, "/livez", http.StatusOK, "ok\n"},
		{"readiness", serving, http.MethodGet, "/readyz", http.StatusOK, "ok\n"},
		{"readiness while shutting down", stopped, http.MethodGet, "/readyz", http.StatusServiceUnavailable, "shutting down\n"},
		{"liveness while shutting down", stopped, http.MethodGet, "/livez", http.StatusOK, "ok\n"},
		{"liveness HEAD", serving, http.MethodHead, "/livez", http.StatusOK, "ok\n"},
		{"liveness POST", serving, http.MethodPost, "/livez", http.StatusMethodNotAllowed, "method not allowed\n"},
		{"readiness DELETE while shutting down", stopped, http.MethodDelete, "/readyz", http.StatusMethodNotAllowed, "method not allowed\n"},
		{"chat completions", serving, http.MethodPost, "/v1/chat/completions", passed, ""},
		{"chat completions while shutting down", stopped, http.MethodPost, "/v1/chat/completions", passed, ""},
		{"trailing slash", serving, http.MethodGet, "/livez/", passed, ""},
		{"metrics", serving, http.MethodGet, "/metrics", passed, ""},
		{"root", serving, http.MethodGet, "/", passed, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(passed) })
			rec := httptest.NewRecorder()
			withProbes(tt.ctx, next).ServeHTTP(rec, httptest.NewRequest(tt.method, tt.path, nil))

			if rec.Code != tt.status || rec.Body.String() != tt.body {
				t.Errorf("%s %s = %d %q, want %d %q", tt.method, tt.path, rec.Code, rec.Body.String(), tt.status, tt.body)
			}
			if tt.status == passed {
				return
			}
			if got := rec.Header().Get("Content-Type"); got != "text/plain; charset=utf-8" {
				t.Errorf("Content-Type = %q", got)
			}
			if got := rec.Header().Get("Cache-Control"); got != "no-store" {
				t.Errorf("Cache-Control = %q", got)
			}
			if allow := rec.Header().Get("Allow"); (tt.status == http.StatusMethodNotAllowed) != (allow == "GET, HEAD") {
				t.Errorf("Allow = %q", allow)
			}
		})
	}
}

// A probe never reads a body. Discarding one that stalls is bounded, so
// the response still arrives and the connection is released.
func TestProbeBoundsStalledBody(t *testing.T) {
	old := probeReadTimeout
	probeReadTimeout = 100 * time.Millisecond
	t.Cleanup(func() { probeReadTimeout = old })

	srv := httptest.NewServer(withProbes(context.Background(), http.NotFoundHandler()))
	t.Cleanup(srv.Close)
	conn, err := net.Dial("tcp", srv.Listener.Addr().String())
	if err != nil {
		t.Fatalf("Dial() error = %v", err)
	}
	t.Cleanup(func() { conn.Close() })
	if err := conn.SetDeadline(time.Now().Add(guard)); err != nil {
		t.Fatalf("SetDeadline() error = %v", err)
	}
	if _, err := io.WriteString(conn, "GET /livez HTTP/1.1\r\nHost: gateway\r\nContent-Length: 10\r\n\r\nx"); err != nil {
		t.Fatalf("Write() error = %v", err)
	}
	resp, err := http.ReadResponse(bufio.NewReader(conn), nil)
	if err != nil {
		t.Fatalf("ReadResponse() error = %v (no response before the guard deadline)", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want 200", resp.StatusCode)
	}
}

// Probes on the running gateway need no key and leave no trace in logs,
// metrics, spans, or usage records.
func TestRunServesProbesWithoutTelemetry(t *testing.T) {
	collector := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("collector received %s %s; probes must not create spans", r.Method, r.URL.Path)
	}))
	t.Cleanup(collector.Close)
	// The exporter reads its endpoint from the process environment.
	t.Setenv(otlpEndpointVar, collector.URL)

	store := &fakeStartupStore{}
	var logs syncBuffer
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	result := make(chan error, 1)
	go func() {
		vars := map[string]string{
			"ANTHROPIC_MODEL": "claude-opus-5-5", "ANTHROPIC_API_KEY": anthropicKey,
			"GATEWAY_ADDR": "127.0.0.1:0", "GATEWAY_METRICS_ADDR": "127.0.0.1:0", clientsFileVar: writeClientsFile(t),
			databaseURLVar: testDatabaseURL, pricingFileVar: pricingPath, otlpEndpointVar: collector.URL,
		}
		result <- runWith(ctx, env(vars), func(path string) ([]byte, error) {
			if path == pricingPath {
				return []byte(`{"prices":[]}`), nil
			}
			return os.ReadFile(path)
		}, func(context.Context, string, trace.TracerProvider) (accountingStore, error) {
			return store, nil
		}, slog.New(slog.NewTextHandler(&logs, nil)))
	}()
	deadline := time.After(guard)
	var match []string
	for match == nil {
		select {
		case err := <-result:
			t.Fatalf("run() returned early: %v", err)
		case <-deadline:
			t.Fatal("timed out waiting for the gateway to listen")
		case <-time.After(10 * time.Millisecond):
		}
		match = listenAddrs.FindStringSubmatch(logs.String())
	}
	if !strings.Contains(logs.String(), "tracing=true") {
		t.Fatalf("startup log does not report tracing:\n%s", logs.String())
	}
	api, metricsURL := "http://"+match[1], "http://"+match[2]
	startupLogs := logs.String()
	before := gatewaySeries(t, metricsURL)

	for _, probe := range []struct {
		method, path string
		status       int
	}{
		{http.MethodGet, "/livez", http.StatusOK},
		{http.MethodGet, "/readyz", http.StatusOK},
		{http.MethodHead, "/readyz", http.StatusOK},
		{http.MethodPost, "/readyz", http.StatusMethodNotAllowed},
	} {
		req, err := http.NewRequestWithContext(ctx, probe.method, api+probe.path, nil)
		if err != nil {
			t.Fatalf("NewRequest() error = %v", err)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("%s %s error = %v", probe.method, probe.path, err)
		}
		resp.Body.Close()
		if resp.StatusCode != probe.status {
			t.Errorf("%s %s = %d, want %d", probe.method, probe.path, resp.StatusCode, probe.status)
		}
		if id := resp.Header.Get("X-Request-ID"); id != "" {
			t.Errorf("%s %s has request ID %q, want none", probe.method, probe.path, id)
		}
	}
	if after := gatewaySeries(t, metricsURL); after != before {
		t.Errorf("probes changed gateway metrics:\nbefore:\n%s\nafter:\n%s", before, after)
	}
	if got := logs.String(); got != startupLogs {
		t.Errorf("probes logged:\n%s", strings.TrimPrefix(got, startupLogs))
	}

	// Shutdown flushes pending spans and usage records before run returns.
	cancel()
	select {
	case err := <-result:
		if err != nil {
			t.Fatalf("run() error = %v", err)
		}
	case <-time.After(guard):
		t.Fatal("timed out waiting for shutdown")
	}
	if n := len(store.stored()); n != 0 {
		t.Errorf("probes stored %d usage records, want 0", n)
	}
}

// gatewaySeries scrapes the gateway's own metric samples, without Go
// runtime and process metrics.
func gatewaySeries(t *testing.T, metricsURL string) string {
	t.Helper()
	resp, err := http.Get(metricsURL + "/metrics")
	if err != nil {
		t.Fatalf("GET /metrics error = %v", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /metrics = %d, %v", resp.StatusCode, err)
	}
	var series []string
	for line := range strings.Lines(string(body)) {
		if strings.HasPrefix(line, "gateway_") {
			series = append(series, line)
		}
	}
	if len(series) == 0 {
		t.Fatalf("no gateway metrics in:\n%s", body)
	}
	return strings.Join(series, "")
}
