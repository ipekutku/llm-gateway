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
	// InputTokens is the whole prompt, including cached tokens.
	InputTokens int
	// CacheReadInputTokens is the part of InputTokens read from the
	// provider's prompt cache.
	CacheReadInputTokens int
	// CacheWriteInputTokens is the part of InputTokens written to the
	// provider's prompt cache.
	CacheWriteInputTokens int
	OutputTokens          int
}

// ChatResponse is a single assistant completion.
type ChatResponse struct {
	// Provider identifies the upstream that answered, e.g. "openai", as in
	// ProviderError.
	Provider     string
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
