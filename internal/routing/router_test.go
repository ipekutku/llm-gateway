package routing

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/ipekutku/llm-gateway/internal/llm"
)

// providerFunc adapts a function to llm.Provider.
type providerFunc func(ctx context.Context, req llm.ChatRequest) (llm.ChatResponse, error)

func (f providerFunc) Chat(ctx context.Context, req llm.ChatRequest) (llm.ChatResponse, error) {
	return f(ctx, req)
}

// replyWith returns a provider that answers with content and counts its calls.
func replyWith(content string, calls *int) llm.Provider {
	return providerFunc(func(ctx context.Context, req llm.ChatRequest) (llm.ChatResponse, error) {
		*calls++
		return llm.ChatResponse{
			Model:   req.Model,
			Message: llm.Message{Role: llm.RoleAssistant, Content: content},
		}, nil
	})
}

func mustNew(t *testing.T, routes map[string]llm.Provider) *Router {
	t.Helper()
	r, err := New(routes)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	return r
}

func TestRouterSelectsProviderByModel(t *testing.T) {
	wantA := llm.ChatResponse{
		Model:        "model-a-resolved",
		Message:      llm.Message{Role: llm.RoleAssistant, Content: "from A"},
		FinishReason: llm.FinishReasonStop,
		Usage:        llm.Usage{InputTokens: 12, OutputTokens: 8},
	}
	wantB := llm.ChatResponse{
		Model:        "model-b-resolved",
		Message:      llm.Message{Role: llm.RoleAssistant, Content: "from B"},
		FinishReason: llm.FinishReasonLength,
		Usage:        llm.Usage{InputTokens: 20, OutputTokens: 100},
	}
	var callsA, callsB int
	r := mustNew(t, map[string]llm.Provider{
		"model-a": providerFunc(func(context.Context, llm.ChatRequest) (llm.ChatResponse, error) {
			callsA++
			return wantA, nil
		}),
		"model-b": providerFunc(func(context.Context, llm.ChatRequest) (llm.ChatResponse, error) {
			callsB++
			return wantB, nil
		}),
	})

	tests := []struct {
		model string
		want  llm.ChatResponse
	}{
		{"model-a", wantA},
		{"model-b", wantB},
	}
	for _, tt := range tests {
		t.Run(tt.model, func(t *testing.T) {
			resp, err := r.Chat(context.Background(), llm.ChatRequest{Model: tt.model})
			if err != nil {
				t.Fatalf("Chat() error = %v", err)
			}
			if resp != tt.want {
				t.Errorf("Chat() response = %+v, want %+v", resp, tt.want)
			}
		})
	}
	if callsA != 1 || callsB != 1 {
		t.Errorf("calls = (A %d, B %d), want (1, 1)", callsA, callsB)
	}
}

func TestRouterUnknownModel(t *testing.T) {
	var calls int
	r := mustNew(t, map[string]llm.Provider{"model-a": replyWith("", &calls)})

	for _, model := range []string{"model-x", "", "MODEL-A", " model-a"} {
		t.Run(model, func(t *testing.T) {
			_, err := r.Chat(context.Background(), llm.ChatRequest{Model: model})
			if !errors.Is(err, llm.ErrUnknownModel) {
				t.Errorf("Chat() error = %v, want wrapping %v", err, llm.ErrUnknownModel)
			}
		})
	}
	if calls != 0 {
		t.Errorf("provider called %d times, want 0", calls)
	}
}

func TestRouterForwardsContextAndRequestUnchanged(t *testing.T) {
	type ctxKey struct{}
	ctx := context.WithValue(context.Background(), ctxKey{}, "marker")
	req := llm.ChatRequest{
		Model: "model-a",
		Messages: []llm.Message{
			{Role: llm.RoleSystem, Content: "Answer concisely."},
			{Role: llm.RoleUser, Content: "Explain TCP."},
		},
		MaxTokens: 100,
	}

	var gotCtx context.Context
	var gotReq llm.ChatRequest
	r := mustNew(t, map[string]llm.Provider{
		"model-a": providerFunc(func(ctx context.Context, req llm.ChatRequest) (llm.ChatResponse, error) {
			gotCtx, gotReq = ctx, req
			return llm.ChatResponse{}, nil
		}),
	})

	if _, err := r.Chat(ctx, req); err != nil {
		t.Fatalf("Chat() error = %v", err)
	}
	if gotCtx != ctx {
		t.Error("provider received a different context")
	}
	if !reflect.DeepEqual(gotReq, req) {
		t.Errorf("provider received %+v, want %+v", gotReq, req)
	}
}

func TestRouterReturnsProviderErrorsUnchanged(t *testing.T) {
	providerErr := &llm.ProviderError{Provider: "openai", StatusCode: 429}
	r := mustNew(t, map[string]llm.Provider{
		"model-a": providerFunc(func(context.Context, llm.ChatRequest) (llm.ChatResponse, error) {
			return llm.ChatResponse{}, providerErr
		}),
		"model-b": providerFunc(func(context.Context, llm.ChatRequest) (llm.ChatResponse, error) {
			return llm.ChatResponse{}, &llm.ProviderError{Provider: "anthropic", Err: context.Canceled}
		}),
	})

	_, err := r.Chat(context.Background(), llm.ChatRequest{Model: "model-a"})
	var pe *llm.ProviderError
	if !errors.As(err, &pe) || pe != providerErr {
		t.Errorf("Chat() error = %v, want %v", err, providerErr)
	}
	if errors.Is(err, llm.ErrUnknownModel) {
		t.Error("provider error must not match ErrUnknownModel")
	}

	_, err = r.Chat(context.Background(), llm.ChatRequest{Model: "model-b"})
	if !errors.Is(err, context.Canceled) {
		t.Errorf("Chat() error = %v, want wrapping %v", err, context.Canceled)
	}
}

func TestNewRejectsInvalidRoutes(t *testing.T) {
	var calls int
	valid := replyWith("", &calls)

	tests := []struct {
		name   string
		routes map[string]llm.Provider
	}{
		{"nil map", nil},
		{"empty map", map[string]llm.Provider{}},
		{"empty model", map[string]llm.Provider{"": valid}},
		{"whitespace model", map[string]llm.Provider{" \t": valid}},
		{"nil provider", map[string]llm.Provider{"model-a": valid, "model-b": nil}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r, err := New(tt.routes)
			if err == nil {
				t.Fatal("New() error = nil, want error")
			}
			if r != nil {
				t.Errorf("New() router = %v, want nil", r)
			}
		})
	}
}

func TestNewCopiesRoutes(t *testing.T) {
	var callsA, callsB int
	routes := map[string]llm.Provider{"model-a": replyWith("from A", &callsA)}
	r := mustNew(t, routes)

	delete(routes, "model-a")
	routes["model-b"] = replyWith("from B", &callsB)

	if _, err := r.Chat(context.Background(), llm.ChatRequest{Model: "model-a"}); err != nil {
		t.Errorf("model-a after caller delete: error = %v, want nil", err)
	}
	if _, err := r.Chat(context.Background(), llm.ChatRequest{Model: "model-b"}); !errors.Is(err, llm.ErrUnknownModel) {
		t.Errorf("model-b after caller insert: error = %v, want %v", err, llm.ErrUnknownModel)
	}
	if callsB != 0 {
		t.Errorf("provider B called %d times, want 0", callsB)
	}
}
