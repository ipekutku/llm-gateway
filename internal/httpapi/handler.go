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
	"github.com/ipekutku/llm-gateway/internal/usage"
)

// ChatCompletionsPath is the path of the chat completions endpoint.
const ChatCompletionsPath = "/v1/chat/completions"

// RequestIDHeader is the response header carrying the ID the gateway
// assigned to the request.
const RequestIDHeader = "X-Request-ID"

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
	accounting      Accounting
	metrics         Metrics
}

// UsageRecorder accepts records without waiting for persistent storage.
type UsageRecorder interface {
	Record(usage.Record) bool
}

// Metrics observes requests to the chat completions endpoint.
// Implementations bound the label values themselves.
type Metrics interface {
	// ObserveRequest is called once per request, including rejected and
	// abandoned ones. model is the requested model, or empty if the
	// request was rejected before it was known. code is the error code, or
	// empty on success.
	ObserveRequest(model string, status int, code string, d time.Duration)
	// ObserveUsage is called once per request whose usage the provider
	// reported, with the same values as its usage record. model is the
	// provider's configured model, the name prices use. cost is nil if
	// unknown.
	ObserveUsage(provider, model string, u llm.Usage, cost *usage.Cost)
}

type noMetrics struct{}

func (noMetrics) ObserveRequest(string, int, string, time.Duration)   {}
func (noMetrics) ObserveUsage(string, string, llm.Usage, *usage.Cost) {}

// Accounting provides the recorder, prices, and each provider's configured
// model name. Pricing uses configured names rather than response snapshots.
type Accounting struct {
	Recorder UsageRecorder
	Pricing  *usage.Pricing
	Models   map[string]string
}

