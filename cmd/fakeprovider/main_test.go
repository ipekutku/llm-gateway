package main

import (
	"bytes"
	"context"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"
)

type startupOutput struct {
	mu   sync.Mutex
	buf  bytes.Buffer
	addr chan string
}

func (o *startupOutput) Write(p []byte) (int, error) {
	o.mu.Lock()
	n, err := o.buf.Write(p)
	o.mu.Unlock()
	if strings.HasPrefix(string(p), "fake provider listening ") {
		for _, field := range strings.Fields(string(p)) {
			if addr, ok := strings.CutPrefix(field, "addr="); ok {
				o.addr <- addr
			}
		}
	}
	return n, err
}

func (o *startupOutput) String() string {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.buf.String()
}

func TestRunConfiguration(t *testing.T) {
	for _, tt := range []struct {
		name string
		args []string
		want string
	}{
		{"negative delay", []string{"-delay=-1ms"}, "delay must not be negative"},
		{"invalid delay", []string{"-delay=invalid"}, "invalid"},
		{"blank address", []string{"-addr= "}, "addr must not be blank"},
		{"invalid address", []string{"-addr=127.0.0.1:bad"}, "listen:"},
		{"unknown flag", []string{"-unknown"}, "flag provided but not defined"},
		{"positional arguments", []string{"extra"}, "unexpected positional arguments"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			err := run(t.Context(), tt.args, io.Discard)
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Errorf("run() error = %v, want %q", err, tt.want)
			}
		})
	}
	var help bytes.Buffer
	if err := run(t.Context(), []string{"-help"}, &help); err != nil {
		t.Fatal(err)
	}
	for _, value := range []string{defaultAddr, "-addr", "-delay"} {
		if !strings.Contains(help.String(), value) {
			t.Errorf("help does not include %q", value)
		}
	}
}

func TestRunLifecycle(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	output := &startupOutput{addr: make(chan string, 1)}
	done := make(chan error, 1)
	go func() { done <- run(ctx, []string{"-addr=127.0.0.1:0", "-delay=1ms"}, output) }()
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Errorf("run() error = %v", err)
			}
		case <-time.After(guard):
			t.Error("fake provider did not stop")
		}
		if !strings.Contains(output.String(), "fake provider stopped connections=1") {
			t.Errorf("missing final connection count: %s", output.String())
		}
		for _, sensitive := range []string{"synthetic-key-marker", "synthetic-prompt-marker"} {
			if strings.Contains(output.String(), sensitive) {
				t.Error("request data leaked in output")
			}
		}
	})
	var addr string
	select {
	case addr = <-output.addr:
	case <-time.After(guard):
		t.Fatal("fake provider did not start")
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	t.Cleanup(transport.CloseIdleConnections)
	client := &http.Client{Transport: transport, Timeout: guard}
	for _, path := range []string{"/v1/chat/completions", "/v1/messages"} {
		req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, "http://"+addr+path,
			strings.NewReader(`{"model":"local","messages":[{"role":"user","content":"synthetic-prompt-marker"}]}`))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Authorization", "Bearer synthetic-key-marker")
		req.Header.Set("X-Api-Key", "synthetic-key-marker")
		resp, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_, readErr := io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		if readErr != nil || resp.StatusCode != http.StatusOK {
			t.Fatalf("response = %d, %v", resp.StatusCode, readErr)
		}
	}
}

func TestRunRejectsOccupiedAddress(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	err = run(t.Context(), []string{"-addr=" + ln.Addr().String()}, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "listen:") {
		t.Errorf("run() error = %v, want listen failure", err)
	}
}
