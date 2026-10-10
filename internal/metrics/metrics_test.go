package metrics

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func newTestMetrics(t *testing.T) *Metrics {
	t.Helper()
	m, err := New([]string{"gpt-4o", "claude-opus-5-5"})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	return m
}

// scrape returns the metrics text served by m.
func scrape(t *testing.T, m *Metrics) string {
	t.Helper()
	rec := httptest.NewRecorder()
	m.Handler(slog.New(slog.DiscardHandler)).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("metrics status = %d", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/plain") {
		t.Errorf("Content-Type = %q, want the Prometheus text format", ct)
	}
	body, _ := io.ReadAll(rec.Body)
	return string(body)
}

// seriesLines returns the lines of text for the metric name, without
// comments.
func seriesLines(text, name string) []string {
	var lines []string
	for line := range strings.Lines(text) {
		if strings.HasPrefix(line, name+"{") || strings.HasPrefix(line, name+" ") {
			lines = append(lines, strings.TrimSpace(line))
		}
	}
	return lines
}

func TestNewRejectsInvalidModels(t *testing.T) {
	for _, models := range [][]string{{""}, {"  "}, {UnknownModel}} {
		if _, err := New(models); err == nil {
			t.Errorf("New(%q) error = nil, want an error", models)
		}
	}
}

func TestObserveRequest(t *testing.T) {
	m := newTestMetrics(t)
	m.ObserveRequest("gpt-4o", http.StatusOK, "", 300*time.Millisecond)
	m.ObserveRequest("gpt-4o", http.StatusOK, "", 2*time.Second)
	m.ObserveRequest("claude-opus-5-5", http.StatusBadGateway, "upstream_error", time.Second)
	m.ObserveRequest("", http.StatusUnauthorized, "invalid_api_key", time.Millisecond)

	text := scrape(t, m)
	for _, want := range []string{
		`gateway_requests_total{code="",model="gpt-4o",status="200"} 2`,
		`gateway_requests_total{code="upstream_error",model="claude-opus-5-5",status="502"} 1`,
		`gateway_requests_total{code="invalid_api_key",model="unknown",status="401"} 1`,
		`gateway_request_duration_seconds_count{model="gpt-4o"} 2`,
		`gateway_request_duration_seconds_sum{model="gpt-4o"} 2.3`,
		`gateway_request_duration_seconds_bucket{model="gpt-4o",le="0.5"} 1`,
		`gateway_request_duration_seconds_count{model="unknown"} 1`,
	} {
		if !strings.Contains(text, want) {
			t.Errorf("metrics do not contain %s:\n%s", want, text)
		}
	}
	if !strings.Contains(text, "go_goroutines") {
		t.Error("metrics do not include the Go runtime metrics")
	}
}

func TestModelLabelsAreBounded(t *testing.T) {
	m := newTestMetrics(t)
	for _, model := range []string{"gpt-5", "GPT-4O", "gpt-4o ", "claude", "../../etc/passwd", strings.Repeat("x", 1000), UnknownModel} {
		m.ObserveRequest(model, http.StatusNotFound, "model_not_found", time.Millisecond)
	}

	text := scrape(t, m)
	got := seriesLines(text, "gateway_requests_total")
	want := `gateway_requests_total{code="model_not_found",model="unknown",status="404"} 7`
	if len(got) != 1 || got[0] != want {
		t.Errorf("request series = %q, want only %s", got, want)
	}
	if n := len(seriesLines(text, "gateway_request_duration_seconds_count")); n != 1 {
		t.Errorf("got %d duration series, want 1", n)
	}
}