// New returns the gateway's HTTP handler. It serves POST
// /v1/chat/completions and sends every request to provider, which is
// normally the router. A nil log uses slog.Default. Unless its handler
// comes from NewLogHandler, log is wrapped with one, so the handler's own
// logs always carry the request and client IDs.
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
// Every request is given a random ID when it arrives, returned in the
// RequestIDHeader response header and logged with failures. A successful
// completion's id is "chatcmpl-" followed by it. An X-Request-ID sent by the
// client is ignored.
//
// Request bodies must arrive within bodyReadTimeout, or the response is
// 408. The time to wait for the upstream is not limited by it.
//
// accounting records every validated request once, including failures and
// cancellations. Admission and validation rejections are not recorded.
//
// metrics observes every request to the endpoint, rejections included. A
// nil metrics observes nothing.
func New(provider llm.Provider, authenticator *auth.Authenticator, limiter *ratelimit.Limiter, upstreamTimeout time.Duration, accounting Accounting, metrics Metrics, log *slog.Logger) (http.Handler, error) {
	switch {
	case provider == nil:
		return nil, errors.New("httpapi: nil provider")
	case authenticator == nil:
		return nil, errors.New("httpapi: nil authenticator")
	case limiter == nil:
		return nil, errors.New("httpapi: nil limiter")
	case upstreamTimeout <= 0:
		return nil, errors.New("httpapi: upstream timeout must be positive")
	case accounting.Recorder == nil || accounting.Pricing == nil:
		return nil, errors.New("httpapi: recorder and pricing are required")
	}
	if log == nil {
		log = slog.Default()
	}
	if metrics == nil {
		metrics = noMetrics{}
	}
	if _, ok := log.Handler().(logHandler); !ok {
		log = slog.New(NewLogHandler(log.Handler()))
	}
	models := make(map[string]string, len(accounting.Models))
	for provider, model := range accounting.Models {
		models[provider] = model
	}
	accounting.Models = models
	h := &handler{provider: provider, auth: authenticator, limiter: limiter, upstreamTimeout: upstreamTimeout, accounting: accounting, metrics: metrics, log: log}

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

		received := time.Now()
		id := rand.Text()
		w.Header().Set(RequestIDHeader, id)
		ctx := context.WithValue(r.Context(), requestIDKey{}, id)
		ctx = context.WithValue(ctx, receivedAtKey{}, received)
		mux.ServeHTTP(w, r.WithContext(ctx))
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
	received, _ := r.Context().Value(receivedAtKey{}).(time.Time)
	identity, _ := auth.FromContext(r.Context())
	record := usage.Record{RequestID: requestID(r), Time: received, ClientID: identity.ClientID, RequestedModel: req.Model}
	ctx, stats := llm.WithStats(r.Context())
	var chatErr error
	defer func() {
		if r.Context().Err() != nil {
			record.Status, record.ErrorCode = usage.StatusClientClosed, "client_closed"
		}
		record.Duration = time.Since(received)
		h.logOutcome(r.Context(), record, chatErr, stats)
		h.metrics.ObserveRequest(record.RequestedModel, record.Status, record.ErrorCode, record.Duration)
		// The recorder drops invalid records, such as inconsistent token
		// counts; metrics skip them too, so both agree.
		if record.Usage != nil && record.Validate() == nil {
			h.metrics.ObserveUsage(record.Provider, h.accounting.Models[record.Provider], *record.Usage, record.Cost)
		}
		h.accounting.Recorder.Record(record)
	}()
	ctx, cancel := context.WithTimeout(ctx, h.upstreamTimeout)
	defer cancel()

	resp, err := h.provider.Chat(ctx, req)
	if err != nil {
		chatErr = err
		if pe, ok := errors.AsType[*llm.ProviderError](err); ok {
			record.Provider = pe.Provider
		}
		if r.Context().Err() != nil {
			// The client is gone; there is nobody to write a response to.
			// The deferred function records the cancellation.
			return
		}
		failure := classifyChatError(err)
		record.Status, record.ErrorCode = failure.status, failure.code
		if writeErr := writeError(w, failure); writeErr != nil {
			record.Status, record.ErrorCode = usage.StatusClientClosed, "client_closed"
		}
		return
	}

	model := resp.Model
	if model == "" {
		model = req.Model
	}
	record.Status, record.Provider, record.Model = http.StatusOK, resp.Provider, model
	record.Usage = &resp.Usage
	cost, costErr := h.accounting.Pricing.Cost(usage.Model{Provider: resp.Provider, Model: h.accounting.Models[resp.Provider]}, resp.Usage)
	if costErr == nil {
		record.Cost = &cost
	} else if !errors.Is(costErr, usage.ErrNoPrice) {
		h.log.LogAttrs(r.Context(), slog.LevelWarn, "usage cost could not be estimated", slog.Any("error", costErr))
	}
	err = writeJSON(w, http.StatusOK, chatResponse{
		ID:      "chatcmpl-" + requestID(r),
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
	if err != nil || r.Context().Err() != nil {
		record.Status, record.ErrorCode = usage.StatusClientClosed, "client_closed"
	}
}

// fail logs a request rejected before it reached a provider, and writes
// the error envelope. Logs contain no credentials, prompt or completion
// content, or raw upstream bodies; err must follow the same rule, as
// llm.ProviderError does. The request and client IDs come from the context
// through logHandler.
func (h *handler) fail(w http.ResponseWriter, r *http.Request, e apiError, err error, attrs ...slog.Attr) {
	attrs = append(attrs,
		slog.Int("status", e.status),
		slog.String("code", e.code),
	)
	if err != nil {
		attrs = append(attrs, slog.Any("error", err))
	}
	h.log.LogAttrs(r.Context(), statusLevel(e.status), "chat completion failed", attrs...)
	_ = writeError(w, e)
	received, _ := r.Context().Value(receivedAtKey{}).(time.Time)
	h.metrics.ObserveRequest("", e.status, e.code, time.Since(received))
}

// logOutcome logs the one line describing a validated request's outcome,
// with the same status, error code, and provider as its usage record. err
// is the provider error, if any; the rules of fail apply to it.
func (h *handler) logOutcome(ctx context.Context, record usage.Record, err error, stats *llm.Stats) {
	attrs := []slog.Attr{
		slog.String("model", record.RequestedModel),
		slog.Int("status", record.Status),
	}
	if record.ErrorCode != "" {
		attrs = append(attrs, slog.String("code", record.ErrorCode))
	}
	if record.Provider != "" {
		attrs = append(attrs, slog.String("provider", record.Provider))
	}
	if pe, ok := errors.AsType[*llm.ProviderError](err); ok {
		attrs = append(attrs, slog.Int("upstream_status", pe.StatusCode))
	}
	attrs = append(attrs,
		slog.Duration("latency", record.Duration),
		slog.Int("retry_count", stats.Retries()),
		slog.Bool("fallback", stats.Fallback()),
	)
	if err != nil {
		attrs = append(attrs, slog.Any("error", err))
	}
	level := statusLevel(record.Status)
	if record.Status == usage.StatusClientClosed {
		level = slog.LevelInfo
	}
	h.log.LogAttrs(ctx, level, "request completed", attrs...)
}

// statusLevel is the log level for a response status: error for 5xx, warn
// for 4xx, and info otherwise.
func statusLevel(status int) slog.Level {
	switch {
	case status >= http.StatusInternalServerError:
		return slog.LevelError
	case status >= http.StatusBadRequest:
		return slog.LevelWarn
	default:
		return slog.LevelInfo
	}
}

// writeError writes the error envelope for e.
func writeError(w http.ResponseWriter, e apiError) error {
	return writeJSON(w, e.status, errorResponse{Error: errorBody{
		Message: e.message,
		Type:    e.typ,
		Code:    e.code,
	}})
}

type requestIDKey struct{}
type receivedAtKey struct{}

// requestID is the ID assigned to r when it arrived.
func requestID(r *http.Request) string {
	id, _ := r.Context().Value(requestIDKey{}).(string)
	return id
}

func writeJSON(w http.ResponseWriter, status int, v any) error {
	body, err := json.Marshal(v)
	if err != nil {
		// Unreachable: the response types contain only strings and integers.
		w.WriteHeader(http.StatusInternalServerError)
		return err
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, err = w.Write(body)
	return err
}
