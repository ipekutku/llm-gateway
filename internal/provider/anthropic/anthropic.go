// Package anthropic implements llm.Provider using the Anthropic Messages
// API. Wire types are private to this package.
package anthropic

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"

	"github.com/ipekutku/llm-gateway/internal/llm"
)

const (
	// ProviderName identifies this provider in errors and logs.
	ProviderName = "anthropic"

	// DefaultBaseURL is the production API origin.
	DefaultBaseURL = "https://api.anthropic.com"

	// APIVersion is the pinned anthropic-version header value.
	APIVersion = "2023-06-01"

	endpointPath = "/v1/messages"

	// maxResponseBytes bounds how much of an upstream response is read.
	maxResponseBytes = 4 << 20
)

// Client calls the Anthropic Messages API. It is safe for concurrent use.
type Client struct {
	apiKey   string
	endpoint string
	http     *http.Client
}

var _ llm.Provider = (*Client)(nil)

// New returns a Client. baseURL is an http or https origin with an optional
// path prefix and without the endpoint suffix, for example
// DefaultBaseURL; /v1/messages is appended to it. A nil httpClient uses
// http.DefaultClient.
func New(apiKey, baseURL string, httpClient *http.Client) (*Client, error) {
	if strings.TrimSpace(apiKey) == "" {
		return nil, errors.New("anthropic: API key is required")
	}
	endpoint, err := endpointURL(baseURL)
	if err != nil {
		return nil, fmt.Errorf("anthropic: %w", err)
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

type messagesRequest struct {
	Model     string    `json:"model"`
	System    string    `json:"system,omitempty"`
	Messages  []message `json:"messages"`
	MaxTokens int       `json:"max_tokens"`
}

type message struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type messagesResponse struct {
	Type       string         `json:"type"`
	Role       string         `json:"role"`
	Model      string         `json:"model"`
	Content    []contentBlock `json:"content"`
	StopReason string         `json:"stop_reason"`
	Usage      *usage         `json:"usage"`
}

type contentBlock struct {
	Type string  `json:"type"`
	Text *string `json:"text"`
}

type usage struct {
	InputTokens              *int `json:"input_tokens"`
	OutputTokens             *int `json:"output_tokens"`
	CacheCreationInputTokens int  `json:"cache_creation_input_tokens"`
	CacheReadInputTokens     int  `json:"cache_read_input_tokens"`
}

// Chat sends req to the Messages endpoint. Every upstream failure,
// including cancellation, is returned as an *llm.ProviderError. A request
// the gateway should never have produced returns a plain error.
func (c *Client) Chat(ctx context.Context, req llm.ChatRequest) (llm.ChatResponse, error) {
	body, err := encodeRequest(req)
	if err != nil {
		return llm.ChatResponse{}, err
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.endpoint, bytes.NewReader(body))
	if err != nil {
		return llm.ChatResponse{}, fmt.Errorf("anthropic: build request: %w", err)
	}
	httpReq.Header.Set("x-api-key", c.apiKey)
	httpReq.Header.Set("anthropic-version", APIVersion)
	httpReq.Header.Set("Content-Type", "application/json")

	resp, err := c.http.Do(httpReq)
	if err != nil {
		return llm.ChatResponse{}, upstreamError(ctx, 0, fmt.Errorf("send request: %w", err))
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		// The body is not read: it may echo request content and must not
		// reach errors or logs.
		return llm.ChatResponse{}, upstreamError(ctx, resp.StatusCode, errors.New("unexpected status"))
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

// encodeRequest moves an optional leading system message into the
// top-level system field and forwards the remaining messages in order.
func encodeRequest(req llm.ChatRequest) ([]byte, error) {
	if strings.TrimSpace(req.Model) == "" {
		return nil, errors.New("anthropic: invalid request: blank model")
	}
	if req.MaxTokens <= 0 {
		return nil, errors.New("anthropic: invalid request: max tokens must be positive")
	}

	in := req.Messages
	var system string
	if len(in) > 0 && in[0].Role == llm.RoleSystem {
		system = in[0].Content
		in = in[1:]
	}
	if len(in) == 0 {
		return nil, errors.New("anthropic: invalid request: no conversation messages")
	}
	messages := make([]message, len(in))
	for i, m := range in {
		switch m.Role {
		case llm.RoleUser, llm.RoleAssistant:
		case llm.RoleSystem:
			return nil, errors.New("anthropic: invalid request: system message after the first message")
		default:
			return nil, fmt.Errorf("anthropic: invalid request: unsupported role %q", m.Role)
		}
		messages[i] = message{Role: m.Role, Content: m.Content}
	}

	body, err := json.Marshal(messagesRequest{
		Model:     req.Model,
		System:    system,
		Messages:  messages,
		MaxTokens: req.MaxTokens,
	})
	if err != nil {
		return nil, fmt.Errorf("anthropic: encode request: %w", err)
	}
	return body, nil
}

// decodeResponse translates a successful response body. Text blocks are
// concatenated in order without separators. Thinking blocks are reasoning,
// not output, and are skipped. Any other block type, or a response without
// a known stop reason and complete usage, is a protocol error.
func decodeResponse(data []byte) (llm.ChatResponse, error) {
	var resp messagesResponse
	if err := json.Unmarshal(data, &resp); err != nil {
		return llm.ChatResponse{}, errors.New("malformed response JSON")
	}
	if resp.Type != "message" {
		return llm.ChatResponse{}, fmt.Errorf("response has type %q, want message", resp.Type)
	}
	if resp.Role != llm.RoleAssistant {
		return llm.ChatResponse{}, fmt.Errorf("response has role %q, want assistant", resp.Role)
	}
	if resp.Content == nil {
		return llm.ChatResponse{}, errors.New("response has no content")
	}

	var text strings.Builder
	for i, block := range resp.Content {
		switch block.Type {
		case "text":
			if block.Text == nil {
				return llm.ChatResponse{}, fmt.Errorf("content[%d] text block has no text", i)
			}
			text.WriteString(*block.Text)
		case "thinking", "redacted_thinking":
		default:
			return llm.ChatResponse{}, fmt.Errorf("content[%d] has unsupported type %q", i, block.Type)
		}
	}

	finish, err := finishReason(resp.StopReason)
	if err != nil {
		return llm.ChatResponse{}, err
	}

	if resp.Usage == nil || resp.Usage.InputTokens == nil || resp.Usage.OutputTokens == nil {
		return llm.ChatResponse{}, errors.New("response has no token usage")
	}
	u := resp.Usage

	return llm.ChatResponse{
		Model:        resp.Model,
		Message:      llm.Message{Role: llm.RoleAssistant, Content: text.String()},
		FinishReason: finish,
		Usage: llm.Usage{
			// input_tokens excludes cached tokens; the neutral count is the
			// whole prompt, as with OpenAI's prompt_tokens.
			InputTokens:  *u.InputTokens + u.CacheCreationInputTokens + u.CacheReadInputTokens,
			OutputTokens: *u.OutputTokens,
		},
	}, nil
}

// finishReason maps the documented stop reasons for API version
// 2023-06-01. tool_use and pause_turn belong to tool loops, which the
// gateway does not support; they and any undocumented reason are protocol
// errors.
func finishReason(reason string) (string, error) {
	switch reason {
	case "end_turn", "stop_sequence":
		return llm.FinishReasonStop, nil
	case "max_tokens", "model_context_window_exceeded":
		return llm.FinishReasonLength, nil
	case "refusal":
		return llm.FinishReasonContentFilter, nil
	default:
		return "", fmt.Errorf("unsupported stop reason %q", reason)
	}
}

// upstreamError wraps cause in an *llm.ProviderError. If ctx is done, the
// context error is wrapped as well, so callers can detect cancellation even
// when the transport reports it differently.
func upstreamError(ctx context.Context, status int, cause error) error {
	if ctxErr := ctx.Err(); ctxErr != nil && !errors.Is(cause, ctxErr) {
		cause = fmt.Errorf("%w: %w", ctxErr, cause)
	}
	return &llm.ProviderError{Provider: ProviderName, StatusCode: status, Err: cause}
}
