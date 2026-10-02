package httpapi

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/ipekutku/llm-gateway/internal/llm"
)

const (
	// maxRequestBytes limits the size of an incoming request body.
	maxRequestBytes = 1 << 20

	// defaultMaxTokens is used when a request omits max_tokens or sets it
	// to null. Every provider receives the same normalized value.
	defaultMaxTokens = 1024
)

// parseRequest decodes and validates a request body and returns the
// normalized neutral request. Every error describes a client mistake, and
// its message is gateway-authored and safe to return to the caller.
func parseRequest(body []byte) (llm.ChatRequest, error) {
	req, err := decodeRequest(body)
	if err != nil {
		return llm.ChatRequest{}, err
	}
	return normalize(req)
}

// decodeRequest parses body as exactly one JSON object followed only by
// whitespace. Unknown fields are ignored.
func decodeRequest(body []byte) (chatRequest, error) {
	dec := json.NewDecoder(bytes.NewReader(body))

	var raw json.RawMessage
	if err := dec.Decode(&raw); err != nil {
		if errors.Is(err, io.EOF) {
			return chatRequest{}, errors.New("request body is empty")
		}
		return chatRequest{}, errors.New("request body is not valid JSON")
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return chatRequest{}, errors.New("request body must contain a single JSON object")
	}
	if raw[0] != '{' {
		return chatRequest{}, errors.New("request body must be a JSON object")
	}

	var req chatRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		if typeErr, ok := errors.AsType[*json.UnmarshalTypeError](err); ok && typeErr.Field != "" {
			return chatRequest{}, fmt.Errorf("field %q has the wrong type", typeErr.Field)
		}
		return chatRequest{}, errors.New("request body has the wrong shape")
	}
	return req, nil
}

// normalize validates req and converts it to the neutral request passed to
// the router. Message content is forwarded unchanged.
func normalize(req chatRequest) (llm.ChatRequest, error) {
	if req.Model == nil || strings.TrimSpace(*req.Model) == "" {
		return llm.ChatRequest{}, errors.New("model is required")
	}
	if req.Stream != nil && *req.Stream {
		return llm.ChatRequest{}, errors.New("streaming is not supported")
	}

	maxTokens := defaultMaxTokens
	if req.MaxTokens != nil {
		if *req.MaxTokens <= 0 {
			return llm.ChatRequest{}, errors.New("max_tokens must be a positive integer")
		}
		maxTokens = *req.MaxTokens
	}

	if len(req.Messages) == 0 {
		return llm.ChatRequest{}, errors.New("messages must not be empty")
	}
	messages := make([]llm.Message, 0, len(req.Messages))
	conversational := 0
	for i, m := range req.Messages {
		if m.Role == nil {
			return llm.ChatRequest{}, fmt.Errorf("messages[%d].role is required", i)
		}
		switch role := *m.Role; role {
		case llm.RoleSystem:
			if i != 0 {
				return llm.ChatRequest{}, fmt.Errorf("messages[%d]: a system message is only allowed as the first message", i)
			}
		case llm.RoleUser, llm.RoleAssistant:
			conversational++
		default:
			return llm.ChatRequest{}, fmt.Errorf("messages[%d].role must be one of system, user, or assistant", i)
		}
		if m.Content == nil {
			return llm.ChatRequest{}, fmt.Errorf("messages[%d].content must be a string", i)
		}
		if strings.TrimSpace(*m.Content) == "" {
			return llm.ChatRequest{}, fmt.Errorf("messages[%d].content must not be blank", i)
		}
		messages = append(messages, llm.Message{Role: *m.Role, Content: *m.Content})
	}
	if conversational == 0 {
		return llm.ChatRequest{}, errors.New("messages must contain at least one user or assistant message")
	}

	return llm.ChatRequest{
		Model:     *req.Model,
		Messages:  messages,
		MaxTokens: maxTokens,
	}, nil
}
