package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/ipekutku/llm-gateway/internal/llm"
	"github.com/ipekutku/llm-gateway/internal/provider/anthropic"
	"github.com/ipekutku/llm-gateway/internal/provider/openai"
)

const guard = 5 * time.Second

func TestFakeProviderAdapters(t *testing.T) {
	fake := newFakeProvider(0)
	srv := httptest.NewUnstartedServer(fake.handler)
	srv.Config.ConnState = fake.connState
	srv.Start()
	t.Cleanup(srv.Close)
	oa, err := openai.New("synthetic-openai-key", srv.URL, srv.Client())
	if err != nil {
		t.Fatal(err)
	}
	an, err := anthropic.New("synthetic-anthropic-key", srv.URL, srv.Client())
	if err != nil {
		t.Fatal(err)
	}
	for _, tt := range []struct {
		name     string
		provider llm.Provider
	}{
		{"openai", oa},
		{"anthropic", an},
	} {
		t.Run(tt.name, func(t *testing.T) {
			for _, model := range []string{"local-model", "local-\"model\n"} {
				ctx, cancel := context.WithTimeout(t.Context(), guard)
				defer cancel()
				got, err := tt.provider.Chat(ctx, llm.ChatRequest{
					Model: model, Messages: []llm.Message{{Role: llm.RoleUser, Content: "synthetic prompt"}}, MaxTokens: 32,
				})
				if err != nil {
					t.Fatal(err)
				}
				want := llm.ChatResponse{
					Provider: tt.name, Model: model,
					Message:      llm.Message{Role: llm.RoleAssistant, Content: completionText},
					FinishReason: llm.FinishReasonStop, Usage: llm.Usage{InputTokens: 12, OutputTokens: 3},
				}
				if got != want {
					t.Errorf("Chat() = %+v, want %+v", got, want)
				}
			}
		})
	}
	if got := fake.connections.Load(); got != 1 {
		t.Errorf("connections = %d, want 1 for four sequential keep-alive calls", got)
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.DisableKeepAlives = true
	t.Cleanup(transport.CloseIdleConnections)
	noReuse, err := openai.New("synthetic-key", srv.URL, &http.Client{Transport: transport})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), guard)
	defer cancel()
	for range 2 {
		_, err := noReuse.Chat(ctx, llm.ChatRequest{Model: "local", Messages: []llm.Message{{Role: llm.RoleUser, Content: "hi"}}, MaxTokens: 32})
		if err != nil {
			t.Fatal(err)
		}
	}
	if got := fake.connections.Load(); got != 3 {
		t.Errorf("connections = %d, want 3 after two calls without keep-alive", got)
	}
}

func TestFakeProviderWireResponses(t *testing.T) {
	for _, path := range []string{"/v1/chat/completions", "/v1/messages"} {
		r := httptest.NewRequest(http.MethodPost, path, strings.NewReader(`{"model":"local-model","messages":[]}`))
		w := httptest.NewRecorder()
		newFakeProvider(0).handler.ServeHTTP(w, r)
		if w.Code != http.StatusOK || w.Header().Get("Content-Type") != "application/json" {
			t.Fatalf("response = %d, %v", w.Code, w.Header())
		}
		var body map[string]json.RawMessage
		if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
			t.Fatal(err)
		}
		if string(body["model"]) != `"local-model"` || len(body["usage"]) == 0 {
			t.Errorf("model or usage missing: %s", w.Body)
		}
		// Identical requests must produce identical bytes, with no random IDs.
		second := httptest.NewRecorder()
		newFakeProvider(0).handler.ServeHTTP(second, httptest.NewRequest(http.MethodPost, path, strings.NewReader(`{"model":"local-model"}`)))
		if second.Body.String() != w.Body.String() {
			t.Error("fixture response is not deterministic")
		}
	}
}

func TestFakeProviderConcurrentRequests(t *testing.T) {
	fake := newFakeProvider(0)
	srv := httptest.NewUnstartedServer(fake.handler)
	srv.Config.ConnState = fake.connState
	srv.Start()
	t.Cleanup(srv.Close)
	oa, err := openai.New("synthetic-key", srv.URL, srv.Client())
	if err != nil {
		t.Fatal(err)
	}
	an, err := anthropic.New("synthetic-key", srv.URL, srv.Client())
	if err != nil {
		t.Fatal(err)
	}
	for i := range 32 {
		t.Run(fmt.Sprintf("request-%d", i), func(t *testing.T) {
			t.Parallel()
			var provider llm.Provider = oa
			if i%2 != 0 {
				provider = an
			}
			ctx, cancel := context.WithTimeout(t.Context(), guard)
			defer cancel()
			model := fmt.Sprintf("local-model-%d", i)
			got, err := provider.Chat(ctx, llm.ChatRequest{Model: model, Messages: []llm.Message{{Role: llm.RoleUser, Content: "hi"}}, MaxTokens: 32})
			if err != nil {
				t.Fatal(err)
			}
			if got.Model != model || got.Message.Content != completionText || got.Usage != (llm.Usage{InputTokens: 12, OutputTokens: 3}) {
				t.Errorf("concurrent request response = %+v", got)
			}
		})
	}
}

