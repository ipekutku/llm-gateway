package main

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/ipekutku/llm-gateway/internal/usage"
	"go.opentelemetry.io/otel/trace"
)

// startGateway runs runWith with vars, both listeners on free local ports,
// and store, until ctx is canceled. It returns the API address once the
// gateway listens.
func startGateway(t *testing.T, ctx context.Context, vars map[string]string, store *fakeStartupStore) (addr string, logs *syncBuffer, result <-chan error) {
	t.Helper()
	vars["GATEWAY_ADDR"], vars["GATEWAY_METRICS_ADDR"] = "127.0.0.1:0", "127.0.0.1:0"
	vars[clientsFileVar], vars[databaseURLVar], vars[pricingFileVar] = clientsPath, testDatabaseURL, pricingPath
	logs = &syncBuffer{}
	listening := &listeningHandler{Handler: slog.NewTextHandler(logs, nil), addr: make(chan string, 1)}
	done := make(chan error, 1)
	go func() {
		done <- runWith(ctx, env(vars), files(map[string]string{clientsPath: testClientsFile, pricingPath: `{"prices":[]}`}),
			func(context.Context, string, trace.TracerProvider) (accountingStore, error) { return store, nil },
			slog.New(listening))
	}()
	select {
	case addr = <-listening.addr:
	case err := <-done:
		t.Fatalf("runWith() returned early: %v", err)
	case <-time.After(guard):
		t.Fatal("gateway did not start")
	}
	return "http://" + addr, logs, done
}

// newConnection sends each request on a new connection, so a request
// shows whether the listener still accepts connections.
var newConnection = &http.Client{Transport: &http.Transport{DisableKeepAlives: true}, Timeout: guard}

