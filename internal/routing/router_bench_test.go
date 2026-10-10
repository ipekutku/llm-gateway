package routing

import (
	"context"
	"log/slog"
	"net/http"
	"testing"
	"time"

	"github.com/ipekutku/llm-gateway/internal/llm"
)

func BenchmarkRoute(b *testing.B) {
	ok := providerFunc(func(_ context.Context, req llm.ChatRequest) (llm.ChatResponse, error) {
		return llm.ChatResponse{Model: req.Model, Provider: "fake"}, nil
	})
	unavailable := providerFunc(func(context.Context, llm.ChatRequest) (llm.ChatResponse, error) {
		return llm.ChatResponse{}, &llm.ProviderError{Provider: "fake", StatusCode: http.StatusServiceUnavailable}
	})
	for _, tc := range []struct {
		name      string
		primary   llm.Provider
		fallback  bool
		wantModel string
	}{
		{"direct", ok, false, "model-a"},
		{"fallback_configured", ok, true, "model-a"},
		{"fallback_taken", unavailable, true, "model-b"},
	} {
		b.Run(tc.name, func(b *testing.B) {
			route := Route{Provider: tc.primary}
			if tc.fallback {
				route.Fallback = &Fallback{Model: "model-b", Provider: ok, PrimaryTimeout: time.Second}
			}
			router, err := New(map[string]Route{"model-a": route}, slog.New(slog.DiscardHandler))
			if err != nil {
				b.Fatal(err)
			}
			req := llm.ChatRequest{Model: "model-a", Messages: []llm.Message{{Role: llm.RoleUser, Content: "Explain TCP."}}, MaxTokens: 200}
			ctx := context.Background()
			b.ReportAllocs()
			for b.Loop() {
				resp, err := router.Chat(ctx, req)
				if err != nil || resp.Model != tc.wantModel {
					b.Fatalf("Chat: model %q, error %v", resp.Model, err)
				}
			}
		})
	}
}
