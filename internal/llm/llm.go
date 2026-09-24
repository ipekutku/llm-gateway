// Package llm defines the vendor-neutral chat contract shared by the HTTP
// layer, the router, and provider adapters. It imports no other project
// package.
package llm

import "context"

// Message roles.
const (
	RoleSystem    = "system"
	RoleUser      = "user"
	RoleAssistant = "assistant"
)

// Finish reasons.
const (
	FinishReasonStop          = "stop"
	FinishReasonLength        = "length"
	FinishReasonContentFilter = "content_filter"
)

// Message is a single conversation turn.
type Message struct {
	Role    string
	Content string
}

// ChatRequest is a validated, normalized chat completion request.
type ChatRequest struct {
	Model     string
	Messages  []Message
	MaxTokens int
}

// Usage holds the token counts reported by the upstream provider.
type Usage struct {
	InputTokens  int
	OutputTokens int
}

// ChatResponse is a single assistant completion.
type ChatResponse struct {
	Model        string
	Message      Message
	FinishReason string
	Usage        Usage
}

// Provider completes chat requests. Implementations must honor ctx
// cancellation, support concurrent calls, and must not mutate req or its
// Messages slice.
type Provider interface {
	Chat(ctx context.Context, req ChatRequest) (ChatResponse, error)
}
