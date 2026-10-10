package main

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"strings"
	"testing"
	"time"

	"go.opentelemetry.io/otel/trace"
)

// listeningHandler reports the bound API address without polling the logs.
type listeningHandler struct {
	slog.Handler
	addr chan string
}

func (h *listeningHandler) Handle(ctx context.Context, r slog.Record) error {
	if err := h.Handler.Handle(ctx, r); err != nil {
		return err
	}
	if r.Message == "gateway listening" {
		r.Attrs(func(a slog.Attr) bool {
			if a.Key == "addr" {
				h.addr <- a.Value.String()
				return false
			}
			return true
		})
	}
	return nil
}

func TestRunWithProviderBaseURLs(t *testing.T) {
	oa := newUpstream(t, "/proxy/v1/chat/completions", reply(http.StatusOK, openaiReply))
	an := newUpstream(t, "/proxy/v1/messages", reply(http.StatusOK, anthropicReply))
	vars := map[string]string{
		"OPENAI_MODEL": "gpt-4o", "OPENAI_API_KEY": openaiKey, "OPENAI_BASE_URL": oa.srv.URL + "/proxy/",
		"ANTHROPIC_MODEL": "claude-opus-5-5", "ANTHROPIC_API_KEY": anthropicKey, "ANTHROPIC_BASE_URL": an.srv.URL + "/proxy",
		"GATEWAY_ADDR": "127.0.0.1:0", "GATEWAY_METRICS_ADDR": "127.0.0.1:0",
		clientsFileVar: clientsPath, databaseURLVar: testDatabaseURL, pricingFileVar: pricingPath,
	}
	var logs syncBuffer
	listening := &listeningHandler{Handler: slog.NewTextHandler(&logs, nil), addr: make(chan string, 1)}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() {
		done <- runWith(ctx, env(vars), files(map[string]string{clientsPath: testClientsFile, pricingPath: `{"prices":[]}`}),
			func(context.Context, string, trace.TracerProvider) (accountingStore, error) {
				return &fakeStartupStore{}, nil
			}, slog.New(listening))
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Errorf("runWith() error = %v", err)
			}
		case <-time.After(guard):
			t.Error("gateway did not shut down")
		}
	})
	var addr string
	select {
	case addr = <-listening.addr:
	case <-time.After(guard):
		t.Fatal("gateway did not start")
	}
	requestCtx, stop := context.WithTimeout(t.Context(), guard)
	defer stop()
	for _, tt := range []struct {
		model, content, path, authorization, apiKey string
		upstream                                    *upstream
	}{
		{"gpt-4o", "from openai", "/proxy/v1/chat/completions", "Bearer " + openaiKey, "", oa},
		{"claude-opus-5-5", "from anthropic", "/proxy/v1/messages", "", anthropicKey, an},
	} {
		resp, body, err := postChat(t, requestCtx, "http://"+addr, chatBody(tt.model))
		if err != nil {
			t.Fatal(err)
		}
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("%s status = %d, want 200 (body %s)", tt.model, resp.StatusCode, body)
		}
		var completion struct {
			Choices []struct {
				Message struct{ Content string }
			}
		}
		if err := json.Unmarshal(body, &completion); err != nil {
			t.Fatal(err)
		}
		if len(completion.Choices) != 1 || completion.Choices[0].Message.Content != tt.content {
			t.Fatalf("%s completion = %s", tt.model, body)
		}
		requests := tt.upstream.received()
		if len(requests) != 1 {
			t.Fatalf("%s upstream received %d requests, want 1", tt.model, len(requests))
		}
		r := requests[0]
		if r.Path != tt.path || r.Body["model"] != tt.model {
			t.Errorf("upstream path/model = %q/%v, want %q/%q", r.Path, r.Body["model"], tt.path, tt.model)
		}
		if r.Header.Get("Authorization") != tt.authorization || r.Header.Get("X-Api-Key") != tt.apiKey {
			t.Error("upstream did not receive only its own provider credentials")
		}
	}
	for _, value := range []string{oa.srv.URL, an.srv.URL, openaiKey, anthropicKey, clientKey} {
		if strings.Contains(logs.String(), value) {
			t.Error("provider URL or credential leaked in logs")
		}
	}
}

func TestRunWithRejectsInvalidProviderURLBeforeOpeningDatabase(t *testing.T) {
	const rawURL = "https://synthetic-user:synthetic-password@upstream.example/%invalid"
	opened := false
	err := runWith(t.Context(), env(map[string]string{
		"OPENAI_MODEL": "gpt-4o", "OPENAI_API_KEY": openaiKey, "OPENAI_BASE_URL": rawURL,
		clientsFileVar: clientsPath, databaseURLVar: testDatabaseURL, pricingFileVar: pricingPath,
	}), files(map[string]string{clientsPath: testClientsFile, pricingPath: `{"prices":[]}`}),
		func(context.Context, string, trace.TracerProvider) (accountingStore, error) {
			opened = true
			return &fakeStartupStore{}, nil
		}, slog.New(slog.DiscardHandler))
	if opened {
		t.Error("database opened before rejecting an invalid provider URL")
	}
	if err == nil || !strings.Contains(err.Error(), "OPENAI_BASE_URL") {
		t.Fatalf("runWith() error = %v, want base URL validation error", err)
	}
	if strings.Contains(err.Error(), rawURL) || strings.Contains(err.Error(), "synthetic-") {
		t.Errorf("URL value leaked in error: %v", err)
	}
}
