# Architecture

This document describes the architecture as implemented in v0.1: an HTTP endpoint, a vendor-neutral provider contract, OpenAI and Anthropic adapters, and static routing. Milestone 2 (v0.2) work in progress adds upstream timeouts; retries are not implemented yet.

## Request flow

```text
HTTP request → httpapi → routing → provider adapter → upstream HTTP request
                  └──────── shared internal/llm types ────────┘
```

The incoming request's `context.Context` is passed through every step to the outbound upstream request. The handler derives it once, adding the upstream deadline (see [Upstream timeouts](#upstream-timeouts)); nothing below the handler replaces or detaches it.

## Packages and dependency boundaries

| Package | Responsibility |
|---|---|
| `internal/llm` | Vendor-neutral types (`ChatRequest`, `ChatResponse`, `Message`, `Usage`), the `Provider` interface, and shared errors. |
| `internal/routing` | Exact-match static model router. |
| `internal/httpapi` | Public wire DTOs, validation, handler, error responses. |
| `internal/provider/openai` | OpenAI Chat Completions client with private wire types. |
| `internal/provider/anthropic` | Anthropic Messages client with private wire types. |
| `cmd/gateway` | Environment configuration, wiring, and server lifecycle. |

Rules:

- `llm` imports no other project package. Every other package depends on it.
- Provider-specific request and response types are unexported inside their provider package.
- The HTTP layer never depends on a concrete provider. It depends only on `llm.Provider`.
- `llm` types carry no JSON tags. Public wire formats live in `httpapi`, and upstream wire formats live in each provider package.

## Provider contract

```go
type Provider interface {
    Chat(ctx context.Context, req ChatRequest) (ChatResponse, error)
}
```

Implementations must honor context cancellation, support concurrent calls, and must not mutate the request or its `Messages` slice. The router shares provider instances across requests, so each provider must keep per-call state local or synchronize access to shared mutable state. The contract is deliberately text-only. Content is a `string`, and there is no streaming, tool calling, or multimodal input in Milestone 1.

## Routing

`routing.Router` holds a static `model → provider` table and itself implements `llm.Provider`. The handler therefore needs no separate router interface, and later milestones (fallback, circuit breaking) can wrap providers or the router without changing the handler.

- **Exact match only.** There are no aliases, prefixes, wildcards, or model rewriting. The model name is forwarded to the provider unchanged.
- **Validated at construction.** `routing.New` rejects an empty table, blank model names, and nil providers.
- **Immutable.** The table is copied at construction and never modified afterwards, so concurrent requests need no locking.
- An unknown model returns an error wrapping `llm.ErrUnknownModel`. Provider errors are returned unchanged.

## Errors

| Error | Meaning |
|---|---|
| `llm.ErrUnknownModel` | No provider is configured for the requested model. |
| `*llm.ProviderError` | An upstream call failed: non-2xx status, transport failure, or an unreadable or unusable response. It carries the provider name, the upstream status (0 if there was no response), and a wrapped cause. |

`ProviderError` implements `Unwrap`, so `errors.Is(err, context.Canceled)` and `errors.Is(err, context.DeadlineExceeded)` still work through it. It never carries credentials or raw upstream bodies.

Provider adapters must return every failure, including transport errors and cancellation, as a `*ProviderError`. Any other error type reaching the handler is treated as an unexpected gateway failure.

## HTTP API

`httpapi.New(provider, upstreamTimeout, logger)` returns an `http.Handler` serving `POST /v1/chat/completions`. The provider is normally the router. Other methods on the endpoint receive 405 with `Allow: POST`, and other paths receive 404; both use the standard `ServeMux` responses.

The endpoint implements a deliberately small subset of the OpenAI Chat Completions format. It does not claim full API or SDK compatibility. Public wire types are unexported in `httpapi` and are translated to and from the neutral `llm` types.

### Request

| Field | Rule |
|---|---|
| `model` | Required, non-blank string. Forwarded unchanged for exact-match routing. |
| `messages` | Required, non-empty array. An optional `system` message is allowed only at index 0. Every other role must be `user` or `assistant`, and at least one such message is required. |
| `messages[].content` | Required, non-blank string. Arrays, objects, and `null` are rejected. Content is forwarded unchanged, including surrounding whitespace. |
| `max_tokens` | Optional positive integer. Absent or `null` uses the gateway default of 1024, so every provider receives the same normalized value. Provider-specific limits are left to the upstream. |
| `stream` | Optional. Absent, `null`, or `false` is accepted; `true` is rejected. |

