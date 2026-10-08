// Package openai implements llm.Provider using the OpenAI Chat Completions
// API. Wire types are private to this package.
package openai

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/ipekutku/llm-gateway/internal/llm"
)

const (
	// ProviderName identifies this provider in errors and logs.
	ProviderName = "openai"

	// DefaultBaseURL is the production API origin.
	DefaultBaseURL = "https://api.openai.com"

	endpointPath = "/v1/chat/completions"

	// maxResponseBytes bounds how much of an upstream response is read.
	maxResponseBytes = 4 << 20

	// maxRetryAfterSeconds caps a parsed Retry-After value.
	maxRetryAfterSeconds = 24 * 60 * 60
)

// Client calls the OpenAI Chat Completions API. It is safe for concurrent
// use.
type Client struct {
	apiKey   string
	endpoint string
	http     *http.Client
}

var _ llm.Provider = (*Client)(nil)

// New returns a Client. baseURL is an http or https origin with an optional
// path prefix and without the endpoint suffix, for example
// DefaultBaseURL; /v1/chat/completions is appended to it. A nil httpClient
// uses http.DefaultClient.
func New(apiKey, baseURL string, httpClient *http.Client) (*Client, error) {
	if strings.TrimSpace(apiKey) == "" {
		return nil, errors.New("openai: API key is required")
	}
	endpoint, err := endpointURL(baseURL)
	if err != nil {
		return nil, fmt.Errorf("openai: %w", err)
	}
	if httpClient == nil {
		httpClient = http.DefaultClient
	}
	return &Client{apiKey: apiKey, endpoint: endpoint, http: httpClient}, nil
}

func endpointURL(baseURL string) (string, error) {
	u, err := url.Parse(baseURL)
	if err != nil {
		return "", errors.New("invalid base URL")
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return "", errors.New("base URL must use http or https")
	}
	if u.Host == "" {
		return "", errors.New("base URL must include a host")
	}
	if u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return "", errors.New("base URL must not include credentials, a query, or a fragment")
	}
	u.Path = strings.TrimSuffix(u.Path, "/") + endpointPath
	u.RawPath = ""
	return u.String(), nil
}

// Wire types.

type chatRequest struct {
	Model               string        `json:"model"`
	Messages            []chatMessage `json:"messages"`
	MaxCompletionTokens int           `json:"max_completion_tokens"`
}

type chatMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type chatResponse struct {
	Model   string       `json:"model"`
	Choices []chatChoice `json:"choices"`
	Usage   *chatUsage   `json:"usage"`
}

type chatChoice struct {
	Message      *responseMessage `json:"message"`
	FinishReason string           `json:"finish_reason"`
}

type responseMessage struct {
	Role         string          `json:"role"`
	Content      *string         `json:"content"`
	Refusal      *string         `json:"refusal"`
	ToolCalls    json.RawMessage `json:"tool_calls"`
	FunctionCall json.RawMessage `json:"function_call"`
}

type chatUsage struct {
	PromptTokens     *int `json:"prompt_tokens"`
	CompletionTokens *int `json:"completion_tokens"`
}

// Chat sends req to the Chat Completions endpoint. Every upstream failure,
// including cancellation, is returned as an *llm.ProviderError. A request
// the gateway should never have produced returns a plain error.
func (c *Client) Chat(ctx context.Context, req llm.ChatRequest) (llm.ChatResponse, error) {
	body, err := encodeRequest(req)
	if err != nil {
		return llm.ChatResponse{}, err
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.endpoint, bytes.NewReader(body))
	if err != nil {
		return llm.ChatResponse{}, fmt.Errorf("openai: build request: %w", err)
	}
	httpReq.Header.Set("Authorization", "Bearer "+c.apiKey)
	httpReq.Header.Set("Content-Type", "application/json")

	resp, err := c.http.Do(httpReq)
	if err != nil {
		return llm.ChatResponse{}, upstreamError(ctx, 0, fmt.Errorf("send request: %w", err))
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		// The body is not read: it may echo request content and must not
		// reach errors or logs.
		pe := upstreamError(ctx, resp.StatusCode, errors.New("unexpected status"))
		pe.RetryAfter = retryAfter(resp.Header)
		return llm.ChatResponse{}, pe
	}

	data, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes+1))
	if err != nil {
		return llm.ChatResponse{}, upstreamError(ctx, resp.StatusCode, fmt.Errorf("read response: %w", err))
	}
	if len(data) > maxResponseBytes {
		return llm.ChatResponse{}, upstreamError(ctx, resp.StatusCode, errors.New("response exceeds 4 MiB"))
	}

	out, err := decodeResponse(data)
	if err != nil {
		return llm.ChatResponse{}, upstreamError(ctx, resp.StatusCode, err)
	}
	return out, nil
}

