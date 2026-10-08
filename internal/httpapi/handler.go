// Package httpapi implements the gateway's public HTTP API: request
// decoding and validation, translation between public wire types and the
// neutral llm types, and the error envelope.
package httpapi

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"time"

	"github.com/ipekutku/llm-gateway/internal/llm"
)

// ChatCompletionsPath is the path of the chat completions endpoint.
const ChatCompletionsPath = "/v1/chat/completions"

type handler struct {
	provider        llm.Provider
	upstreamTimeout time.Duration
	log             *slog.Logger
}

// New returns the gateway's HTTP handler. It serves POST
// /v1/chat/completions and sends every request to provider, which is
// normally the router. A nil log uses slog.Default.
//
// upstreamTimeout bounds each provider call, covering all upstream work for
// one request. It must be positive. When it expires while the client is
// still connected, the response is 504.
//
// Requests to other paths receive 404, and other methods on the endpoint
// receive 405 with an Allow header.
func New(provider llm.Provider, upstreamTimeout time.Duration, log *slog.Logger) (http.Handler, error) {
	if provider == nil {
		return nil, errors.New("httpapi: nil provider")
	}
	if upstreamTimeout <= 0 {
		return nil, errors.New("httpapi: upstream timeout must be positive")
	}
	if log == nil {
		log = slog.Default()
	}
	h := &handler{provider: provider, upstreamTimeout: upstreamTimeout, log: log}

	mux := http.NewServeMux()
	mux.HandleFunc("POST "+ChatCompletionsPath, h.chatCompletions)
	return mux, nil
}

func (h *handler) chatCompletions(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxRequestBytes))
	if err != nil {
		if _, ok := errors.AsType[*http.MaxBytesError](err); ok {
			h.fail(w, r, errRequestTooLarge, nil)
			return
		}
		h.fail(w, r, invalidRequest("The request body could not be read."), nil)
		return
	}

	req, err := parseRequest(body)
	if err != nil {
		h.fail(w, r, invalidRequest(err.Error()), nil)
		return
	}
	h.complete(w, r, req)
}

func (h *handler) complete(w http.ResponseWriter, r *http.Request, req llm.ChatRequest) {
	ctx, cancel := context.WithTimeout(r.Context(), h.upstreamTimeout)
	defer cancel()

	resp, err := h.provider.Chat(ctx, req)
	if err != nil {
		if r.Context().Err() != nil {
			// The client is gone; there is nobody to write a response to.
			h.log.InfoContext(r.Context(), "client canceled request",
				slog.String("model", req.Model),
				slog.Any("error", err),
			)
			return
		}
		h.fail(w, r, classifyChatError(err), err, slog.String("model", req.Model))
		return
	}

	model := resp.Model
	if model == "" {
		model = req.Model
	}
	writeJSON(w, http.StatusOK, chatResponse{
		ID:      "chatcmpl-" + rand.Text(),
		Object:  "chat.completion",
		Created: time.Now().Unix(),
		Model:   model,
		Choices: []chatChoice{{
			Index: 0,
			Message: chatResponseMessage{
				Role:    llm.RoleAssistant,
				Content: resp.Message.Content,
			},
			FinishReason: resp.FinishReason,
		}},
		Usage: chatUsage{
			PromptTokens:     resp.Usage.InputTokens,
			CompletionTokens: resp.Usage.OutputTokens,
			TotalTokens:      resp.Usage.InputTokens + resp.Usage.OutputTokens,
		},
	})
}

// fail logs a failed request once and writes the error envelope. Logs
// contain no credentials, prompt or completion content, or raw upstream
// bodies; err must follow the same rule, as llm.ProviderError does.
func (h *handler) fail(w http.ResponseWriter, r *http.Request, e apiError, err error, attrs ...slog.Attr) {
	attrs = append(attrs,
		slog.Int("status", e.status),
		slog.String("code", e.code),
	)
	if pe, ok := errors.AsType[*llm.ProviderError](err); ok {
		attrs = append(attrs,
			slog.String("provider", pe.Provider),
			slog.Int("upstream_status", pe.StatusCode),
		)
	}
	if err != nil {
		attrs = append(attrs, slog.Any("error", err))
	}

	level := slog.LevelWarn
	if e.status >= http.StatusInternalServerError {
		level = slog.LevelError
	}
	h.log.LogAttrs(r.Context(), level, "chat completion failed", attrs...)

	writeJSON(w, e.status, errorResponse{Error: errorBody{
		Message: e.message,
		Type:    e.typ,
		Code:    e.code,
	}})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	body, err := json.Marshal(v)
	if err != nil {
		// Unreachable: the response types contain only strings and integers.
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(body)
}