func TestFakeProviderRejectsInvalidRequests(t *testing.T) {
	for _, path := range []string{"/v1/chat/completions", "/v1/messages"} {
		for _, tt := range []struct {
			name, body string
			status     int
		}{
			{"empty", "", http.StatusBadRequest},
			{"malformed", `{"model":`, http.StatusBadRequest},
			{"array", `[]`, http.StatusBadRequest},
			{"null", `null`, http.StatusBadRequest},
			{"missing model", `{}`, http.StatusBadRequest},
			{"blank model", `{"model":" "}`, http.StatusBadRequest},
			{"wrong model type", `{"model":1}`, http.StatusBadRequest},
			{"streaming", `{"model":"local","stream":true}`, http.StatusBadRequest},
			{"wrong stream type", `{"model":"local","stream":"true"}`, http.StatusBadRequest},
			{"extra JSON", `{"model":"local"} {}`, http.StatusBadRequest},
			{"trailing garbage", `{"model":"local"} secret-marker`, http.StatusBadRequest},
			{"oversized body", `{"model":"local","ignored":"` + strings.Repeat("x", maxRequestBytes) + `"}`, http.StatusRequestEntityTooLarge},
			{"oversized trailing data", `{"model":"local"}` + strings.Repeat(" ", maxRequestBytes), http.StatusRequestEntityTooLarge},
		} {
			t.Run(path+"/"+tt.name, func(t *testing.T) {
				w := httptest.NewRecorder()
				newFakeProvider(0).handler.ServeHTTP(w, httptest.NewRequest(http.MethodPost, path, strings.NewReader(tt.body)))
				if w.Code != tt.status {
					t.Errorf("status = %d, want %d", w.Code, tt.status)
				}
				if strings.Contains(w.Body.String(), "secret-marker") {
					t.Error("request data leaked in the error response")
				}
			})
		}
	}
	for _, tt := range []struct {
		method, path string
		status       int
	}{
		{http.MethodGet, "/v1/chat/completions", http.StatusMethodNotAllowed},
		{http.MethodGet, "/v1/messages", http.StatusMethodNotAllowed},
		{http.MethodPost, "/unknown", http.StatusNotFound},
	} {
		w := httptest.NewRecorder()
		newFakeProvider(0).handler.ServeHTTP(w, httptest.NewRequest(tt.method, tt.path, nil))
		if w.Code != tt.status {
			t.Errorf("%s %s: status = %d, want %d", tt.method, tt.path, w.Code, tt.status)
		}
	}
}

func TestFakeProviderDelayAndCancellation(t *testing.T) {
	for _, path := range []string{"/v1/chat/completions", "/v1/messages"} {
		t.Run(path+"/delay", func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				const delay = 50 * time.Millisecond
				w := httptest.NewRecorder()
				done := make(chan struct{})
				go func() {
					newFakeProvider(delay).handler.ServeHTTP(w, httptest.NewRequest(http.MethodPost, path, strings.NewReader(`{"model":"local"}`)))
					close(done)
				}()
				synctest.Wait()
				time.Sleep(delay - time.Nanosecond) // Virtual time, not a wall-clock synchronization sleep.
				synctest.Wait()
				select {
				case <-done:
					t.Fatal("response completed before the configured delay")
				default:
				}
				time.Sleep(time.Nanosecond)
				synctest.Wait()
				select {
				case <-done:
					if w.Code != http.StatusOK || w.Body.Len() == 0 {
						t.Errorf("response after delay = %d, %s", w.Code, w.Body)
					}
				default:
					t.Fatal("response did not complete after the configured delay")
				}
			})
		})
		for _, delay := range []time.Duration{0, time.Hour} {
			t.Run(path+"/cancel/"+delay.String(), func(t *testing.T) {
				synctest.Test(t, func(t *testing.T) {
					ctx, cancel := context.WithCancel(context.Background())
					defer cancel()
					if delay == 0 {
						cancel()
					}
					w := httptest.NewRecorder()
					done := make(chan struct{})
					go func() {
						r := httptest.NewRequestWithContext(ctx, http.MethodPost, path, strings.NewReader(`{"model":"local"}`))
						newFakeProvider(delay).handler.ServeHTTP(w, r)
						close(done)
					}()
					synctest.Wait()
					cancel()
					synctest.Wait()
					select {
					case <-done:
						if w.Body.Len() != 0 || len(w.Header()) != 0 {
							t.Error("a canceled request produced a response")
						}
					default:
						t.Fatal("canceled request is still waiting for the delay")
					}
				})
			})
		}
	}
}

func TestAdapterCancellationStopsFakeHandler(t *testing.T) {
	for _, name := range []string{"openai", "anthropic"} {
		t.Run(name, func(t *testing.T) {
			entered, exited := make(chan struct{}), make(chan struct{})
			fake := newFakeProvider(time.Hour)
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				close(entered)
				fake.handler.ServeHTTP(w, r)
				close(exited)
			}))
			t.Cleanup(srv.Close)
			var provider llm.Provider
			var err error
			if name == "openai" {
				provider, err = openai.New("synthetic-key", srv.URL, srv.Client())
			} else {
				provider, err = anthropic.New("synthetic-key", srv.URL, srv.Client())
			}
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(t.Context(), guard)
			t.Cleanup(cancel)
			done := make(chan error, 1)
			go func() {
				_, err := provider.Chat(ctx, llm.ChatRequest{Model: "local", Messages: []llm.Message{{Role: llm.RoleUser, Content: "hi"}}, MaxTokens: 32})
				done <- err
			}()
			select {
			case <-entered:
			case <-time.After(guard):
				t.Fatal("adapter did not reach the fake provider")
			}
			cancel()
			select {
			case err := <-done:
				if !errors.Is(err, context.Canceled) {
					t.Errorf("Chat() error = %v, want context.Canceled", err)
				}
			case <-time.After(guard):
				t.Fatal("adapter did not return after cancellation")
			}
			select {
			case <-exited:
			case <-time.After(guard):
				t.Fatal("fake handler kept waiting after the client canceled")
			}
		})
	}
}