func encodeRequest(req llm.ChatRequest) ([]byte, error) {
	if strings.TrimSpace(req.Model) == "" {
		return nil, errors.New("openai: invalid request: blank model")
	}
	if req.MaxTokens <= 0 {
		return nil, errors.New("openai: invalid request: max tokens must be positive")
	}
	if len(req.Messages) == 0 {
		return nil, errors.New("openai: invalid request: no messages")
	}
	messages := make([]chatMessage, len(req.Messages))
	for i, m := range req.Messages {
		switch m.Role {
		case llm.RoleSystem, llm.RoleUser, llm.RoleAssistant:
		default:
			return nil, fmt.Errorf("openai: invalid request: unsupported role %q", m.Role)
		}
		messages[i] = chatMessage{Role: m.Role, Content: m.Content}
	}

	body, err := json.Marshal(chatRequest{
		Model:               req.Model,
		Messages:            messages,
		MaxCompletionTokens: req.MaxTokens,
	})
	if err != nil {
		return nil, fmt.Errorf("openai: encode request: %w", err)
	}
	return body, nil
}

// decodeResponse translates a successful response body. Any shape that
// does not carry exactly one assistant text completion with usage is a
// protocol error.
func decodeResponse(data []byte) (llm.ChatResponse, error) {
	var resp chatResponse
	if err := json.Unmarshal(data, &resp); err != nil {
		return llm.ChatResponse{}, errors.New("malformed response JSON")
	}
	if len(resp.Choices) != 1 {
		return llm.ChatResponse{}, fmt.Errorf("response has %d choices, want 1", len(resp.Choices))
	}
	choice := resp.Choices[0]
	msg := choice.Message
	if msg == nil {
		return llm.ChatResponse{}, errors.New("response choice has no message")
	}
	if msg.Role != llm.RoleAssistant {
		return llm.ChatResponse{}, fmt.Errorf("response message has role %q, want assistant", msg.Role)
	}
	if isPresent(msg.ToolCalls) || isPresent(msg.FunctionCall) {
		return llm.ChatResponse{}, errors.New("response contains tool calls, which are unsupported")
	}

	// A refusal arrives as a refusal string instead of content. It is
	// returned as the message text with the content_filter finish reason,
	// matching how the Anthropic adapter reports refusals.
	var text, finish string
	switch {
	case msg.Refusal != nil:
		text, finish = *msg.Refusal, llm.FinishReasonContentFilter
	case msg.Content != nil:
		var err error
		if finish, err = finishReason(choice.FinishReason); err != nil {
			return llm.ChatResponse{}, err
		}
		text = *msg.Content
	default:
		return llm.ChatResponse{}, errors.New("response message has no text content")
	}

	if resp.Usage == nil || resp.Usage.PromptTokens == nil || resp.Usage.CompletionTokens == nil {
		return llm.ChatResponse{}, errors.New("response has no token usage")
	}

	return llm.ChatResponse{
		Model:        resp.Model,
		Message:      llm.Message{Role: llm.RoleAssistant, Content: text},
		FinishReason: finish,
		Usage: llm.Usage{
			InputTokens:  *resp.Usage.PromptTokens,
			OutputTokens: *resp.Usage.CompletionTokens,
		},
	}, nil
}

func finishReason(reason string) (string, error) {
	switch reason {
	case "stop":
		return llm.FinishReasonStop, nil
	case "length":
		return llm.FinishReasonLength, nil
	case "content_filter":
		return llm.FinishReasonContentFilter, nil
	default:
		// tool_calls, function_call, and any undocumented reason.
		return "", fmt.Errorf("unsupported finish reason %q", reason)
	}
}

// isPresent reports whether an optional JSON field holds a non-null,
// non-empty value.
func isPresent(raw json.RawMessage) bool {
	s := strings.TrimSpace(string(raw))
	return s != "" && s != "null" && s != "[]"
}

// upstreamError wraps cause in an *llm.ProviderError. If ctx is done, the
// context error is wrapped as well, so callers can detect cancellation even
// when the transport reports it differently.
func upstreamError(ctx context.Context, status int, cause error) *llm.ProviderError {
	if ctxErr := ctx.Err(); ctxErr != nil && !errors.Is(cause, ctxErr) {
		cause = fmt.Errorf("%w: %w", ctxErr, cause)
	}
	return &llm.ProviderError{Provider: ProviderName, StatusCode: status, Err: cause}
}

// retryAfter parses a Retry-After header in its delay-seconds form. An
// absent, non-positive, or unparsable value, including the HTTP-date form,
// counts as no request to wait.
func retryAfter(h http.Header) time.Duration {
	secs, err := strconv.ParseInt(strings.TrimSpace(h.Get("Retry-After")), 10, 64)
	if err != nil || secs <= 0 {
		return 0
	}
	return time.Duration(min(secs, maxRetryAfterSeconds)) * time.Second
}