Other rules:

- The body is limited to 1 MiB and must be exactly one JSON object, optionally surrounded by whitespace. Empty bodies, `null`, non-object values, malformed JSON, and trailing values are rejected.
- Fields with the wrong JSON type are rejected.
- Unknown fields such as `temperature`, `tools`, or `n` are accepted for forward compatibility but have no effect.
- Invalid requests never reach the router.

### Response

A success returns 200 with exactly one choice:

```json
{
  "id": "chatcmpl-<26 random base32 characters>",
  "object": "chat.completion",
  "created": 1700000000,
  "model": "<provider-reported model, or the requested model>",
  "choices": [
    {
      "index": 0,
      "message": {"role": "assistant", "content": "..."},
      "finish_reason": "stop"
    }
  ],
  "usage": {"prompt_tokens": 12, "completion_tokens": 8, "total_tokens": 20}
}
```

`id` (from `crypto/rand`) and `created` (Unix seconds) are gateway metadata, not upstream identifiers. `total_tokens` is the sum of the neutral input and output counts. Finish reasons are `stop`, `length`, or `content_filter`.

### Error mapping

Errors use a gateway-owned envelope:

```json
{"error": {"message": "...", "type": "server_error", "code": "upstream_error"}}
```

| Condition | Status | `code` | `type` |
|---|---|---|---|
| Invalid JSON, validation failure, streaming requested | 400 | `invalid_request` | `invalid_request_error` |
| Body exceeds 1 MiB | 413 | `request_too_large` | `invalid_request_error` |
| Unknown model (`llm.ErrUnknownModel`) | 404 | `model_not_found` | `invalid_request_error` |
| Upstream 400 | 400 | `invalid_request` | `invalid_request_error` |
| Upstream 429 | 429 | `provider_rate_limited` | `rate_limit_error` |
| Other upstream status, including 401 and 403 | 502 | `upstream_error` | `server_error` |
| Transport failure, or a malformed, oversized, or unusable upstream response | 502 | `upstream_error` | `server_error` |
| `context.DeadlineExceeded` while the incoming request is still active | 504 | `upstream_timeout` | `server_error` |
| Any other error | 500 | `internal_error` | `server_error` |

Decisions:

- **Context errors are checked first.** If the incoming request's context is done, the client is gone. The handler logs the cancellation and writes nothing. Otherwise a wrapped `DeadlineExceeded` maps to 504, and a wrapped `Canceled` maps to 502, because a canceled provider operation alone does not prove that the client disconnected.
- **Upstream 401 and 403 map to 502.** They indicate a gateway credential problem, not a client mistake.
- **Messages are fixed per category.** Validation messages are gateway-authored and describe the client's mistake. Upstream messages and bodies are never returned to the caller.
- **Each failure is logged once** at the handler with `slog`: status, code, model, provider, upstream status, and the error. Logs never include credentials, prompt or completion content, or raw upstream bodies. 5xx responses log at error level and 4xx responses at warn.

A slow upstream that exceeds the upstream timeout produces 504 `upstream_timeout`, as long as the client is still connected.

## Provider adapters

Each adapter's constructor takes an API key, a base URL, and an `*http.Client`.

- **Base URL.** An `http` or `https` origin with an optional path prefix and without the endpoint suffix. The adapter appends its endpoint path exactly once, so `https://api.openai.com` and `http://127.0.0.1:9000/proxy/` both work. URLs with credentials, a query, or a fragment are rejected. Production uses the HTTPS provider origin; tests inject local `httptest` servers.
- **HTTP client.** `nil` means `http.DefaultClient`. The adapter adds no timeout or retry policy of its own: deadlines arrive through the request context, and connection-setup limits through the injected client's transport.
- **Requests.** Built with `http.NewRequestWithContext`, so the incoming context reaches the upstream call.
- **Responses.** Bodies are closed on every path. Successful responses are read up to 4 MiB; anything larger is an upstream failure. Non-2xx response bodies are not read at all, so they cannot leak into errors or logs.
- **Errors.** Every upstream failure is a `*llm.ProviderError`: non-2xx status, transport failure, unreadable or oversized body, malformed JSON, or a structurally unusable response. When the context is done, the context error is always wrapped, so `errors.Is(err, context.Canceled)` holds even if cancellation interrupts the body read. A request the gateway should never produce (blank model, non-positive `MaxTokens`, no messages, unknown role) returns a plain error without calling the upstream. The handler treats that as an internal error.
- **Usage.** Never fabricated. A response without both token counts is a protocol error.
- **Empty text.** A structurally valid response with empty text is a success.

