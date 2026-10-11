package main

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// Two synchronized waves prove reuse without timing assertions or a paid
// origin. Warm HTTP/2 negotiation first; speculative parallel dials may still
// inflate the first wave. The second wave measures established reuse.
// The transport is the real, unchanged gateway client. Reading a body to EOF
// returns its connection to the idle pool before the read returns, so the
// second wave starts with every first-wave connection available.
func TestUpstreamConnectionReuse(t *testing.T) {
	for _, h2 := range []bool{false, true} {
		t.Run(fmt.Sprintf("http2=%t", h2), func(t *testing.T) {
			const workers = 500
			if workers > maxIdleConnsPerHost {
				t.Fatalf("%d workers exceed the idle pool of %d", workers, maxIdleConnsPerHost)
			}
			ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
			defer cancel()
			var accepted, closed, arrivals atomic.Int64
			var protocolBad atomic.Bool
			all := make(chan struct{}, 2)
			gates := []chan struct{}{make(chan struct{}), make(chan struct{})}
			s := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				want := 1
				if h2 {
					want = 2
				}
				if r.ProtoMajor != want {
					protocolBad.Store(true)
				}
				if r.URL.Path != "/warm" {
					n := arrivals.Add(1)
					wave := (n - 1) / workers
					if n%workers == 0 {
						all <- struct{}{}
					}
					select {
					case <-gates[wave]:
					case <-ctx.Done():
						return
					case <-r.Context().Done():
						return
					}
				}
				_, _ = io.WriteString(w, "synthetic")
			}))
			s.EnableHTTP2 = h2
			s.Config.ConnState = func(_ net.Conn, state http.ConnState) {
				if state == http.StateNew {
					accepted.Add(1)
				}
				if state == http.StateClosed {
					closed.Add(1)
				}
			}
			if h2 {
				s.StartTLS()
			} else {
				s.Start()
			}
			t.Cleanup(s.Close)
			// Cancellation must unblock handlers before httptest waits for them.
			t.Cleanup(cancel)
			client := newUpstreamClient(config{ConnectTimeout: 10 * time.Second})
			transport := client.Transport.(*http.Transport)
			if h2 {
				transport.TLSClientConfig = s.Client().Transport.(*http.Transport).TLSClientConfig.Clone()
			}
			t.Cleanup(client.CloseIdleConnections)
			call := func(path string) error {
				req, err := http.NewRequestWithContext(ctx, http.MethodGet, s.URL+path, nil)
				if err != nil {
					return err
				}
				res, err := client.Do(req)
				if err != nil {
					return err
				}
				defer res.Body.Close()
				_, err = io.Copy(io.Discard, res.Body)
				if res.StatusCode != http.StatusOK {
					return fmt.Errorf("status %d", res.StatusCode)
				}
				return err
			}
			if err := call("/warm"); err != nil {
				t.Fatal(err)
			}
			var counts [2]int64
			for wave := range gates {
				start := accepted.Load()
				results := make(chan error, workers)
				for range workers {
					go func() { results <- call("/wave") }()
				}
				select {
				case <-all:
				case <-ctx.Done():
					t.Fatal("concurrent wave did not reach upstream")
				}
				close(gates[wave])
				for range workers {
					if err := <-results; err != nil {
						t.Fatal(err)
					}
				}
				counts[wave] = accepted.Load() - start
			}
			if protocolBad.Load() {
				t.Fatal("unexpected negotiated protocol")
			}
			if counts[1] != 0 {
				t.Fatalf("second wave opened %d new connections, want reuse of all %d", counts[1], workers)
			}
			if !h2 && closed.Load() != 0 {
				t.Fatalf("idle pool closed %d HTTP/1.1 connections", closed.Load())
			}
			t.Logf("workers=%d new connections per wave=%v total=%d (includes warmup and speculative dials); idle limit=%d per host", workers, counts, accepted.Load(), transport.MaxIdleConnsPerHost)
		})
	}
}

// Use the production server settings with the same keep-alive behavior as
// Vegeta. The counters instrument only this test, not production telemetry.
func TestInboundConnectionReuse(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()
	var accepted, arrivals atomic.Int64
	all := make(chan struct{}, 2)
	gates := []chan struct{}{make(chan struct{}), make(chan struct{})}
	server := newServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := arrivals.Add(1)
		if n%500 == 0 {
			all <- struct{}{}
		}
		select {
		case <-gates[(n-1)/500]:
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
	defer s.Close()
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.MaxIdleConns = 10000
	transport.MaxIdleConnsPerHost = 10000
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport}
	for wave := range gates {
		var wg sync.WaitGroup
		for range 500 {
			wg.Go(func() {
				req, _ := http.NewRequestWithContext(ctx, http.MethodGet, s.URL, nil)
				res, err := client.Do(req)
				if err != nil {
					t.Error(err)
					return
				}
				defer res.Body.Close()
				if _, err := io.Copy(io.Discard, res.Body); err != nil {
					t.Error(err)
				}
			})
		}
		select {
		case <-all:
			close(gates[wave])
		case <-ctx.Done():
			wg.Wait()
			t.Fatal("inbound wave did not arrive")
		}
		wg.Wait()
	}
	if n := accepted.Load(); n != 500 {
		t.Fatalf("keep-alive opened %d connections for 1000 calls", n)
	}
	t.Logf("1000 calls, accepted=%d; IdleTimeout=%s", accepted.Load(), server.IdleTimeout)
}