func status(t *testing.T, method, url, body string) int {
	t.Helper()
	req, err := http.NewRequest(method, url, strings.NewReader(body))
	if err != nil {
		t.Fatalf("NewRequest() error = %v", err)
	}
	if body != "" {
		req.Header.Set("Authorization", "Bearer "+clientKey)
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := newConnection.Do(req)
	if err != nil {
		t.Fatalf("%s %s error = %v", method, url, err)
	}
	resp.Body.Close()
	return resp.StatusCode
}

func TestRunKeepsServingDuringShutdownDelay(t *testing.T) {
	// The test ends the delay itself, after checking what is served during it.
	delays, endDelay := fakeShutdownDelay(t)

	slow := newBlockingHandler(t, openaiReply)
	oa := newUpstream(t, "/v1/chat/completions", slow.ServeHTTP)
	an := newUpstream(t, "/v1/messages", reply(http.StatusOK, anthropicReply))
	store := &fakeStartupStore{}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	api, logs, result := startGateway(t, ctx, map[string]string{
		"OPENAI_MODEL": "gpt-4o", "OPENAI_API_KEY": openaiKey, "OPENAI_BASE_URL": oa.srv.URL,
		"ANTHROPIC_MODEL": "claude-opus-5-5", "ANTHROPIC_API_KEY": anthropicKey, "ANTHROPIC_BASE_URL": an.srv.URL,
		"GATEWAY_SHUTDOWN_DELAY": "7s",
	}, store)
	if got := status(t, http.MethodGet, api+"/readyz", ""); got != http.StatusOK {
		t.Fatalf("readiness before shutdown = %d, want 200", got)
	}

	inFlight := make(chan int, 1)
	go func() {
		resp, _, err := postChat(t, context.Background(), api, chatBody("gpt-4o"))
		if err != nil {
			inFlight <- 0
			return
		}
		inFlight <- resp.StatusCode
	}()
	waitFor(t, slow.started, "the in-flight request to reach the upstream")

	cancel() // The shutdown signal.
	select {
	case d := <-delays:
		if d != 7*time.Second {
			t.Errorf("shutdown delay = %v, want 7s", d)
		}
	case <-time.After(guard):
		t.Fatal("shutdown delay did not start")
	}

	// During the delay, readiness fails while new connections and requests
	// are still served.
	if got := status(t, http.MethodGet, api+"/readyz", ""); got != http.StatusServiceUnavailable {
		t.Errorf("readiness during the delay = %d, want 503", got)
	}
	if got := status(t, http.MethodGet, api+"/livez", ""); got != http.StatusOK {
		t.Errorf("liveness during the delay = %d, want 200", got)
	}
	if got := status(t, http.MethodPost, api+"/v1/chat/completions", chatBody("claude-opus-5-5")); got != http.StatusOK {
		t.Errorf("new request during the delay = %d, want 200", got)
	}
	slow.Release()
	select {
	case got := <-inFlight:
		if got != http.StatusOK {
			t.Errorf("in-flight request = %d, want 200", got)
		}
	case <-time.After(guard):
		t.Fatal("in-flight request did not finish")
	}
	if got := status(t, http.MethodGet, api+"/readyz", ""); got != http.StatusServiceUnavailable {
		t.Errorf("readiness at the end of the delay = %d, want 503", got)
	}

	close(endDelay)
	select {
	case err := <-result:
		if err != nil {
			t.Errorf("runWith() error = %v, want nil", err)
		}
	case <-time.After(guard):
		t.Fatal("gateway did not shut down after the delay")
	}
	if conn, err := net.Dial("tcp", strings.TrimPrefix(api, "http://")); err == nil {
		conn.Close()
		t.Error("API listener still accepts connections after shutdown")
	}
	records := store.stored()
	if len(records) != 2 || records[0].Status != http.StatusOK || records[1].Status != http.StatusOK {
		t.Errorf("usage records = %+v, want two successes", records)
	}
	if !strings.Contains(logs.String(), "shutdown_delay=7s shutdown_timeout=5s") {
		t.Errorf("startup log does not report the shutdown settings:\n%s", logs.String())
	}
}

func TestRunShutdownTimeoutCancelsSlowRequests(t *testing.T) {
	slow := newBlockingHandler(t, openaiReply)
	oa := newUpstream(t, "/v1/chat/completions", slow.ServeHTTP)
	store := &fakeStartupStore{}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	api, _, result := startGateway(t, ctx, map[string]string{
		"OPENAI_MODEL": "gpt-4o", "OPENAI_API_KEY": openaiKey, "OPENAI_BASE_URL": oa.srv.URL,
		"GATEWAY_SHUTDOWN_TIMEOUT": "50ms",
	}, store)

	clientDone := make(chan struct{})
	go func() {
		defer close(clientDone)
		_, _, _ = postChat(t, context.Background(), api, chatBody("gpt-4o"))
	}()
	waitFor(t, slow.started, "the request to reach the upstream")
	cancel()

	select {
	case err := <-result:
		if !errors.Is(err, context.DeadlineExceeded) || !strings.Contains(err.Error(), "graceful shutdown") {
			t.Errorf("runWith() error = %v, want the graceful shutdown deadline", err)
		}
	case <-time.After(guard):
		t.Fatal("gateway did not shut down after the timeout")
	}
	// Closing the server cancels the request all the way upstream, and the
	// handler still records it before the recorder drains.
	waitFor(t, slow.canceled, "cancellation to reach the upstream")
	waitFor(t, clientDone, "the client to finish")
	records := store.stored()
	if len(records) != 1 || records[0].Status != usage.StatusClientClosed || records[0].ErrorCode != "client_closed" {
		t.Errorf("usage records = %+v, want one 499 client_closed", records)
	}
}

// fakeShutdownDelay replaces shutdownDelay for the test. Each started delay
// sends its duration on the returned channel and ends when end is closed.
func fakeShutdownDelay(t *testing.T) (started <-chan time.Duration, end chan time.Time) {
	t.Helper()
	delays, end := make(chan time.Duration, 1), make(chan time.Time)
	old := shutdownDelay
	shutdownDelay = func(d time.Duration) <-chan time.Time { delays <- d; return end }
	t.Cleanup(func() { shutdownDelay = old })
	return delays, end
}

func done(ctx context.Context) bool { return ctx.Err() != nil }

func TestShutdownStages(t *testing.T) {
	t.Run("signal waits for the delay", func(t *testing.T) {
		started, end := fakeShutdownDelay(t)
		var logs syncBuffer
		signal, cancel := context.WithCancel(t.Context())
		draining, stopping, stop := shutdownStages(signal, 7*time.Second, slog.New(slog.NewTextHandler(&logs, nil)))
		defer stop()
		if done(draining) || done(stopping) {
			t.Fatal("shutdown began before the signal")
		}

		cancel()
		select {
		case d := <-started:
			if d != 7*time.Second {
				t.Errorf("delay = %v, want 7s", d)
			}
		case <-time.After(guard):
			t.Fatal("delay did not start")
		}
		if !done(draining) || done(stopping) {
			t.Errorf("during the delay: draining done = %v, stopping done = %v; want true, false", done(draining), done(stopping))
		}
		if !strings.Contains(logs.String(), `msg="shutting down" shutdown_delay=7s`) {
			t.Errorf("no shutdown line with the delay:\n%s", logs.String())
		}

		close(end)
		waitFor(t, stopping.Done(), "the servers to stop after the delay")
	})

	t.Run("server failure skips the delay", func(t *testing.T) {
		started, _ := fakeShutdownDelay(t)
		// No signal ever arrives.
		draining, stopping, stop := shutdownStages(context.Background(), time.Hour, slog.New(slog.DiscardHandler))
		stop()
		if !done(draining) || !done(stopping) {
			t.Errorf("after stop: draining done = %v, stopping done = %v; want both", done(draining), done(stopping))
		}
		select {
		case d := <-started:
			t.Errorf("a delay of %v started without a signal", d)
		default:
		}
	})
}
