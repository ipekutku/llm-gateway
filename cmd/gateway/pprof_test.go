package main

import (
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestRunPprofListenerIsolation(t *testing.T) {
	for _, setting := range []string{"", "false", "true"} {
		t.Run("setting="+setting, func(t *testing.T) {
			var logs syncBuffer
			listening := &listeningHandler{Handler: slog.NewTextHandler(&logs, nil), addr: make(chan string, 1)}
			ctx, cancel := context.WithCancel(t.Context())
			clientsFile := writeClientsFile(t)
			done := make(chan error, 1)
			go func() {
				done <- runWithFakeDatabase(ctx, map[string]string{
					"OPENAI_MODEL": "local", "OPENAI_API_KEY": openaiKey,
					"GATEWAY_ADDR": "127.0.0.1:0", "GATEWAY_METRICS_ADDR": "127.0.0.1:0",
					"GATEWAY_PPROF": setting, clientsFileVar: clientsFile,
				}, slog.New(listening))
			}()
			t.Cleanup(func() {
				cancel()
				select {
				case err := <-done:
					if err != nil {
						t.Errorf("run() error = %v", err)
					}
				case <-time.After(guard):
					t.Error("gateway did not shut down")
				}
			})
			select {
			case <-listening.addr:
			case <-time.After(guard):
				t.Fatal("gateway did not start")
			}
			addrs := listenAddrs.FindStringSubmatch(logs.String())
			if len(addrs) != 3 {
				t.Fatal("startup log did not include both listener addresses")
			}
			api, metricsURL := "http://"+addrs[1], "http://"+addrs[2]
			client := &http.Client{Timeout: guard}
			for _, path := range []string{
				"/debug/pprof", "/debug/pprof/", "/debug/pprof/heap", "/debug/pprof/allocs",
				"/debug/pprof/goroutine?debug=1", "/debug/pprof/block", "/debug/pprof/mutex",
				"/debug/pprof/threadcreate", "/debug/pprof/cmdline", "/debug/pprof/symbol",
				"/debug/pprof/profile?seconds=1", "/debug/pprof/trace?seconds=0.01",
			} {
				get := func(url string) (*http.Response, []byte) {
					t.Helper()
					resp, err := client.Get(url)
					if err != nil {
						t.Fatal(err)
					}
					body, err := io.ReadAll(resp.Body)
					resp.Body.Close()
					if err != nil {
						t.Fatal(err)
					}
					return resp, body
				}
				resp, _ := get(api + path)
				if resp.StatusCode != http.StatusNotFound {
					t.Errorf("API %s status = %d, want 404", path, resp.StatusCode)
				}
				resp, body := get(metricsURL + path)
				want := http.StatusNotFound
				if setting == "true" {
					want = http.StatusOK
				}
				if resp.StatusCode != want {
					t.Errorf("metrics %s status = %d, want %d", path, resp.StatusCode, want)
				}
				if setting == "true" && len(body) == 0 {
					t.Errorf("enabled pprof %s returned no data", path)
				}
			}
			// The application path is unchanged: no key still gets 401.
			resp, _, err := postChatAs(t, t.Context(), api, "", chatBody("local"))
			if err != nil || resp.StatusCode != http.StatusUnauthorized {
				t.Fatalf("chat request = %v, %v; want 401", resp, err)
			}
			resp, err = client.Get(metricsURL + "/metrics")
			if err != nil {
				t.Fatal(err)
			}
			body, err := io.ReadAll(resp.Body)
			resp.Body.Close()
			if err != nil || resp.StatusCode != http.StatusOK || !strings.Contains(string(body), "gateway_requests_total") {
				t.Fatalf("metrics scrape = %d, %v; want existing metrics", resp.StatusCode, err)
			}
			if setting == "true" {
				resp, err := client.Post(metricsURL+"/debug/pprof/profile", "text/plain", nil)
				if err != nil {
					t.Fatal(err)
				}
				resp.Body.Close()
				if resp.StatusCode != http.StatusMethodNotAllowed {
					t.Errorf("POST pprof = %d, want 405", resp.StatusCode)
				}
			}
		})
	}
}

func TestPprofExtendsWriteDeadlineForTimedProfiles(t *testing.T) {
	// CPU capture must outlast the ordinary metrics write timeout. Go's
	// pprof handler extends it by the requested duration; keep that behavior.
	srv := newMetricsServer(http.NotFoundHandler(), true, slog.New(slog.DiscardHandler))
	srv.WriteTimeout = 500 * time.Millisecond
	ts := httptest.NewUnstartedServer(srv.Handler)
	ts.Config = srv
	ts.Start()
	t.Cleanup(ts.Close)
	client := ts.Client()
	client.Timeout = guard
	resp, err := client.Get(ts.URL + "/debug/pprof/profile?seconds=1")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK || resp.Header.Get("Content-Type") != "application/octet-stream" {
		t.Fatalf("CPU profile response = %d, %v", resp.StatusCode, resp.Header)
	}
	z, err := gzip.NewReader(bytes.NewReader(body))
	if err != nil {
		t.Fatalf("CPU profile is not gzip data: %v", err)
	}
	defer z.Close()
	profile, err := io.ReadAll(z)
	if err != nil || len(profile) == 0 {
		t.Fatalf("CPU profile payload = %d bytes, %v", len(profile), err)
	}
}

func TestPprofShutdownCancelsLongCapture(t *testing.T) {
	srv := newMetricsServer(http.NotFoundHandler(), true, slog.New(slog.DiscardHandler))
	entered, exited := make(chan struct{}), make(chan struct{})
	h := srv.Handler
	srv.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(entered)
		defer close(exited)
		h.ServeHTTP(w, r)
	})
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	stopped := make(chan struct{})
	var serveErr error
	go func() {
		serveErr = serve(ctx, srv, ln, 100*time.Millisecond)
		close(stopped)
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case <-stopped:
		case <-time.After(guard):
			t.Error("profiling server did not stop")
		}
	})
	requestCtx, stopRequest := context.WithTimeout(t.Context(), guard)
	defer stopRequest()
	req, err := http.NewRequestWithContext(requestCtx, http.MethodGet, "http://"+ln.Addr().String()+"/debug/pprof/profile?seconds=60", nil)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() {
		if resp, err := http.DefaultClient.Do(req); err == nil {
			resp.Body.Close()
		}
		close(done)
	}()
	select {
	case <-entered:
	case <-time.After(guard):
		t.Fatal("CPU request did not reach the profiling server")
	}
	cancel()
	select {
	case <-stopped:
		if !errors.Is(serveErr, context.DeadlineExceeded) {
			t.Errorf("serve() error = %v, want forced shutdown deadline", serveErr)
		}
	case <-time.After(guard):
		t.Fatal("long capture prevented bounded shutdown")
	}
	select {
	case <-exited:
	case <-time.After(guard):
		t.Fatal("capture kept running after its connection was closed")
	}
	select {
	case <-done:
	case <-time.After(guard):
		t.Fatal("profile client did not return after shutdown")
	}
}
