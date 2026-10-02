# Architecture

This document describes the architecture as implemented. It grows with each Milestone 1 pull request; sections for the provider adapters and configuration are added as those parts land.

## Request flow

```text
HTTP request → httpapi → routing → provider adapter → upstream HTTP request
                  └──────── shared internal/llm types ────────┘
```

The incoming request's `context.Context` is passed unchanged through every step to the outbound upstream request.

## Packages and dependency boundaries

| Package | Responsibility |
|---|---|
| `internal/llm` | Vendor-neutral types (`ChatRequest`, `ChatResponse`, `Message`, `Usage`), the `Provider` interface, and shared errors. |
| `internal/routing` | Exact-match static model router. |
| `internal/httpapi` | Public wire DTOs, validation, handler, error responses. |
| `internal/provider/openai` | OpenAI Chat Completions client with private wire types. |
| `internal/provider/anthropic` | *(planned)* Anthropic Messages client with private wire types. |
| `cmd/gateway` | *(planned)* Configuration, wiring, and server lifecycle. |

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

`httpapi.New(provider, logger)` returns an `http.Handler` serving `POST /v1/chat/completions`. The provider is normally the router. Other methods on the endpoint receive 405 with `Allow: POST`, and other paths receive 404; both use the standard `ServeMux` responses.

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

Milestone 1 imposes no request deadline. A slow upstream runs until it completes or the client cancels. Upstream timeouts are a Milestone 2 concern; the 504 mapping is ready for them.

## Provider adapters

Each adapter's constructor takes an API key, a base URL, and an `*http.Client`.

- **Base URL.** An `http` or `https` origin with an optional path prefix and without the endpoint suffix. The adapter appends its endpoint path exactly once, so `https://api.openai.com` and `http://127.0.0.1:9000/proxy/` both work. URLs with credentials, a query, or a fragment are rejected. Production uses the HTTPS provider origin; tests inject local `httptest` servers.
- **HTTP client.** `nil` means `http.DefaultClient`. The adapter adds no timeout or retry policy of its own; that is Milestone 2.
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
- **Refusals are protocol errors.** A refusal arrives as `content: null` with a `refusal` string, so it is rejected for now.
- **Finish reasons.** `stop`, `length`, and `content_filter` map to the neutral values of the same name. `tool_calls`, `function_call`, and any undocumented reason are protocol errors.

Response fixtures follow the example in OpenAI's published OpenAPI specification. No live model has been verified yet; the README will list tested model IDs.
