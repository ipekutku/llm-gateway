//go:build performance

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"
)

// Exercise the actual pinned generator against production inbound server
// settings, without instrumenting the gateway binary. This isolates connection
// reuse, not auth/routing/accounting (covered by full load scenarios).
func TestVegetaInboundConnectionReuse(t *testing.T) {
	vegeta := os.Getenv("LOADTEST_VEGETA_BIN")
	if vegeta == "" {
		t.Skip("set LOADTEST_VEGETA_BIN to a load run's bin/vegeta for the optional generator probe")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
	defer cancel()
	var accepted, requests atomic.Int64
	gate := make(chan struct{})
	server := newServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer r.Body.Close()
		if _, err := io.Copy(io.Discard, r.Body); err != nil {
			return
		}
		if requests.Add(1) == 500 {
			close(gate)
		}
		select {
		case <-gate:
		case <-ctx.Done():
			return
		case <-r.Context().Done():
			return
		}
		timer := time.NewTimer(500 * time.Millisecond)
		defer timer.Stop()
		select {
		case <-timer.C:
		case <-ctx.Done():
			return
		case <-r.Context().Done():
			return
		}
		_, _ = io.WriteString(w, "synthetic")
	}), slog.New(slog.DiscardHandler))
	server.ConnState = func(_ net.Conn, state http.ConnState) {
		if state == http.StateNew {
			accepted.Add(1)
		}
	}
	s := httptest.NewUnstartedServer(server.Handler)
	s.Config = server
	s.Start()
	t.Cleanup(s.Close)
	t.Cleanup(cancel)
	dir := t.TempDir()
	body := filepath.Join(dir, "body.json")
	if err := os.WriteFile(body, []byte(`{"synthetic":true}`), 0600); err != nil {
		t.Fatal(err)
	}
	results := filepath.Join(dir, "results.bin")
	cmd := exec.CommandContext(ctx, vegeta, "-cpus=8", "attack", "-duration=3s", "-rate=0", "-workers=500", "-max-workers=500", "-connections=10000", "-keepalive=true", "-http2=false", "-timeout=10s", "-max-body=0", "-output="+results)
	cmd.Stdin = bytes.NewBufferString("POST " + s.URL + "\nContent-Type: application/json\n@" + body + "\n")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("Vegeta: %v\n%s", err, out)
	}
	out, err := exec.CommandContext(ctx, vegeta, "-cpus=8", "report", "-type=json", results).CombinedOutput()
	if err != nil {
		t.Fatalf("report: %v\n%s", err, out)
	}
	var report struct {
		Requests            uint64
		Success, Throughput float64
	}
	if err := json.Unmarshal(out, &report); err != nil {
		t.Fatal(err)
	}
	if report.Success != 1 || report.Requests <= 500 || accepted.Load() != 500 {
		t.Fatalf("requests=%d success=%g accepted=%d", report.Requests, report.Success, accepted.Load())
	}
	t.Logf("Vegeta requests=%d accepted=%d success=%g throughput=%.2f; production IdleTimeout=%s", report.Requests, accepted.Load(), report.Success, report.Throughput, server.IdleTimeout)
}
