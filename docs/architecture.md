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
| `internal/provider/openai`, `internal/provider/anthropic` | *(planned)* Provider HTTP clients with private wire types. |
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