### OpenAI

`POST {base}/v1/chat/completions` with `Authorization: Bearer <key>`.

| Neutral | OpenAI |
|---|---|
| `Model` | `model` |
| `Messages` | `messages`, with roles and content unchanged; `system` stays a message |
| `MaxTokens` | `max_completion_tokens` |
| response `Model` | `model` |
| `Usage.InputTokens` / `OutputTokens` | `usage.prompt_tokens` / `usage.completion_tokens` |

- **`max_completion_tokens`, not `max_tokens`.** OpenAI's API specification marks `max_tokens` as deprecated and "not compatible with o-series models". `max_completion_tokens` is accepted by current models. On reasoning models it also counts reasoning tokens, so a small limit can produce an empty, length-truncated answer.
- **Exactly one choice.** The response must contain one choice with an `assistant` message and string `content`. Zero or several choices, a missing message, `null` content, `tool_calls`, and `function_call` are protocol errors.
- **Refusals map to `content_filter`.** A refusal arrives as a `refusal` string instead of `content`. It is returned as the message text with finish reason `content_filter`, matching the Anthropic adapter.
- **Finish reasons.** `stop`, `length`, and `content_filter` map to the neutral values of the same name. `tool_calls`, `function_call`, and any undocumented reason are protocol errors.

Response fixtures follow the example in OpenAI's published OpenAPI specification. No live model has been verified yet; the README will list tested model IDs.

### Anthropic

`POST {base}/v1/messages` with `x-api-key: <key>` and the pinned header `anthropic-version: 2023-06-01`.

| Neutral | Anthropic |
|---|---|
| `Model` | `model` |
| leading `system` message | top-level `system` string, omitted when absent |
| other `Messages` | `messages`, with roles and content unchanged and in order |
| `MaxTokens` | `max_tokens`, always sent with no provider-specific default |
| response `Model` | `model` |
| `Usage.InputTokens` | `usage.input_tokens + cache_creation_input_tokens + cache_read_input_tokens` |
| `Usage.OutputTokens` | `usage.output_tokens` |

- **Conversation rules are left to the upstream.** Anthropic constraints such as role alternation or a final assistant turn (prefill, rejected by current models) are not pre-validated. They surface as an upstream 400, which the gateway maps to 400.
- **Response content.** `text` blocks are concatenated in order with no separator. `thinking` and `redacted_thinking` blocks are reasoning, not output, and are skipped; current models such as Claude Opus 5.5 always think, so rejecting them would fail every request. Any other block type (`tool_use`, `server_tool_use`, tool results, unknown types) is a protocol error. An empty `content` array or empty text is a valid empty answer.
- **Input usage includes cached tokens.** `input_tokens` excludes prompt-cache reads and writes, while OpenAI's `prompt_tokens` includes cached tokens. Adding the cache fields keeps the neutral count comparable. The gateway does not request caching itself.

Stop reasons, as documented for API version `2023-06-01`:

| `stop_reason` | Neutral finish reason |
|---|---|
| `end_turn`, `stop_sequence` | `stop` |
| `max_tokens`, `model_context_window_exceeded` | `length` |
| `refusal` | `content_filter`, keeping any partial text |
| `tool_use`, `pause_turn`, anything else | protocol error (502) |

On models that always think, thinking tokens count toward `max_tokens`. A small limit, including the gateway default of 1024, can therefore end with `length` and little or no text. The same applies to OpenAI reasoning models.

Response fixtures follow Anthropic's documented response shape. No live model has been verified yet.

## Configuration

Configuration is read once at startup from the environment in `cmd/gateway`. There is no configuration file or framework.

| Variable | Rule |
|---|---|
| `OPENAI_MODEL`, `OPENAI_API_KEY` | Both absent disables OpenAI. Both present routes that one model to OpenAI. Only one present fails startup. |
| `ANTHROPIC_MODEL`, `ANTHROPIC_API_KEY` | Same rule for Anthropic. |
| `GATEWAY_ADDR` | Listen address. Default `127.0.0.1:8080`, so the unauthenticated gateway is not exposed by accident. |
| `GATEWAY_UPSTREAM_TIMEOUT` | Upstream time budget per request, as a Go duration (`90s`, `2m`). Default `120s`. |
| `GATEWAY_UPSTREAM_CONNECT_TIMEOUT` | Limit on the upstream TCP dial and, separately, the TLS handshake. Default `10s`. |

