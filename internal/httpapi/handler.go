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
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/ipekutku/llm-gateway/internal/auth"
	"github.com/ipekutku/llm-gateway/internal/llm"
	"github.com/ipekutku/llm-gateway/internal/ratelimit"
)

// ChatCompletionsPath is the path of the chat completions endpoint.
const ChatCompletionsPath = "/v1/chat/completions"

// bodyReadTimeout bounds how long a client may take to send a request body,
// counted from when its headers have been read. It also bounds how long the
// server spends discarding the unread body of a rejected request. Replaced
// in tests.
var bodyReadTimeout = 30 * time.Second

type handler struct {
	provider        llm.Provider
	auth            *auth.Authenticator
	limiter         *ratelimit.Limiter
	upstreamTimeout time.Duration
	log             *slog.Logger
}

// New returns the gateway's HTTP handler. It serves POST
// /v1/chat/completions and sends every request to provider, which is
// normally the router. A nil log uses slog.Default.
//
// Every request must carry a gateway API key accepted by authenticator, as
// "Authorization: Bearer <key>", and is then subject to the client's limits
// in limiter, which must have limits for every client. Both checks happen
// before the request body is read. The client's auth.Identity is in the
// context passed to provider.
//
// upstreamTimeout bounds each provider call, covering all upstream work for
// one request. It must be positive. When it expires while the client is
// still connected, the response is 504.
//
// Requests to other paths receive 404, and other methods on the endpoint
// receive 405 with an Allow header.
//
// Request bodies must arrive within bodyReadTimeout, or the response is
// 408. The time to wait for the upstream is not limited by it.
func New(provider llm.Provider, authenticator *auth.Authenticator, limiter *ratelimit.Limiter, upstreamTimeout time.Duration, log *slog.Logger) (http.Handler, error) {
	switch {
	case provider == nil:
		return nil, errors.New("httpapi: nil provider")
	case authenticator == nil:
		return nil, errors.New("httpapi: nil authenticator")
	case limiter == nil:
		return nil, errors.New("httpapi: nil limiter")
	case upstreamTimeout <= 0:
		return nil, errors.New("httpapi: upstream timeout must be positive")
	}
	if log == nil {
		log = slog.Default()
	}
	h := &handler{provider: provider, auth: authenticator, limiter: limiter, upstreamTimeout: upstreamTimeout, log: log}

	mux := http.NewServeMux()
	mux.HandleFunc("POST "+ChatCompletionsPath, h.chatCompletions)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Set for every request, so it also bounds discarding the unread
		// body of a request rejected before its body is read: 401, 429,
		// 404, or 405. Once the body has been read, net/http clears the
		// deadline itself when it starts watching for a client disconnect,
		// so the wait for the upstream is not limited; see
		// TestBodyReadDeadlineDoesNotLimitUpstreamWait. An unsupported
		// writer, such as a test recorder, gets no deadline.
		_ = http.NewResponseController(w).SetReadDeadline(time.Now().Add(bodyReadTimeout))
		mux.ServeHTTP(w, r)
	}), nil
}

func (h *handler) chatCompletions(w http.ResponseWriter, r *http.Request) {
	id, ok := h.authenticate(w, r)
	if !ok {
		return
	}
	r = r.WithContext(auth.NewContext(r.Context(), id))

	release, ok := h.acquire(w, r, id)
	if !ok {
		return
	}
	defer release()

	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxRequestBytes))
	if err != nil {
		if _, ok := errors.AsType[*http.MaxBytesError](err); ok {
			h.fail(w, r, errRequestTooLarge, nil)
			return
		}
		if errors.Is(err, os.ErrDeadlineExceeded) {
			h.fail(w, r, errRequestTimeout, err)
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

// authenticate identifies the client from the Authorization header. On
// failure it writes a 401 and returns false.
func (h *handler) authenticate(w http.ResponseWriter, r *http.Request) (auth.Identity, bool) {
	key, err := bearerToken(r.Header)
	if err != nil {
		w.Header().Set("WWW-Authenticate", "Bearer")
		h.fail(w, r, errMissingAPIKey, err)
		return auth.Identity{}, false
	}
	id, err := h.auth.Authenticate(key)
	if err != nil {
		w.Header().Set("WWW-Authenticate", `Bearer error="invalid_token"`)
		h.fail(w, r, errInvalidAPIKey, err)
		return auth.Identity{}, false
	}
	return id, true
}

// bearerToken returns the token of a single "Authorization: Bearer <token>"
// header. The scheme is case-insensitive. Errors never contain the header's
// value.
func bearerToken(header http.Header) (string, error) {
	values := header.Values("Authorization")
	switch len(values) {
	case 0:
		return "", errors.New("missing Authorization header")
	case 1:
	default:
		return "", errors.New("multiple Authorization headers")
	}
	scheme, token, _ := strings.Cut(values[0], " ")
	token = strings.TrimLeft(token, " ")
	if !strings.EqualFold(scheme, "Bearer") || token == "" || strings.ContainsAny(token, " \t") {
		return "", errors.New("Authorization header is not 'Bearer <key>'")
	}
	return token, nil
}

// acquire admits the request under the client's limits. On success the
// caller must call release when the request has finished. On failure it
// writes a 429, or a 500 for a client without limits, and returns false.
func (h *handler) acquire(w http.ResponseWriter, r *http.Request, id auth.Identity) (release func(), ok bool) {
	release, err := h.limiter.Acquire(id.ClientID)
	if err == nil {
		return release, true
	}
	le, isLimit := errors.AsType[*ratelimit.Error](err)
	switch {
	case !isLimit:
		h.fail(w, r, errInternal, err)
	case le.Limit == ratelimit.ConcurrentRequests:
		h.fail(w, r, errConcurrencyLimitExceeded, err)
	default:
		w.Header().Set("Retry-After", retryAfterSeconds(le.RetryAfter))
		h.fail(w, r, errRateLimitExceeded, err, slog.Duration("retry_after", le.RetryAfter))
	}
	return nil, false
}

// retryAfterSeconds formats d for the Retry-After header: whole seconds,
// rounded up so a client that waits that long is admitted, and at least 1.
func retryAfterSeconds(d time.Duration) string {
	secs := max(1, int64((d+time.Second-1)/time.Second))
	return strconv.FormatInt(secs, 10)
}

func (h *handler) complete(w http.ResponseWriter, r *http.Request, req llm.ChatRequest) {
	ctx, cancel := context.WithTimeout(r.Context(), h.upstreamTimeout)
	defer cancel()

	resp, err := h.provider.Chat(ctx, req)
	if err != nil {
		if r.Context().Err() != nil {
			// The client is gone; there is nobody to write a response to.
			h.log.LogAttrs(r.Context(), slog.LevelInfo, "client canceled request",
				clientAttr(r),
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
		clientAttr(r),
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

// clientAttr is the authenticated client's ID for logs. Before
// authentication it is the empty Attr, which slog omits.
func clientAttr(r *http.Request) slog.Attr {
	id, ok := auth.FromContext(r.Context())
	if !ok {
		return slog.Attr{}
	}
	return slog.String("client_id", id.ClientID)
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
