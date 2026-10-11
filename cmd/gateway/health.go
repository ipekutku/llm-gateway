package main

import (
	"context"
	"io"
	"net/http"
	"time"

	"github.com/ipekutku/llm-gateway/internal/httpapi"
)

const (
	// livenessPath answers 200 while the process serves HTTP.
	livenessPath = "/livez"

	// readinessPath answers 200 until shutdown begins, then 503.
	readinessPath = "/readyz"
)

// probeReadTimeout bounds how long the server spends discarding the body
// of a probe request, which is never read. Replaced in tests.
var probeReadTimeout = httpapi.BodyReadTimeout

// withProbes serves the liveness and readiness probes ahead of next, the
// gateway's API handler, and passes every other request to next. Probes need
// no gateway key and create no request ID, log line, span, metric, or usage
// record, so frequent probing adds nothing to the telemetry.
//
// Readiness depends only on ctx, which is done once shutdown begins:
// /readyz answers 503 from then on, including throughout the shutdown
// delay, while other requests are still served. PostgreSQL, Redis,
// and the providers are deliberately not checked; accounting is best-effort,
// rate limits fall back to local ones, and circuit breakers handle provider
// failures, so none of them should take the gateway out of service. The
// listener opens only after the startup checks, so a pod that answers has
// passed them.
func withProbes(ctx context.Context, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var ready bool
		switch r.URL.Path {
		case livenessPath:
			ready = true
		case readinessPath:
			ready = ctx.Err() == nil
		default:
			next.ServeHTTP(w, r)
			return
		}
		// Like the API handler, bound discarding an unexpected request body,
		// so a stalled sender cannot hold the connection.
		_ = http.NewResponseController(w).SetReadDeadline(time.Now().Add(probeReadTimeout))
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			w.Header().Set("Allow", "GET, HEAD")
			writeProbe(w, http.StatusMethodNotAllowed, "method not allowed\n")
			return
		}
		if !ready {
			writeProbe(w, http.StatusServiceUnavailable, "shutting down\n")
			return
		}
		writeProbe(w, http.StatusOK, "ok\n")
	})
}

// writeProbe writes a fixed plain-text probe response.
func writeProbe(w http.ResponseWriter, status int, body string) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_, _ = io.WriteString(w, body)
}