- **Absent means unset or blank.** A whitespace-only value counts as absent. Non-blank values, including model names, are used unchanged.
- **Startup fails if no provider is enabled, if a pair is incomplete, or if both providers declare the same model.** All pair errors are reported together. Errors name the variables and never include their values. An enabled route is never silently dropped.
- **Base URLs are fixed** to the providers' production HTTPS origins. Tests inject local fake servers through the same `config` struct, but there is no environment variable for them in v0.1.
- **Model IDs are not defaulted.** A built-in model name would go stale; the operator always chooses.
- **Durations must be positive.** An unparsable, zero, or negative duration fails startup and is reported together with any other configuration errors.
- The startup log names the configured models, never the keys.

`newHandler` builds one shared upstream `http.Client`, one adapter per enabled provider, the router, and the HTTP handler.

## Upstream timeouts

Two independent limits bound upstream work:

| Limit | Where | Covers |
|---|---|---|
| Upstream timeout (`GATEWAY_UPSTREAM_TIMEOUT`) | `context.WithTimeout` in the handler, around the provider call | Everything below the handler for one request: routing, connecting, sending, waiting for the response, and reading the body. Later retries (and, in Milestone 3, fallback) share this one budget. |
| Connect timeout (`GATEWAY_UPSTREAM_CONNECT_TIMEOUT`) | `net.Dialer.Timeout` and `Transport.TLSHandshakeTimeout` on the shared client | Establishing a new connection. A request still cannot exceed the upstream timeout, because the dial also observes the request context. |

Decisions:

- **One budget at the top, not one per layer.** Setting the deadline in the handler means every current and future layer below it sees the same deadline through `ctx`, and nested timeouts cannot add up past what the client was promised.
- **No `http.Client.Timeout` or `ResponseHeaderTimeout`.** Non-streaming providers send headers only after generation, so a header timeout would duplicate the request timeout. Using the context instead keeps the error a wrapped `context.DeadlineExceeded`, which the handler maps to 504.
- **Deadline versus client cancellation.** If the client disconnects, the handler logs the cancellation and writes nothing, as before. If only the derived deadline fired, the response is 504 `upstream_timeout`.
- **Expired requests are canceled upstream.** The deadline cancels the outbound HTTP request, so the provider is not left generating for nobody. The provider may still charge for work done before cancellation.
- **Idle connections** are kept for 90 seconds (a fixed constant, `idleConnTimeout`). Other transport settings, including the proxy from the environment, are those of `http.DefaultTransport`.

## Server lifecycle

- `http.Server` sets `ReadHeaderTimeout` to 5 seconds. Other server timeouts are left unset; handler time is bounded by the upstream timeout instead.
- `signal.NotifyContext` cancels on `SIGINT` or `SIGTERM`. Shutdown then runs with a fresh 5-second context; the signal context is already canceled.
- During graceful shutdown the listener closes and in-flight requests finish normally. If they are still running after 5 seconds, the server is closed. That cancels their request contexts, and the cancellation propagates to the upstream calls. The process then exits non-zero.
- `http.ErrServerClosed` counts as a normal stop. Configuration, listen, and serve errors are logged and exit with status 1.

These limits govern the server's own lifecycle and are separate from the upstream timeouts. A shutdown can therefore cut off a request that is still within its upstream timeout.

## Testing

All tests run without credentials or network access, using `httptest` servers.

| Level | What it proves |
|---|---|
| `internal/routing`, `internal/httpapi` | Routing, validation, response translation, and error mapping, using fake providers. |
| `internal/provider/*` | Wire format, headers, status handling, malformed and oversized responses, transport failure, cancellation, and slow upstreams, against fake provider servers. |
| `cmd/gateway` | Configuration rules, and the complete path: the real handler, router, and both adapters behind an `httptest.Server`, talking to two fake upstreams. It covers routing to each provider, unknown models, upstream failures, client cancellation and upstream timeouts reaching the upstream, a stalled TLS handshake hitting the connect timeout, and graceful and forced shutdown. |

Tests coordinate with channels. Timeouts are used only as failure guards. CI runs `gofmt`, `go vet`, `go test -race`, and `go build ./cmd/gateway`.
