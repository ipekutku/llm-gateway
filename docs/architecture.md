# Architecture

This document describes the implemented gateway through v0.6 observability. The request path has client authentication and per-client rate limits, a vendor-neutral provider contract, OpenAI and Anthropic adapters, static routing with provider fallback, upstream timeouts, bounded retries, a circuit breaker per provider, and asynchronous usage persistence with estimated costs. Structured logs, bounded Prometheus metrics, optional OpenTelemetry traces, and a local Grafana dashboard make that behavior observable.

## Request flow

```text
HTTP request → httpapi (auth → rate limit) → routing (fallback) → breaker → retry → provider adapter → upstream HTTP request
                  └──────────────────────────── shared internal/llm types ────────────────────────────┘
                  └─ validated request outcome → usage.Recorder → postgres → PostgreSQL
                  └─ every request outcome → metrics ← GET /metrics on a separate listener
                  └─ spans: server request → route → provider → each attempt → OTLP/HTTP exporter (optional)
```

The handler authenticates the client and applies its rate limits before reading the request body (see [Client authentication](#client-authentication) and [Rate limiting](#rate-limiting)). The incoming request's `context.Context`, carrying the client's identity, request ID, and arrival time, is passed through every step to the outbound upstream request. The handler derives it once, adding the upstream deadline (see [Upstream timeouts](#upstream-timeouts)); upstream work never detaches from it. Usage persistence deliberately has a separate lifetime, described in [Asynchronous usage recording](#asynchronous-usage-recording).

## Packages and dependency boundaries

| Package | Responsibility |
|---|---|
| `internal/llm` | Vendor-neutral types (`ChatRequest`, `ChatResponse`, `Message`, `Usage`), the `Provider` interface, shared errors, and the per-request `Stats` of retries and fallback carried in the context. |
| `internal/routing` | Exact-match static model router. |
| `internal/retry` | Bounded retries of transient upstream failures, as an `llm.Provider` that wraps another. |
| `internal/breaker` | Circuit breaker for one provider, as an `llm.Provider` that wraps another. |
| `internal/auth` | Gateway client API keys and the request's client identity. |
| `internal/ratelimit` | Per-client request rate and concurrency limits. |
| `internal/usage` | Model prices, cost estimation, the usage record type, and asynchronous recording. |
| `internal/postgres` | PostgreSQL connection pool, schema migrations, and storage of usage records. |
| `internal/metrics` | Prometheus collectors in a private registry, bounded label values, and the `/metrics` handler. |
| `internal/tracing` | OpenTelemetry spans: a server-span HTTP middleware and `llm.Provider` decorators for the route, each provider, and each upstream attempt. |
| `internal/httpapi` | Public wire DTOs, validation, handler, error responses. |
| `internal/provider/openai` | OpenAI Chat Completions client with private wire types. |
| `internal/provider/anthropic` | Anthropic Messages client with private wire types. |
| `cmd/gateway` | Environment configuration, wiring, and server lifecycle. |

Rules:

- `llm` imports no other project package. Every other package depends on it.
- Provider-specific request and response types are unexported inside their provider package.
- The HTTP layer never depends on a concrete provider. It depends only on `llm.Provider`.
- `postgres` depends on `usage`; only `cmd/gateway` constructs and owns the database store. The HTTP handler accepts a small recorder interface and a pricing table, without depending on PostgreSQL.
- Only `metrics` imports Prometheus. It depends on `llm`, `breaker` (for the state type), and `usage` (for the cost type); none of them depends on it. The HTTP handler accepts a small `Metrics` interface (`ObserveRequest`, `ObserveUsage`); retry, routing, and breaker expose optional callbacks; `cmd/gateway` connects them and owns the metrics listener.
- Only `cmd/gateway` imports the OpenTelemetry SDK and exporter; it owns the tracer provider and its shutdown. `tracing` creates spans through the OTel API, given a `TracerProvider`, and depends on `llm` and `httpapi` (for the route path). `httpapi`, `retry`, and `routing` import only the OTel API, to annotate the span already in the context (`trace.SpanFromContext`); without tracing that span is a no-op. `postgres` creates its database spans with a `TracerProvider` passed to `Open`, and `usage.Record` carries the request's `trace.SpanContext` for linking. `llm` and the provider adapters do not import OpenTelemetry.
- `llm` types carry no JSON tags. Public wire formats live in `httpapi`, and upstream wire formats live in each provider package.

## Provider contract

```go
type Provider interface {
    Chat(ctx context.Context, req ChatRequest) (ChatResponse, error)
}
```

Implementations must honor context cancellation, support concurrent calls, and must not mutate the request or its `Messages` slice. The router shares provider instances across requests, so each provider must keep per-call state local or synchronize access to shared mutable state. The contract is deliberately text-only. Content is a `string`, and there is no streaming, tool calling, or multimodal input through v0.6.

## Routing

`routing.Router` holds a static `model → Route` table and itself implements `llm.Provider`. The handler therefore needs no separate router interface. A `Route` has a provider and an optional `Fallback`: a second provider, the model name to send it, and a `PrimaryTimeout`.

- **Exact match only.** There are no aliases, prefixes, or wildcards. The requested model name is forwarded unchanged to the route's provider. The only rewriting is the configured fallback model, sent to the fallback provider.
- **Validated at construction.** `routing.New` rejects an empty table, blank model names, nil providers, and fallbacks with a blank model, a nil provider, or a non-positive primary timeout.
- **Immutable.** The table, including each fallback, is copied at construction and never modified afterwards, so concurrent requests need no locking.
- An unknown model returns an error wrapping `llm.ErrUnknownModel`. Without a fallback, the context and request are forwarded unchanged and provider errors are returned unchanged.

### Fallback

With a fallback, the router:

1. Calls the primary with the request unchanged, under a context limited to `PrimaryTimeout` (or the request's own deadline, if earlier).
2. If the primary fails and the failure qualifies, logs `primary provider failed, falling back` at warn (provider, upstream status, model, fallback model, error) and calls the fallback once, with a copy of the request carrying the fallback model and the request's own context, so it gets all the remaining time.
3. Never calls the primary again. Retries happen only inside each provider, so no retry or fallback loop is possible.

| Primary failure | Falls back | Reason |
|---|---|---|
| Rate limiting (429), server errors (5xx, 529), transport failures, unusable responses | Yes | Another provider may be healthy. |
| Primary timeout | Yes | The fallback still has the rest of the request's time. **A primary canceled mid-generation may still bill for it.** |
| Other 4xx (401, 403, 404, 413, …) | Yes | Mostly configuration problems or limits of the primary, which another provider may not share. |
| Open circuit breaker (`llm.ErrCircuitOpen`) | Yes | The primary is known to be failing. |
| Upstream 400 | No | The request itself was rejected; the client should fix it. |
| The incoming request is canceled or out of time | No | Nobody would receive the answer. |
| Errors from neither an upstream nor a breaker | No | They indicate a gateway bug. |

Decisions:

- **The client sees the last provider's result.** If the fallback fails too, its error is returned and decides the HTTP status. The primary's failure is included only as text in the error message, so for example a primary timeout cannot turn a fallback's 503 into a 504.
- **The response names the model that answered.** The provider-reported model is returned. If the fallback's upstream reports none, the router reports the fallback model, never the requested one.
- **The primary timeout applies only with a fallback.** Without one, the primary keeps the whole request budget, as in v0.2.

## Errors

| Error | Meaning |
|---|---|
| `llm.ErrUnknownModel` | No provider is configured for the requested model. |
| `llm.ErrCircuitOpen` | A provider was not called because its circuit breaker is open. It is not a `*ProviderError`: no upstream call happened. |
| `*llm.ProviderError` | An upstream call failed: non-2xx status, transport failure, or an unreadable or unusable response. It carries the provider name, the upstream status (0 if there was no response), the delay the upstream asked for in `Retry-After` (0 if none), and a wrapped cause. |

`ProviderError` implements `Unwrap`, so `errors.Is(err, context.Canceled)` and `errors.Is(err, context.DeadlineExceeded)` still work through it. It never carries credentials or raw upstream bodies.

Provider adapters must return every failure, including transport errors and cancellation, as a `*ProviderError`. Any other error type reaching the handler is treated as an unexpected gateway failure.

## HTTP API

`httpapi.New(provider, authenticator, limiter, upstreamTimeout, accounting, logger)` returns an `http.Handler` serving `POST /v1/chat/completions`. `Accounting` supplies the required recorder and pricing table, plus each provider's configured model name. The provider is normally the router. Other methods on the endpoint receive 405 with `Allow: POST`, and other paths receive 404; both use the standard `ServeMux` responses.

The endpoint implements a deliberately small subset of the OpenAI Chat Completions format. It does not claim full API or SDK compatibility. Public wire types are unexported in `httpapi` and are translated to and from the neutral `llm` types.

### Request IDs

Every request, including one rejected with 401, 404, or 405, gets an ID of 26 random base32 characters from `crypto/rand` when it arrives. It is returned in the `X-Request-ID` response header, carried in the request context, and logged as `request_id` on every line logged with that context: the handler's outcome or rejection line and the retry, fallback, and breaker logs. A successful completion's `id` is `chatcmpl-` followed by it. An `X-Request-ID` header sent by the client is ignored: the ID is gateway-owned, so a client cannot make two requests share one or inject arbitrary text into logs. Usage records are keyed by it.

### Request

| Field | Rule |
|---|---|
| `model` | Required, non-blank string. Forwarded unchanged for exact-match routing. |
| `messages` | Required, non-empty array. An optional `system` message is allowed only at index 0. Every other role must be `user` or `assistant`, and at least one such message is required. |
| `messages[].content` | Required, non-blank string. Arrays, objects, and `null` are rejected. Content is forwarded unchanged, including surrounding whitespace. |
| `max_tokens` | Optional positive integer. Absent or `null` uses the gateway default of 1024, so every provider receives the same normalized value. Provider-specific limits are left to the upstream. |
| `stream` | Optional. Absent, `null`, or `false` is accepted; `true` is rejected. |

Other rules:

- The body must arrive within 30 seconds of the headers (`bodyReadTimeout`), or the response is 408.
- The body is limited to 1 MiB and must be exactly one JSON object, optionally surrounded by whitespace. Empty bodies, `null`, non-object values, malformed JSON, and trailing values are rejected.
- Fields with the wrong JSON type are rejected.
- Unknown fields such as `temperature`, `tools`, or `n` are accepted for forward compatibility but have no effect.
- Invalid requests never reach the router.

### Response

A success returns 200 with exactly one choice:

```json
{
  "id": "chatcmpl-<request ID>",
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

`id` (the request ID with a `chatcmpl-` prefix) and `created` (Unix seconds) are gateway metadata, not upstream identifiers. `total_tokens` is the sum of the neutral input and output counts. Finish reasons are `stop`, `length`, or `content_filter`.

### Error mapping

Errors use a gateway-owned envelope:

```json
{"error": {"message": "...", "type": "server_error", "code": "upstream_error"}}
```

| Condition | Status | `code` | `type` |
|---|---|---|---|
| No `Authorization` header, several, or not `Bearer <key>` | 401 | `missing_api_key` | `authentication_error` |
| Unknown or disabled key (`auth.ErrInvalidKey`, `auth.ErrDisabledKey`) | 401 | `invalid_api_key` | `authentication_error` |
| Client over its request rate | 429 | `rate_limit_exceeded` | `rate_limit_error` |
| Client at its concurrent request limit | 429 | `concurrency_limit_exceeded` | `rate_limit_error` |
| Invalid JSON, validation failure, streaming requested | 400 | `invalid_request` | `invalid_request_error` |
| Body exceeds 1 MiB | 413 | `request_too_large` | `invalid_request_error` |
| Body not received within 30 seconds | 408 | `request_timeout` | `invalid_request_error` |
| Unknown model (`llm.ErrUnknownModel`) | 404 | `model_not_found` | `invalid_request_error` |
| Upstream 400 | 400 | `invalid_request` | `invalid_request_error` |
| Upstream 429 | 429 | `provider_rate_limited` | `rate_limit_error` |
| Other upstream status, including 401 and 403 | 502 | `upstream_error` | `server_error` |
| Transport failure, or a malformed, oversized, or unusable upstream response | 502 | `upstream_error` | `server_error` |
| `context.DeadlineExceeded` while the incoming request is still active | 504 | `upstream_timeout` | `server_error` |
| Circuit breaker open (`llm.ErrCircuitOpen`) and no fallback answered | 503 | `provider_unavailable` | `server_error` |
| Any other error, including an authenticated client without rate limits | 500 | `internal_error` | `server_error` |

Decisions:

- **Context errors are checked first.** If the incoming request's context is done, the client is gone. The handler records the cancellation in its outcome line and writes nothing. Otherwise a wrapped `DeadlineExceeded` maps to 504, and a wrapped `Canceled` maps to 502, because a canceled provider operation alone does not prove that the client disconnected.
- **Gateway-client failures are distinct from provider failures.** A client's own 429 is `rate_limit_exceeded` or `concurrency_limit_exceeded`; a provider rate limiting the gateway stays `provider_rate_limited`. A gateway 401 is always about the client's gateway key, never a provider key.
- **Upstream 401 and 403 map to 502.** They indicate a gateway credential problem, not a client mistake.
- **Messages are fixed per category.** Validation messages are gateway-authored and describe the client's mistake. Upstream messages and bodies are never returned to the caller.
- **One outcome line per validated request.** Every request that passes body validation logs exactly one `request completed` line when it finishes, whether it succeeded, failed upstream, or the client went away. It carries the requested `model`, the HTTP `status` and error `code` (the same values as the usage record, including `499 client_closed`), the `provider` that answered or failed, the `upstream_status` and `error` of a provider failure, `latency`, `retry_count` (repeated upstream attempts over all providers tried), and `fallback` (whether a fallback provider was called). Successes and client cancellations log at info, 4xx responses at warn, and 5xx responses at error. `retry_count` and `fallback` come from an `llm.Stats` value the handler puts in the request context; the retry layer and router update it, and it is read after the provider call has returned.
- **Rejections are logged once.** A request rejected before reaching a provider (401, 429, an unreadable or oversized body, or a validation failure) logs one `chat completion failed` line with its status, code, and reason instead, and is not recorded in usage.
- **No content in logs.** Logs never include credentials, prompt or completion content, or raw upstream bodies. The intermediate retry, fallback, and breaker lines remain, so the steps of a request are visible before its outcome line.
- **Request context on every line.** `httpapi.NewLogHandler` wraps the gateway's root `slog` handler and adds `request_id` and, once the client is authenticated, `client_id` from the record's context. Every layer that logs with the request's context, such as the retry, router, and breaker logs, therefore identifies the request without importing `httpapi` or `auth`. `httpapi.New` wraps a logger that is not already wrapped, so the handler's own logs carry the IDs either way and never twice. Records logged without a request context, such as startup and usage-writer logs, are unchanged.

A slow upstream that exceeds the upstream timeout produces 504 `upstream_timeout`, as long as the client is still connected.

## Provider adapters

Each adapter's constructor takes an API key, a base URL, and an `*http.Client`.

- **Base URL.** An `http` or `https` origin with an optional path prefix and without the endpoint suffix. The adapter appends its endpoint path exactly once, so `https://api.openai.com` and `http://127.0.0.1:9000/proxy/` both work. URLs with credentials, a query, or a fragment are rejected. Production uses the HTTPS provider origin; tests inject local `httptest` servers.
- **HTTP client.** `nil` means `http.DefaultClient`. The adapter adds no timeout or retry policy of its own: deadlines arrive through the request context, and connection-setup limits through the injected client's transport.
- **Redirects.** The shared client built by `cmd/gateway` refuses all redirects with `http.ErrUseLastResponse`; adapters classify the original 3xx as an upstream failure. This keeps provider keys and prompts at the configured destination. An injected client owns its own redirect policy. A configured fallback may still answer; the upstream 3xx is not retried and counts as an unusable response against the breaker.
- **Safe diagnostics.** Unexpected roles, content types, and finish reasons produce fixed errors rather than quoting response values. HTTP transport/read errors also omit raw messages, since Go's HTTP parser can quote malformed upstream bytes. Their causes remain wrapped for context checks and retry classification.
- **Requests.** Built with `http.NewRequestWithContext`, so the incoming context reaches the upstream call.
- **Responses.** Bodies are closed on every path. Successful responses are read up to 4 MiB; anything larger is an upstream failure. Non-2xx response bodies are never parsed or kept, so they cannot leak into errors or logs. Up to 64 KiB is read and discarded so the connection returns to the pool and a retry can reuse it instead of opening a new TCP and TLS connection; a larger error body closes the connection. The read observes the request context like any other.
- **Errors.** Every upstream failure is a `*llm.ProviderError`: non-2xx status, transport failure, unreadable or oversized body, malformed JSON, or a structurally unusable response. When the context is done, the context error is always wrapped, so `errors.Is(err, context.Canceled)` holds even if cancellation interrupts the body read. For non-2xx responses, the delay-seconds form of `Retry-After` is recorded in `RetryAfter`, capped at 24 hours; the HTTP-date form, invalid values, and non-positive values count as absent. A request the gateway should never produce (blank model, non-positive `MaxTokens`, no messages, unknown role) returns a plain error without calling the upstream. The handler treats that as an internal error.
- **Usage.** Never fabricated. A response without both token counts, or with a negative count, is a protocol error. `InputTokens` is always the whole prompt; `CacheReadInputTokens` and `CacheWriteInputTokens` say how much of it was read from or written to the provider's prompt cache, which providers price differently. A missing cache field counts as zero.
- **Provider.** A successful response names the adapter in `Provider`, the same name a `ProviderError` carries, so callers above the router know which provider answered after a fallback.
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
| `Usage.CacheReadInputTokens` | `usage.prompt_tokens_details.cached_tokens`; more than `prompt_tokens` is a protocol error |
| `Usage.CacheWriteInputTokens` | always 0; OpenAI reports no cache writes |

- **`max_completion_tokens`, not `max_tokens`.** OpenAI's API specification marks `max_tokens` as deprecated and "not compatible with o-series models". `max_completion_tokens` is accepted by current models. On reasoning models it also counts reasoning tokens, so a small limit can produce an empty, length-truncated answer.
- **Exactly one choice.** The response must contain one choice with an `assistant` message and string `content`. Zero or several choices, a missing message, `null` content, `tool_calls`, and `function_call` are protocol errors.
- **Refusals map to `content_filter`.** A refusal arrives as a `refusal` string instead of `content`. It is returned as the message text with finish reason `content_filter`, matching the Anthropic adapter.
- **Finish reasons.** `stop`, `length`, and `content_filter` map to the neutral values of the same name. `tool_calls`, `function_call`, and any undocumented reason are protocol errors.

Response fixtures follow the example in OpenAI's published OpenAPI specification. The smoke test verified `gpt-4o` (reported as `gpt-4o-2024-08-06`) live on 2026-10-09: completion, `stop` and `length` finish reasons, and usage, including live responses passing the cache-token checks. A manual end-to-end run the same day stored live records in PostgreSQL, including OpenAI cache reads (1,792 of 1,965 prompt tokens) costed at the cache-read price. The README lists live-verified models and what the live run did not cover.

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
| `Usage.CacheReadInputTokens` | `usage.cache_read_input_tokens` |
| `Usage.CacheWriteInputTokens` | `usage.cache_creation_input_tokens` |
| `Usage.OutputTokens` | `usage.output_tokens` |

- **Conversation rules are left to the upstream.** Anthropic constraints such as role alternation or a final assistant turn (prefill, rejected by current models) are not pre-validated. They surface as an upstream 400, which the gateway maps to 400.
- **Response content.** `text` blocks are concatenated in order with no separator. `thinking` and `redacted_thinking` blocks are reasoning, not output, and are skipped; current models such as Claude Opus 5.5 always think, so rejecting them would fail every request. Any other block type (`tool_use`, `server_tool_use`, tool results, unknown types) is a protocol error. An empty `content` array or empty text is a valid empty answer.
- **Input usage includes cached tokens.** `input_tokens` excludes prompt-cache reads and writes, while OpenAI's `prompt_tokens` includes cached tokens. Adding the cache fields keeps the neutral count comparable. The gateway does not request caching itself. Cache writes are one count; the 5-minute and 1-hour cache lifetimes, which Anthropic prices differently, are not distinguished.

Stop reasons, as documented for API version `2023-06-01`:

| `stop_reason` | Neutral finish reason |
|---|---|
| `end_turn`, `stop_sequence` | `stop` |
| `max_tokens`, `model_context_window_exceeded` | `length` |
| `refusal` | `content_filter`, keeping any partial text |
| `tool_use`, `pause_turn`, anything else | protocol error (502) |

On models that always think, thinking tokens count toward `max_tokens`. A small limit, including the gateway default of 1024, can therefore end with `length` and little or no text. The same applies to OpenAI reasoning models.

Response fixtures follow Anthropic's documented response shape. The smoke test verified `claude-opus-5-5` live on 2026-10-09: completion, the `stop` and `length` finish reasons, and usage with its cache fields, and a live response was stored with its estimated cost. Nonzero Anthropic cache reads and writes were not exercised live, because the gateway does not request caching.

## Configuration

Configuration is read once at startup in `cmd/gateway`: settings from the environment, gateway clients from `GATEWAY_CLIENTS_FILE`, and prices from `GATEWAY_PRICING_FILE`. There is no configuration framework. Changing clients or prices requires a restart.

| Variable | Rule |
|---|---|
| `OPENAI_MODEL`, `OPENAI_API_KEY` | Both absent disables OpenAI. Both present routes that one model to OpenAI. Only one present fails startup. |
| `ANTHROPIC_MODEL`, `ANTHROPIC_API_KEY` | Same rule for Anthropic. |
| `GATEWAY_CLIENTS_FILE` | Required. Path of the clients file (see below). |
| `GATEWAY_DATABASE_URL` | Required. PostgreSQL connection setting; never logged. Connection and schema checks share a 10-second startup budget. |
| `GATEWAY_PRICING_FILE` | Required. Path of the JSON pricing file, described below. |
| `GATEWAY_ADDR` | Listen address. Default `127.0.0.1:8080`. The gateway serves plain HTTP, so gateway keys would cross the network unencrypted; exposing it beyond the host needs TLS terminated in front of it. |
| `GATEWAY_METRICS_ADDR` | Listen address of the unauthenticated `/metrics` endpoint. Default `127.0.0.1:9464`. It must differ from `GATEWAY_ADDR`; a conflict fails at listen time. |
| `GATEWAY_LOG_FORMAT` | `text` (default) or `json`. Read before the rest of the configuration, so configuration errors are logged in the chosen format; any other value fails startup. |
| `OTEL_EXPORTER_OTLP_ENDPOINT`, `OTEL_EXPORTER_OTLP_TRACES_ENDPOINT` | Either one enables tracing. Each must be an absolute `http` or `https` URL; the exporter would otherwise ignore it silently and send to its default. Never included in errors. See [Tracing](#tracing). |
| `OTEL_EXPORTER_OTLP_PROTOCOL`, `OTEL_EXPORTER_OTLP_TRACES_PROTOCOL` | Absent or `http/protobuf`; anything else fails startup, since only OTLP over HTTP is supported. |
| `GATEWAY_UPSTREAM_TIMEOUT` | Upstream time budget per request, as a Go duration (`90s`, `2m`). Default `120s`. |
| `GATEWAY_UPSTREAM_CONNECT_TIMEOUT` | Limit on the upstream TCP dial and, separately, the TLS handshake. Default `10s`. |
| `GATEWAY_RETRY_MAX_ATTEMPTS` | Total attempts per request, including the first, from 1 to 10. Default `3`; `1` disables retries. |
| `GATEWAY_RETRY_BASE_DELAY` | Backoff ceiling before the first retry, doubling per retry. Default `500ms`. |
| `GATEWAY_RETRY_MAX_DELAY` | Cap on the backoff ceiling and on the `Retry-After` the gateway will wait for; must not be less than the base delay. Default `8s`. |
| `OPENAI_FALLBACK`, `ANTHROPIC_FALLBACK` | The other provider's name (`anthropic` or `openai`): that provider's configured model serves this provider's failed requests. Unset means no fallback. Both directions may be set. |
| `GATEWAY_PROVIDER_TIMEOUT` | Time limit for a primary provider that has a fallback. Default half of `GATEWAY_UPSTREAM_TIMEOUT`; an explicit value must be less than it. |
| `GATEWAY_BREAKER_FAILURES` | Consecutive failed requests that open a provider's circuit, from 1 to 100. Default `5`. |
| `GATEWAY_BREAKER_COOLDOWN` | How long an open circuit rejects requests before probing. Default `30s`. |

- **Absent means unset or blank.** A whitespace-only value counts as absent. Non-blank values, including model names, are used unchanged.
- **Startup fails if no provider is enabled, if a pair is incomplete, or if both providers declare the same model.** All pair errors are reported together. Errors name the variables and never include their values. An enabled route is never silently dropped.
- **Base URLs are fixed** to the providers' production HTTPS origins. Tests inject local fake servers through the same `config` struct, but there is no environment variable for them, so a manual run of the gateway always talks to the real providers. This keeps upstream destinations under server control and avoids sending provider keys to an arbitrary host through misconfiguration.
- **Model IDs are not defaulted.** A built-in model name would go stale; the operator always chooses.
- **Durations must be positive.** An unparsable, zero, or negative duration, an attempt count outside 1–10, or a breaker failure count outside 1–100 fails startup and is reported together with any other configuration errors.
- **Fallbacks must be usable.** A `*_FALLBACK` value other than the other provider's name, a fallback on a disabled provider, or a fallback to a disabled provider fails startup.
- The startup log names the configured models and counts the enabled and disabled clients, never logging keys or key hashes.

### Clients file

```json
{
  "clients": [
    {"id": "team-a", "key_sha256": "<64 hex characters>"},
    {"id": "team-b", "key_sha256": "<64 hex characters>", "requests_per_minute": 120, "burst": 20, "max_concurrent": 10},
    {"id": "team-old", "key_sha256": "<64 hex characters>", "disabled": true}
  ]
}
```

| Field | Rule |
|---|---|
| `id` | Required client ID: 1 to 64 ASCII letters, digits, `-`, `_`, or `.`; unique. |
| `key_sha256` | Required hex SHA-256 hash of the client's key, in either case; unique across clients. |
| `disabled` | Optional. `true` rejects the key with 401. Default `false`. |
| `requests_per_minute`, `burst`, `max_concurrent` | Optional limits from 1 to 1,000,000. Defaults `60`, `10`, and `5`. |

- **Strict.** The file must be one JSON object. Unknown fields, such as a plaintext `key`, wrong types, and trailing data fail startup, so a typo cannot silently drop a limit.
- **At least one enabled client.** Otherwise every request would be rejected, so startup fails instead.
- **Errors name the client's position and ID**, never a hash. All entry errors are reported together.
- **Hashes are not secrets**, but the file decides who may use the gateway and should be writable only by the operator.

`newHandler` builds one shared upstream `http.Client`, one adapter per enabled provider wrapped in a `retry.Provider` and then a `breaker.Breaker`, the routes with their fallbacks, the router, the authenticator and rate limiter from the clients, and the HTTP handler. A fallback reuses the other provider's wrapped client, so each provider has exactly one breaker, whether a request reaches it through its own model or as a fallback. The startup log includes the fallbacks, timeouts, retry policy, and breaker settings.

## Upstream timeouts

Two independent limits bound upstream work:

| Limit | Where | Covers |
|---|---|---|
| Upstream timeout (`GATEWAY_UPSTREAM_TIMEOUT`) | `context.WithTimeout` in the handler, around the provider call | Everything below the handler for one request: routing, connecting, sending, waiting for the response, and reading the body. Later retries (and, in Milestone 3, fallback) share this one budget. |
| Connect timeout (`GATEWAY_UPSTREAM_CONNECT_TIMEOUT`) | `net.Dialer.Timeout` and `Transport.TLSHandshakeTimeout` on the shared client | Establishing a new connection. A request still cannot exceed the upstream timeout, because the dial also observes the request context. |

Decisions:

- **One budget at the top, not one per layer.** Setting the deadline in the handler means every current and future layer below it sees the same deadline through `ctx`, and nested timeouts cannot add up past what the client was promised.
- **No `http.Client.Timeout` or `ResponseHeaderTimeout`.** Non-streaming providers send headers only after generation, so a header timeout would duplicate the request timeout. Using the context instead keeps the error a wrapped `context.DeadlineExceeded`, which the handler maps to 504.
- **Deadline versus client cancellation.** If the client disconnects, the handler writes nothing and logs the outcome as `499 client_closed`. If only the derived deadline fired, the response is 504 `upstream_timeout`.
- **Expired requests are canceled upstream.** The deadline cancels the outbound HTTP request, so the provider is not left generating for nobody. The provider may still charge for work done before cancellation.
- **Idle connections** are kept for 90 seconds (a fixed constant, `idleConnTimeout`). Other transport settings, including the proxy from the environment, are those of `http.DefaultTransport`.

## Retries

`retry.Provider` wraps one provider and implements `llm.Provider` itself. `cmd/gateway` wraps each adapter with the policy from configuration, below its circuit breaker and the router's fallback. Retrying per provider keeps the policy next to the upstream it protects, and fallback above it never nests retry loops inside each other.

### Policy

| Setting | Meaning |
|---|---|
| `MaxAttempts` | Total attempts including the first; `1` disables retries. |
| `BaseDelay` | Backoff ceiling before the second attempt. It doubles per attempt. |
| `MaxDelay` | Cap on the backoff ceiling, and the longest `Retry-After` that is waited for. |

The wait before attempt *n + 1* is a uniformly random duration in `[0, min(BaseDelay × 2^(n−1), MaxDelay)]` ("full jitter"), so many clients failing together do not retry in lockstep. If the upstream sent `Retry-After`, the wait is at least that long. If it asks for more than `MaxDelay`, retrying stops and the failure is returned at once: holding the request for a long provider-requested delay would keep a healthy fallback, or the client's own retry logic, waiting.

### What is retried

A chat completion is not idempotent. An attempt that reached the model may have produced a generation the provider bills for, and a retry produces another. Only failures where the upstream normally did no work are retried:

| Failure | Retried | Reason |
|---|---|---|
| 429 Too Many Requests | Yes | Rejected before generation. |
| 503 Service Unavailable, Anthropic 529 overloaded | Yes | Rejected before generation. |
| 502 Bad Gateway, 504 Gateway Timeout | Yes | Usually a failure in front of the model. **A provider proxy can also return these after the model generated, so a retry can duplicate a billed generation.** This risk is accepted for these transient failures. |
| Dial failure: DNS lookup, connection refused, TCP connect timeout | Yes | No request byte was sent. |
| TLS handshake failure | No | Not distinguishable from later transport failures without tracing; kept out for simplicity. |
| Transport failure after the connection was established (reset, EOF) | No | The request may have reached the model. |
| Upstream timeout or cancellation | No | The request may still be generating; the budget is spent or the client is gone. |
| Other statuses (400, 401, 403, 404, 500, …) | No | Retrying cannot fix a client or configuration error; a 500 may follow a generation. |
| Unusable 2xx response (malformed, oversized, unsupported shape) | No | The generation already happened. |
| Gateway errors (unknown model, invalid internal request) | No | Not an upstream failure. |

### Time budget and cancellation

- **One budget.** All attempts and waits share the request context, so the handler's upstream timeout bounds the whole sequence. There is no per-attempt timeout: an attempt that times out is not retried, so a separate per-attempt limit would only shorten the budget.
- **No hopeless or long waits.** A wait that would end at or after the context deadline, or a `Retry-After` above `MaxDelay`, is not started, and the last failure is returned at once. A 429 asking for a long wait therefore goes straight to the fallback if there is one, and otherwise reaches the client as 429 immediately.
- **Cancellation stops retries.** If the context is already done after an attempt, no wait starts. A cancellation or deadline during a wait ends it at once; the error then wraps both the context error and the last upstream failure, so the handler's mapping (client gone, or 504) is unchanged.
- **Retries are logged.** Each retry logs one warn line, `upstream attempt failed, retrying`, with model, provider, attempt number, upstream status, the wait (`retry_in`), and the error, plus the request and client IDs from the context. A request that recovers is therefore still visible. Each repeated attempt is also counted in the request's `llm.Stats`, reported as `retry_count` on the outcome line. As with the handler's logs, no prompt or completion content or upstream body is logged.
- **Errors stay inspectable.** After more than one attempt, the error message reports the count (`after 3 attempts: …`) and wraps the last failure, so its `*llm.ProviderError`, status, and the handler's error mapping are preserved.

## Circuit breaker

`breaker.Breaker` wraps one provider and implements `llm.Provider`. `cmd/gateway` places one breaker per provider, above that provider's `retry.Provider` and below the router's fallback:

```text
router → fallback → breaker → retry → adapter
```

Above retry, one client request is one outcome for the breaker, however many attempts it took.

### States

| State | Calls | Transition |
|---|---|---|
| Closed | Pass through. | `Failures` consecutive failures → open. A success resets the count. |
| Open | Rejected at once with an error wrapping `llm.ErrCircuitOpen` and naming the provider; the provider is not called. | After `Cooldown` → half-open, on the next call. |
| Half-open | Exactly one call passes as a probe; others are rejected as if open. | Probe success → closed. Probe failure → open for another cooldown. A probe that tells nothing (client cancellation) lets the next call probe. |

### Outcomes

| Result | Effect | Reason |
|---|---|---|
| Success | Success | |
| 429, 5xx (including 529) | Failure | The provider is unavailable to the gateway, including when it is rate limiting it. |
| Transport failure, timeout (`DeadlineExceeded`) | Failure | |
| Unusable 2xx response | Failure | The provider answered, but not usably. |
| Other 4xx (400, 401, 404, …) | Success | The provider is up; the problem is the request or the gateway's configuration. |
| Client cancellation (`Canceled`), errors not from the provider | Ignored | They say nothing about the provider's health. Ignored results neither count as a failure nor reset the count. |

Decisions:

- **Consecutive failures, not a failure rate.** A count is simple, needs no time window, and is easy to test. A rate over a window can come later if a provider fails intermittently enough to matter.
- **Stale results are ignored.** Every state change starts a new generation, and a result from a call admitted under an earlier generation is dropped. Otherwise a slow call that started while closed could close the breaker while a half-open probe is still deciding.
- **State is per process.** Several gateway instances each keep their own breaker; shared state is a Milestone 7 (Redis) concern.
- **Transitions are logged**: `circuit opened` at warn (provider, previous state, consecutive failures, cooldown), and `circuit half-open, probing provider` and `circuit closed` at info.

## Client authentication

`auth.Authenticator` maps gateway API keys to clients. Every request to the chat completions endpoint is authenticated first:

```text
Authorization: Bearer <gateway key> → httpapi → Authenticator → client identity in the request context → rate limit → router
```

Gateway keys are unrelated to provider keys: clients never see provider credentials, and a gateway key gives no access to a provider except through the gateway.

| Element | Rule |
|---|---|
| `auth.Client` | ID, SHA-256 hash of the key, and a `Disabled` flag. One key per client. |
| Client ID | 1 to 64 ASCII letters, digits, `-`, `_`, or `.`. IDs appear in logs, rate limits, and usage records. |
| `auth.New` | Rejects an empty client list, invalid or duplicate IDs, missing hashes, and two clients sharing a key, including a disabled one. The client list is copied and never changes afterwards, so lookups need no locking. |
| `Authenticate(key)` | Returns the client's `auth.Identity`, an error wrapping `auth.ErrDisabledKey` that names a disabled client, or `auth.ErrInvalidKey` for any other key, including an empty one. Errors never contain the key. |
| `auth.NewContext`, `auth.FromContext` | Carry the `Identity` in the request context, so later layers can identify the client. |

Decisions:

- **Only hashes are stored.** The gateway holds the SHA-256 hash of each key, never the key, so the key configuration cannot be used to authenticate.
- **Unsalted SHA-256, because keys are random.** A slow password hash (bcrypt, Argon2) protects low-entropy secrets against guessing. Gateway keys must be high-entropy random values (for example `openssl rand -base64 32`), which a fast hash already protects, and a fast hash keeps per-request authentication cheap.
- **Lookup by hash, not by comparing keys.** The presented key is hashed and looked up in a map. Lookup timing depends only on the hash, which an attacker cannot steer toward a valid key's hash, so no comparison over the secret runs at all. This replaces a constant-time comparison against every stored key.
- **Disabled looks invalid to the client.** Both get the same 401 `invalid_api_key` response, so a response never reveals whether a key once existed. The log records the reason and, for a disabled key, the client.
- **Authenticate before anything else.** Authentication and rate limiting run before the body is read, so an unauthenticated or limited request costs the gateway almost nothing and never reaches a provider. Requests to unknown paths or with other methods still get 404 or 405 from the `ServeMux` without authentication; those reveal only which endpoint exists.
- **Header rules.** Exactly one `Authorization` header with the scheme `Bearer` (any case), one or more spaces, and a key without whitespace. Anything else is `missing_api_key`. Every 401 carries `WWW-Authenticate: Bearer`, with `error="invalid_token"` for a rejected key, as RFC 6750 describes.
- **Identity in the context.** The handler stores the client's `auth.Identity` in the request context passed to the router, so every layer below can identify the client. The handler's logs carry it as `client_id`, and usage records store it.
- **One key per client.** Key rotation with several keys per client is deferred; replacing a key means replacing its hash.

## Rate limiting

`ratelimit.Limiter` enforces each client's limits. The handler applies it right after authentication, before the request body is read, so a limited client costs the gateway as little as possible. The concurrency slot is held until the handler returns, whatever the outcome.

Every client has two independent limits, configured as `ratelimit.Limits`:

| Limit | Setting | Behavior |
|---|---|---|
| Request rate (`request_rate`) | `RequestsPerMinute`, `Burst` | A token bucket of `Burst` requests, refilled at `RequestsPerMinute`. A request with no token left is rejected with `RetryAfter`, the exact time until the next token. |
| Concurrent requests (`concurrent_requests`) | `MaxConcurrent` | At most this many of the client's requests in progress. A request beyond it is rejected; `RetryAfter` is 0, because a slot frees when another request finishes, not at a known time. |

`Acquire(clientID)` either admits a request and returns a `release` function, which the caller must call once when the request has finished, or returns a `*ratelimit.Error` naming the client and the limit. The error matches `ratelimit.ErrLimitExceeded`. A client without configured limits returns an error wrapping `ratelimit.ErrUnknownClient`; that indicates a gateway bug, not a client mistake.

Decisions:

- **Reject, never queue.** A limited request fails at once instead of waiting. Waiting would hold connections and request time for a client that is already over its share.
- **A rejected request consumes nothing.** It neither takes a token nor counts as concurrent, so a client retrying too eagerly does not push its own recovery further away.
- **GCRA form of the token bucket.** Instead of a token count, each client stores the time its bucket would be full again. This is equivalent to a token bucket but needs only integer time arithmetic, so refills and `RetryAfter` are exact and tests are deterministic. The one rounding is the time between requests, `1m / RequestsPerMinute`, truncated to whole nanoseconds. When the rate does not divide a minute evenly (for example 7 per minute), the effective rate is higher by less than one nanosecond per request, which is negligible.
- **Fixed clients, per-client locks.** The set of clients is fixed when the limiter is built, so its memory does not grow with traffic, and each client has its own lock, so clients never wait on each other.
- **Values from 1 to 1,000,000.** The bound keeps the arithmetic far from overflow. There is no "unlimited" value.
- **State is per process.** Several gateway instances each enforce their own limits, so a client can use up to N times its limit across N instances. Shared limits are a Milestone 7 (Redis) concern.
- **Retry-After for the request rate.** A `rate_limit_exceeded` response sets `Retry-After` to `RetryAfter` in whole seconds, rounded up and at least 1, so a client that waits that long is admitted. A `concurrency_limit_exceeded` response has no `Retry-After`.
- **Logged at the handler.** Each rejection is logged once with the client and the limit, like any other failed request; the limiter itself does not log.

## Cost estimation

`internal/usage` estimates what a request cost from the usage its provider reported. The handler records the result without changing the public response.

- **Exact arithmetic.** A `Rate` is a price in millionths of a dollar per million tokens, parsed from a decimal string such as `"2.50"` or `"0.075"` (up to six decimal places, no sign or exponent, at most $1,000,000 per million tokens). A rate times a token count is a whole number of picodollars (10⁻¹² dollars), so a `Cost` is an `int64` of picodollars with no floating-point rounding, and costs sum exactly. An `int64` holds about $9.2 million per request; usage that would overflow it is rejected rather than wrapped.
- **Four rates per model.** Input, cache read, cache write, and output. Cache reads and writes are part of `Usage.InputTokens`, so only the remaining input tokens are charged at the input rate. OpenAI has no cache writes, so its cache-write rate is irrelevant and can be 0.
- **Prices are keyed by provider and configured model name**, not the model the provider reports: OpenAI answers `gpt-4o` as `gpt-4o-2024-08-06`, and dated names change without the configuration changing.
- **Unknown, never zero.** A model without a price returns `ErrNoPrice`; negative counts, more cached than input tokens, or overflow return an error. Either way the cost is unknown, and callers must not record it as 0.

Costs are estimates. They use the configured prices, which can be out of date, and cover only the usage reported for a successful upstream response, including one whose client disconnected before receiving it. Attempts that were retried, a primary that failed before a fallback answered, and requests that timed out may also be billed by the provider, but report no usage to the gateway.

### Pricing file

The file is a single JSON object with a `prices` array. Each entry requires `provider`, `model`, and four quoted decimal prices: `input`, `cache_read`, `cache_write`, and `output`, in USD per million tokens. The loader rejects unknown fields, missing or invalid rates, trailing data, blank names, and duplicate provider/model entries. Errors identify the file setting, entry index, and rate field without echoing configuration values. See the [README example](../README.md#running-locally).

An empty `prices` array is valid. A configured model with no entry has unknown cost, while reported tokens are still recorded. Because a missing price is allowed, a misspelled provider or model would otherwise go unnoticed until someone saw only `NULL` costs; startup therefore logs a warning for each configured model without a price and for each price that matches no configured model. Per-request lookups that find no price are not logged. The application passes an immutable provider-to-configured-model mapping to the handler; the provider that answered selects the price after fallback, rather than the original requested model or a dated response model name.

## PostgreSQL

`internal/postgres` stores usage records through the asynchronous recorder. The application opens the pool, checks the schema, and starts the writer before listening. An unreachable or incompatible database fails startup; loss of the database afterwards only affects recording.

- **Driver.** [pgx](https://github.com/jackc/pgx) v5 with its `pgxpool` connection pool, the project's first third-party dependency: the standard library has no PostgreSQL driver, and pgx is the maintained, widely used one. `database/sql` is not used; its generic interface would add nothing here.
- **Connecting.** `Open(ctx, url)` takes a PostgreSQL URL or key/value connection string and pings the server, so an unreachable database fails at once. Pool settings such as `pool_max_conns` go in the URL, so there is no separate pool configuration. Errors never include the URL: pgx redacts passwords from its parse errors only on a best-effort basis, so a malformed URL produces a fixed message instead. TLS is configured in the URL too. pgx's default `sslmode=prefer` encrypts when the server offers TLS but does not verify the server, so the README asks for `sslmode=verify-full` for any database not on the local host; the gateway does not enforce it, because the local development database has no TLS.
- **Retention.** Records are never deleted by the gateway. Operators delete old rows themselves; [usage.md](usage.md#retention) shows a batched delete that uses the `received_at` index.

### Schema

`usage_records` has one row per recorded, validated request, keyed by the gateway's request ID:

| Column | Type | Meaning |
|---|---|---|
| `request_id` | `text`, primary key | The request ID returned in `X-Request-ID`. |
| `received_at` | `timestamptz` | When the request arrived. |
| `duration_ms` | `integer` | How long the gateway took to answer. |
| `client_id` | `text` | The authenticated client. |
| `requested_model` | `text` | The model the client asked for. |
| `provider` | `text`, nullable | The provider that answered, or the provider identified by the final upstream error; NULL if that outcome identifies no upstream. |
| `model` | `text`, nullable | The model the provider reported; NULL on failure. |
| `status` | `smallint` | The HTTP status sent to the client; 499 (nginx's convention) if the client disconnected first. |
| `error_code` | `text`, nullable | The public error code; NULL on success. |
| `input_tokens`, `cache_read_input_tokens`, `cache_write_input_tokens`, `output_tokens` | `integer`, nullable | Token usage as reported, with the cache counts part of `input_tokens`; all NULL if unknown. |
| `cost_usd` | `numeric(24, 12)`, nullable | Estimated cost in dollars; NULL if unknown. |

- **Exact cost.** A `usage.Cost` in picodollars is written as a `numeric` with twelve decimal places, so no precision is lost and `SUM(cost_usd)` is exact.
- **Unknown is NULL.** Usage and cost are NULL when unknown, never 0, so sums and averages skip them instead of being pulled toward zero.
- **Constraints repeat the record's rules**: non-negative counts, all four counts present or none, no more cached than input tokens, no cost without usage, and a valid HTTP status.
- **Indexes** on `received_at` and on `(client_id, received_at)`, for queries over a time range, overall or per client.
- **No foreign key to clients.** Clients live in the clients file, not the database, so `client_id` is plain text and records outlive a removed client.

### Migrations

Migrations are SQL files in `internal/postgres/migrations`, named `NNNN_description.sql` and numbered from `0001` without gaps, embedded in the binary.

- **Forward-only.** There are no down migrations; a mistake is corrected by a new migration.
- **One transaction.** `Migrate` runs all pending migrations and their `schema_migrations` entries in one transaction, after taking a transaction-scoped advisory lock. PostgreSQL DDL is transactional, so a failing migration leaves the schema unchanged, and gateways migrating at the same time apply each migration once. Statements that cannot run in a transaction, such as `CREATE INDEX CONCURRENTLY`, are therefore not allowed.
- **Migration history is checked.** Applied migrations must be an exact prefix of the embedded files, including their names. Missing or renamed entries are refused. If the database has a migration the gateway does not know, a newer gateway migrated it; `Migrate` and `CheckSchema` fail rather than write to a schema this version was not built for.
- **`CheckSchema`** reports an error unless the database has applied exactly the known migrations, for a gateway that should not migrate on its own.
- **Command.** `go run ./cmd/gateway migrate` reads only `GATEWAY_DATABASE_URL`, and the OTLP variables if its spans are to be exported, opens the database, and applies pending migrations within a 1-minute budget. It needs no provider credentials, clients file, or pricing file. Normal startup checks the schema but never runs migrations.

### Inserting records

`Insert(ctx, records)` writes a batch in one round trip. pgx sends the batch with a single sync, so PostgreSQL runs it as one implicit transaction: every record is stored or none. Each row is inserted with `ON CONFLICT (request_id) DO NOTHING`, so a batch retried after an uncertain failure, such as a lost commit acknowledgment, creates no duplicates and does not overwrite stored rows. Every record is checked with `usage.Record.Validate` first; if any is invalid, nothing is sent.

### Querying usage

v0.5's query interface is direct SQL against `usage_records`; there is no admin HTTP API. [usage.md](usage.md) documents reports by client, answering provider/model, and UTC day, plus request-ID lookups. Time filters use a half-open interval on `received_at`, matching the time-range indexes. UTC day attribution uses request arrival rather than completion or insertion time.

Reports sum the stored exact `numeric` costs and retain NULL when a group has no known costs. Coverage counts distinguish a known subtotal from missing usage or pricing; cache subsets are not added to input tokens a second time. Groups by `model` describe the reported answering model, while `requested_model` describes client demand, including failures and fallback. Database reporting access is separate from gateway authentication and should use a SELECT-only role.

Pricing is applied at record creation, not query time, so changing the file does not reprice historical records. Accounting can lose records, and unsuccessful upstream attempts can incur unreported charges. Query results are therefore estimates over persisted records, not an exact bill or an exhaustive audit. Automatic retention, admin endpoints, and a database-backed clients/keys model are not implemented in v0.5.

## Asynchronous usage recording

The handler submits one record when a request that passed body validation finishes: success, unknown model, upstream failure, timeout, or client cancellation. Authentication, gateway rate-limit, body-read, body-size, JSON, and message-validation failures never reach recording. A request's arrival timestamp includes admission and body-read time; its duration ends after response writing. Records contain no prompt or completion content.

On upstream success, the record contains the answering provider, reported response model, token usage, and estimated cost if known. On failure, usage and cost are unknown, and a typed provider error identifies the final failed provider. An outcome without a typed upstream identity, such as an unknown model or open circuit, records a NULL provider. The status and error code match the gateway response. An incoming cancellation or failed response write is recorded as `499 client_closed` without sending a 499 response; already reported usage and cost are retained.

`usage.Recorder` validates and copies usage and cost values, then offers the record to a bounded channel without waiting for PostgreSQL. Its only store dependency is an `Insert(ctx, []usage.Record) error` interface, which the PostgreSQL store satisfies.

- **Bounded work.** The default queue holds 1,024 records. One writer inserts batches of at most 100, flushing a partial batch after 1 second. Each write has a 5-second timeout. Dropped records are reported every 10 seconds. These limits are explicit `RecorderOptions`, so tests use small deterministic values.
- **Overload and failure.** A full queue drops the new record. Drops are counted, and the writer logs one warning with the count at most every 10 seconds and at shutdown, so a database outage under load costs one log line per interval instead of one per request. An invalid record or one submitted after shutdown is also rejected and logged. If a batch insert fails, the writer retries it once within the same 5-second write timeout, so a dropped connection or a database failover does not lose it. The retry is safe because `Insert` skips request IDs already stored: a first attempt that committed but reported failure is not stored twice. A write that used up the timeout is not retried, so a hanging database costs one timeout per batch, not two. If the retry also fails, the writer logs the failure, drops that batch, and continues with later records. It never blocks an HTTP response on database availability. These losses mean persisted usage can be incomplete; the gateway must not present it as an exact bill.
- **Context ownership.** The writer uses a context created when the recorder starts, separate from any request context. This intentionally departs from the usual request-cancellation rule: a client disconnecting after upstream work must not cancel a record already accepted by the queue. The separate context is limited for each write and canceled if shutdown's drain deadline expires.
- **Shutdown.** `Close(ctx)` atomically stops new records, drains the queue, and waits for the writer. If its context expires, it cancels the current insert and returns the context error; queued records may be lost. The store must honor context cancellation for this bound to hold. Repeated and concurrent calls are safe.

## Metrics

`internal/metrics` exposes Prometheus metrics in the text format on `GET /metrics` of a separate listener, `GATEWAY_METRICS_ADDR` (default `127.0.0.1:9464`). The metrics listener serves nothing else, and the API listener does not serve `/metrics`.

| Metric | Type | Labels | Meaning |
|---|---|---|---|
| `gateway_requests_total` | counter | `model`, `status`, `code` | Requests to the chat completions endpoint, including authentication, rate-limit, and validation rejections. `status` is the HTTP status, or `499` when the client went away; `code` is the gateway error code, empty on success. |
| `gateway_request_duration_seconds` | histogram | `model` | Time from receiving a request to finishing it, rejections included. Buckets from 10 ms to 120 s. |
| `gateway_provider_requests_total` | counter | `provider`, `outcome` | Upstream attempts, each retry and fallback attempt included. `outcome` is `success`, `error`, `timeout` (deadline exceeded), or `canceled`. |
| `gateway_provider_request_duration_seconds` | histogram | `provider` | Duration of each upstream attempt, all outcomes. Same buckets. |
| `gateway_provider_errors_total` | counter | `provider`, `upstream_status` | Attempts with outcome `error`, by upstream HTTP status; `0` means no usable response (transport failure). Timeouts and cancellations are not errors here. |
| `gateway_provider_retries_total` | counter | `provider` | Repeated attempts, counted just before each one starts. |
| `gateway_provider_fallbacks_total` | counter | `from_provider`, `to_provider` | Requests sent to the fallback provider. |
| `gateway_provider_circuit_state` | gauge | `provider` | Circuit breaker state: `0` closed, `1` half-open, `2` open. |
| `gateway_tokens_total` | counter | `provider`, `model`, `type` | Provider-reported tokens. `type` is `input` (uncached prompt tokens), `cache_read`, `cache_write`, or `output`; the first three add up to the whole prompt. |
| `gateway_estimated_cost_dollars_total` | counter | `provider`, `model` | Estimated cost in US dollars from the configured prices. |

Go runtime (`go_*`) and process (`process_*`) metrics are included.

- **Same values as logs and usage.** A validated request is observed in the same deferred step that logs its outcome line and submits its usage record, so its status and code match both. A rejection is observed where its `chat completion failed` line is logged. Requests to other paths or with other methods (404, 405) are not counted.
- **Bounded labels.** `model` is a configured model name or `unknown`. The requested model is used, so arbitrary names sent by clients, unknown models, and requests rejected before their body was read all become `unknown` and cannot create new series. `status` and `code` come from the gateway's fixed set. Client IDs and request IDs are never labels; per-client usage is in PostgreSQL.
- **Where provider metrics come from.** `cmd/gateway` wraps each adapter with `metrics.Instrument` before the retry layer, so every attempt is timed and counted, including attempts the client never sees. What that wrapper cannot see is reported through optional callbacks in the per-provider settings `cmd/gateway` already builds: `retry.Policy.OnRetry`, `routing.Fallback.OnFallback`, and `breaker.Settings.OnStateChange`. `retry`, `routing`, and `breaker` therefore do not import Prometheus. An open circuit rejects a request without an attempt, so it shows in `gateway_requests_total` as `503 provider_unavailable`, not in the provider counters.
- **Every state change.** `OnStateChange` runs under the breaker's lock, so the gauge receives transitions in order and always ends at the current state. Open becomes half-open lazily, on the first call after the cooldown, so the gauge stays at open until then. Transitions that happen between two scrapes are not visible in the gauge.
- **Series start at zero.** For each configured provider the outcome, retry, and circuit series, and for each configured fallback its fallback series, exist from startup, so rates are defined before the first event.
- **Tokens and cost match usage records.** They are observed in the same deferred step that submits the usage record, from the record's own usage and cost, and only when the record is valid: a record the recorder would drop, such as one with more cached than input tokens, is not counted either. That includes a successful generation whose client went away (`499`), which is billed and recorded. `model` is the configured model of the provider that answered, the name prices use, not the snapshot name the provider reports, so it stays bounded and a fallback is attributed to the fallback's model. Failures without usage add nothing. A request with unknown cost adds its tokens but no cost, never a fabricated zero. `input` excludes cached tokens, unlike `InputTokens` in the usage record, so summing the token types never counts cache tokens twice.
- **Metric cost is an estimate, as a float.** `gateway_estimated_cost_dollars_total` is a float64 sum for dashboards and rates. The exact values are the integer picodollar costs in PostgreSQL; reports and reconciliation use those. Neither is a provider bill. Like all counters, the metrics reset when the process restarts, while usage records persist.
- **Private registry.** Collectors are registered in a registry owned by `metrics.Metrics`, not the global default, so tests and any library code cannot interfere with each other.
- **No authentication.** Anyone who can reach the metrics listener can read request counts per model and status, provider health, and aggregate token use and estimated cost per model, but no keys, client IDs, prompts, or completions. The loopback default keeps it local; expose it only to the scraper.
- **Dependency.** `github.com/prometheus/client_golang`, the official Prometheus Go client, is the second third-party dependency. Writing the text exposition format, histograms, and runtime metrics by hand would duplicate a maintained, widely used library. It brings `client_model`, `common`, `procfs`, and `protobuf` transitively; `make vuln` covers them.

## Tracing

Tracing is off unless `OTEL_EXPORTER_OTLP_ENDPOINT` or `OTEL_EXPORTER_OTLP_TRACES_ENDPOINT` is set. Then `cmd/gateway` builds an SDK tracer provider with a batching OTLP/HTTP exporter, and passes it to `newHandler`, which wraps the layers it already builds:

```text
POST /v1/chat/completions   server span      tracing.Handler around httpapi
└─ route                    internal span    tracing.Route around the router
   └─ provider <name>       internal span    tracing.Provider around the breaker (and so the retries)
      └─ chat <model>       client span      tracing.Attempt around the adapter, one per attempt
```

With tracing off nothing is wrapped, and the context carries no span, so the API calls in `httpapi`, `retry`, and `routing` do nothing.

| Span | Attributes |
|---|---|
| `POST /v1/chat/completions` (server) | `http.request.method`, `http.route`, `http.response.status_code`, `gateway.request_id`, `gateway.client_id`, and, for validated requests, `gen_ai.request.model`, `gateway.retry_count`, `gateway.fallback`. `error.type` is the gateway error code, as in logs and metrics, including `client_closed`. Only 5xx responses set an error status. Other paths and methods are named by the method alone, so client-chosen paths never become span names. |
| `route` | `gen_ai.request.model`; `gen_ai.provider.name` of the provider that answered. A `fallback` event carries `gateway.fallback.model` and `gateway.fallback.from_provider`. |
| `provider <name>` | `gen_ai.provider.name`, `gen_ai.request.model`. One `retry` event per wait before a repeated attempt, with `gateway.retry.failed_attempt`, `gateway.retry.delay_ms`, and the failed attempt's `http.response.status_code`. A call rejected by an open circuit is this span alone, with `error.type` `circuit_open`. |
| `chat <model>` (client) | `gen_ai.operation.name`, `gen_ai.provider.name`, `gen_ai.request.model`, `gen_ai.request.max_tokens`; on success `gen_ai.response.model`, `gen_ai.response.finish_reasons`, `gen_ai.usage.input_tokens`, `gen_ai.usage.output_tokens`, `gateway.usage.cache_read_input_tokens`, `gateway.usage.cache_write_input_tokens`; on an upstream error response `http.response.status_code`. |

- **Errors.** A failed internal or attempt span has an error status whose description is the error message, the same message the logs carry, and an `error.type`: `timeout`, `canceled`, `circuit_open`, `unknown_model`, the upstream HTTP status, `transport` for an upstream failure without a usable response, or `_OTHER`. Context errors are classified first, as in the metrics.
- **No content.** Spans carry identifiers, model names, statuses, durations, and token counts. Prompts, completions, keys, and upstream bodies are never attributes, and error messages exclude them by the `llm.ProviderError` rule. The database URL never reaches tracing.

### Database spans

`postgres.Store` creates spans for its writes when `Open` is given a `TracerProvider`:

| Span | Attributes |
|---|---|
| `INSERT usage_records` (client) | `db.system.name` `postgresql`, `db.operation.name` `INSERT`, `db.collection.name` `usage_records`, `gateway.usage.records` (records in the batch), and on success `gateway.usage.inserted` (rows inserted; fewer when stored request IDs were skipped). |
| `migrate` | `db.system.name`; on success `gateway.migrations.applied`. One child span `migration <file>` per migration applied, with `gateway.migration.version`. |

- **Own traces, linked to requests.** The recorder writes with its own context, separate from any request (see [Asynchronous usage recording](#asynchronous-usage-recording)), so a batch span starts a new trace. Each `usage.Record` carries its request's server span context, and the batch span links to every sampled one, so a write and its requests can be navigated in either direction. Unsampled request spans were never exported and are not linked. A failed batch that the recorder retries produces a second span.
- **Errors.** A failed span's `error.type` is the PostgreSQL SQLSTATE code (for example `42P01` for a missing table), `timeout`, `canceled`, or `_OTHER`; its description contains only that category. Context errors are classified first. Driver errors returned by the store omit raw server messages and connection details, which can contain rejected values or credentials, while retaining gateway-authored operation context and migration names. Wrapped causes remain available through `errors.Is` and typed matching. A migration span succeeds once its statements ran, even if the transaction is later rolled back; the `migrate` span then fails.
- **What is not traced.** Individual SQL statements, `CheckSchema`, connection setup, and pool activity have no spans. Spans never carry SQL text, parameters, record values such as request or client IDs, or the connection string.
- **Propagation.** The server span extracts W3C `traceparent` and `tracestate` from the request, so the gateway joins a client's trace. Baggage is not read. Trace context is never injected: the upstream HTTP client is not instrumented, so OpenAI and Anthropic receive no trace headers and learn nothing about the client's tracing.
- **Sampling and resource.** The SDK's defaults apply: parent-based, always sampling a new trace, so a client's sampled flag decides for its requests. `OTEL_TRACES_SAMPLER` overrides it. `service.name` is `llm-gateway` unless `OTEL_SERVICE_NAME` or `OTEL_RESOURCE_ATTRIBUTES` sets it.
- **Logs.** `httpapi.NewLogHandler` adds `trace_id` from the context to every record, so each log line of a traced request can be found from its trace and the reverse.
- **Export.** Spans are batched and exported in the background by the SDK's batch span processor; a request never waits for the collector. Export errors go to OTel's global error handler, which `cmd/gateway` sets to log a `tracing error` warning containing only a safe failure category. Initialization and shutdown errors are sanitized too: raw exporter errors can include endpoint URLs, environment values, or collector responses. Causes remain inspectable; detailed server diagnostics belong in the collector. The exporter itself reads the other `OTEL_EXPORTER_OTLP_*` settings (headers, TLS, compression, timeout).

## Local observability stack

`deploy/observability` holds a Docker Compose stack for development, started by `make observability` and removed with its data by `make observability-stop`. The gateway itself runs on the host.

| Service | Image | Role |
|---|---|---|
| `prometheus` | `prom/prometheus:v3.15.0` | Scrapes `host.docker.internal:9464` every 5 seconds; 2-day retention. `extra_hosts: host-gateway` provides the name where Docker does not. |
| `jaeger` | `jaegertracing/jaeger:2.22.0` | Receives OTLP over HTTP on 4318, stores traces in memory, and serves its UI on 16686. |
| `grafana` | `grafana/grafana:13.2.3` | Provisions the Prometheus data source and the `LLM Gateway` dashboard (`grafana/dashboards/llm-gateway.json`) read-only, as the home dashboard. |

- **Local only.** Every port is published on `127.0.0.1`, overridable through `PROMETHEUS_PORT`, `GRAFANA_PORT`, `JAEGER_UI_PORT`, and `OTLP_HTTP_PORT`. Grafana has anonymous administrator access and no login form, acceptable only because it is reachable from the local host alone. Images are pinned to exact versions.
- **Dashboard queries.** Rates use `$__rate_interval` and the overview statistics `$__range`. Latency percentiles come from the histograms with `histogram_quantile`; error rate is the share of requests with a gateway error code, including authentication, rate-limit, and `499` outcomes; provider error rate is the share of attempts that did not succeed; cost is the float metric estimate. A unit test in `internal/metrics` checks that every metric the dashboard queries is one the gateway exports, so a renamed metric fails CI.
- **Traces stay in Jaeger.** Jaeger 2.22 serves only its v3 query API, which Grafana's Jaeger data source does not use, so traces are explored in the Jaeger UI, linked from the dashboard, rather than in Grafana.
- **Verification.** On 2026-10-10 the stack ran under Colima on macOS against a temporary harness: the real handler, metrics, and OTLP exporter, with fake upstreams adding latency, `503`s, retries, fallbacks, and cache tokens. Every dashboard query returned data in Prometheus, and Jaeger received the request traces with their `retry` and `fallback` events. The Linux Docker Engine path has not been tested.

### Observability decisions

These choices were made for v0.6; each lists the alternative it rejected.

- **Metrics on a separate listener.** `/metrics` is unauthenticated, because Prometheus scrapes without a gateway key. Serving it on the API port would expose it to every API client and make its exposure follow the API's. A separate listener, loopback by default, can be firewalled or bound independently, at the cost of one more listener in startup and shutdown.
- **Local stack in `deploy/observability`.** A new top-level `deploy/` holds deployment artifacts that are not Go code or documentation; the v0.9 container and Kubernetes work belongs there too. Keeping the stack under `docs/` would mix runnable configuration with prose.
- **Jaeger, not Tempo.** One container receives OTLP and provides its own trace UI, with no storage configuration. Tempo would need a configuration file and Grafana for viewing.
- **Default metrics port 9464.** Prometheus itself defaults to 9090 and Grafana to 3000, so a local stack would collide with 9090. 9464 is the port the OpenTelemetry Prometheus exporter conventionally uses.
- **Metric names.** `docs/ROADMAP.md` lists candidate names. The implementation prefixes every metric with `gateway_`, the Prometheus convention for one application's metrics, names the units (`_seconds`, `_dollars`), and uses `gateway_tokens_total` and `gateway_estimated_cost_dollars_total` instead of `token_usage_total` and `estimated_cost_total`.
- **`code`, not `error_type`, in logs.** The roadmap's `error_type` log field is the gateway error `code`, the same value as in the public error envelope, the usage record, and the `code` metric label, so one name serves all of them.
- **Retry and fallback activity is not persisted.** `retry_count` and `fallback` are in the outcome log line and the provider metrics, but not in usage records; adding them would need a migration for data that logs and metrics already answer. They can be added when a report needs them.
- **Request IDs reach every layer through a log handler.** `httpapi.NewLogHandler` reads the IDs from the context, so retry, routing, and breaker log them without new parameters or imports. It lives in `httpapi`, which owns the request ID's context key.
- **Per-request activity in the context.** `llm.Stats` carries retry and fallback activity up to the handler through the context the layers already share, instead of new return values on `llm.Provider`.
- **Tracing through decorators and the context.** The spans that wrap a call are `llm.Provider` decorators and an HTTP middleware built in `cmd/gateway`, so no constructor changes and `breaker` does not import OpenTelemetry. What only an inner layer knows (request and client IDs, outcome, retry waits, fallback) is added to the span in the context with the OTel API, which is a no-op without tracing. This is the usual OpenTelemetry split: libraries use the API, the application configures the SDK. Callbacks like the metrics ones were rejected, because events need the request's context and attributes.
- **Incoming trace context accepted, none sent upstream.** A client's `traceparent` is honored so the gateway appears in the caller's trace. Injecting trace headers into provider requests would give the providers nothing they use and would reveal the client's trace IDs to them.
- **OTLP over HTTP only, configured by the standard variables.** The standard `OTEL_*` variables configure the exporter, so no gateway-specific settings duplicate them; the gateway only validates the endpoint and protocol, which the exporter would otherwise accept silently. OTLP over gRPC is not offered.
- **Dependencies.** `go.opentelemetry.io/otel` (API and SDK) and its `otlptracehttp` exporter, the official OpenTelemetry Go implementation. Writing the OTLP protobuf encoding, batching, and retrying export by hand would duplicate them. Choosing the HTTP exporter does not avoid gRPC: the exporter uses the OTLP collector package, whose generated code includes gRPC stubs, so `google.golang.org/grpc`, `grpc-gateway`, and `golang.org/x/net` are linked in. The gateway binary grows from about 19.8 MB to 28.5 MB. `make vuln` covers them; `golang.org/x/net` is pinned at v0.60.0, the first version without the HTTP/2 vulnerabilities govulncheck reports in v0.59.0.
- **Callbacks in settings, not new constructor parameters.** `OnRetry`, `OnFallback`, and `OnStateChange` are optional fields of the per-provider settings that `cmd/gateway` already builds, so constructors keep their signatures and these packages never import Prometheus.

## Server lifecycle

- `http.Server` sets `ReadHeaderTimeout` to 5 seconds and `IdleTimeout` to 2 minutes. Without `IdleTimeout`, net/http would fall back to `ReadTimeout`, and with both unset an idle keep-alive connection would never be closed.
- The handler sets a 30-second read deadline (`bodyReadTimeout`) on every request's connection when the request arrives. It bounds reading the body, and also the server's discarding of the unread body of a request rejected before its body was read (401, 429, 404, 405), so a stalled sender cannot hold a connection. Once the body has been read, net/http clears the deadline itself when it starts watching for a client disconnect, so the upstream wait is not limited by it; a test guards that behavior.
- `ReadTimeout` and `WriteTimeout` stay unset on purpose: they apply to the whole request, including the upstream wait of up to `GATEWAY_UPSTREAM_TIMEOUT`, which bounds handler time instead.
- `signal.NotifyContext` cancels on `SIGINT` or `SIGTERM`. Shutdown then runs with a fresh 5-second context; the signal context is already canceled.
- During graceful shutdown the listener closes and in-flight requests finish normally. If they are still running after 5 seconds, the server is closed. That cancels their request contexts, and the cancellation propagates to the upstream calls. The process then exits non-zero.
- After HTTP shutdown, canceled handlers and the recorder share another 5-second budget. An application-owned handler tracker stops admitting new work and waits for accepted handlers to enqueue their final records before closing the recorder. The queue is then drained or canceled, and the database pool closes afterwards. Accounting cleanup also runs on startup/listen errors after the writer was created.
- The metrics listener is opened right after the API listener, and both start serving together. Cancellation shuts both down with the same 5-second budget; if either server fails, the other is shut down too. The metrics server has a 5-second read timeout and a 10-second write timeout, since it never waits for an upstream.
- With tracing enabled, the tracer provider is created before the database is opened and shut down last, after the database pool closes, with a 5-second budget of its own. It therefore flushes the spans of finished requests and of the final usage writes, even when draining the recorder used up its budget. Spans still pending when its budget runs out are lost. The `migrate` command flushes its spans the same way before exiting.
- `http.ErrServerClosed` counts as a normal stop. Configuration, listen, and serve errors are logged and exit with status 1.

These limits govern the server's own lifecycle and are separate from the upstream timeouts. A shutdown can therefore cut off a request that is still within its upstream timeout.

## Testing

Automated tests use synthetic credentials, fake upstream HTTP servers, and fake recorders/stores, with no paid provider calls. PostgreSQL integration tests also need a disposable PostgreSQL, named by `GATEWAY_TEST_DATABASE_URL`; without it they are skipped, except in CI, where they fail.

| Level | What it proves |
|---|---|
| `internal/auth` | Key lookup, invalid, empty, and disabled keys, client validation, and concurrent use. |
| `internal/usage` | Record validation, price parsing (decimal places, bounds, malformed values), costs with and without cache reads and writes, exact picodollar results, inconsistent and overflowing usage, and concurrent use. Recorder tests use a fake store for batching, interval flush, queue overflow with periodic and shutdown drop summaries, failed and timed-out writes, a single retry of a failed batch and none after a timeout, shutdown drain and cancellation, record snapshots, and concurrent Record/Close. |
| `internal/metrics` | Counter and histogram values per label set, model labels limited to configured names under arbitrary requested names, the text format, and runtime metrics; token types with cache tokens kept out of `input`, cost in dollars and none when unknown, attempt outcomes and error statuses through `Instrument`, retry, fallback, and circuit state values, series starting at zero, a nil `Metrics` doing nothing, and every metric queried by the provisioned Grafana dashboard being exported. `breaker`, `retry`, and `routing` tests check that `OnStateChange` reports every transition in order, `OnRetry` runs once per repeated attempt, and `OnFallback` only when falling back. |
| `internal/tracing` | Against an in-memory span recorder: server span names, kinds, status attributes, and error status only for 5xx; client-chosen paths not used as names; a valid incoming `traceparent` becoming the remote parent and a malformed one starting a new trace; `http.ResponseController` still reaching the connection; the exact attribute sets of route, provider, and attempt spans, so no content is added; `error.type` and error status per failure kind; nesting; and a nil `Tracer` wrapping nothing. `httpapi` tests check `trace_id` in logs. |
| `internal/postgres` | Migration file naming; against a real PostgreSQL, each test in its own new database: migrating an empty database, idempotent and concurrent migration, a failing migration leaving no trace, applying only pending migrations, refusing a newer schema; inserting success and failure records with exact costs and NULLs for unknown values, skipping already stored request IDs, rejecting invalid records without writing, all-or-nothing batches, cancellation, and concurrent inserts. Errors never reveal the database password. Spans, recorded in memory: one root client span per non-empty batch with its record and inserted-row counts, links only to sampled request spans, the SQLSTATE as `error.type` on failure, no record values or URL; a `migrate` span with the applied count and a child per migration, and error status on the run and the failing migration. |
| `internal/ratelimit` | Burst, refill, and sustained rates with exact `RetryAfter`, concurrency limits and release, independent clients, and concurrent use, against a manual clock. |
| `internal/routing`, `internal/httpapi` | Routing, validation, response translation, and error mapping, using fake providers; in `httpapi` also the `Authorization` header rules, 401 and 429 responses, identity in the provider's context, concurrency slots released on every outcome, `client_id` in logs, a gateway-assigned request ID on every response and in logs, the log handler adding both IDs to records logged with a request's context, and exactly one outcome line per validated request with its status, provider, retry count, and fallback, and none with prompt or completion content. `retry` and `routing` tests check the retry count and fallback recorded in `llm.Stats`. |
| `internal/provider/*` | Wire format, headers, status handling, malformed and oversized responses, transport failure, cancellation, and slow upstreams, against fake provider servers. |
| `cmd/gateway` | Configuration rules, including the log format, clients, required database/pricing settings, strict price parsing, and startup warnings for unpriced models and unused prices. Startup and lifecycle tests inject a fake store. Complete-path tests use the real handler, router, and both adapters with fake upstreams: routing, errors, retries, fallback, circuit breaking, authentication, rate limits, cancellation, deadlines, graceful/forced shutdown, and JSON logs in which every retry and outcome line carries the request and client IDs. One outcome line is logged per request, with the right `retry_count` and `fallback`, for success, upstream failure, retry exhaustion, fallback success, and client cancellation. Metrics tests count successes, upstream failures, unknown models, and authentication and validation rejections with bounded labels, and check that `/metrics` is served without a key only on the metrics listener, never on the API port. Provider metrics match retries that recover, retry exhaustion followed by a fallback, and a circuit that opens and then rejects without an attempt. Token and cost metrics equal the usage record of a direct and a fallback request with cache reads and writes; an unpriced model adds tokens but no cost; failures add neither. Accounting tests verify one record per validated outcome, cache-aware and fallback prices, NULL costs for unpriced models, and no writes for pre-validation rejections. Tracing tests use the real handler, router, and adapters with an in-memory span processor: the span tree for success, retries (one span per attempt, one `retry` event per retry), fallback (a `fallback` event and a span per provider), an unknown model, and a rejection before routing; server, attempt, and retry-event attributes; a client's `traceparent` continuing into the gateway while no upstream request carries trace headers; `trace_id` on every log line; and no prompt, completion, gateway key, provider key, or baggage in any span. A startup test exports to a fake OTLP/HTTP collector and checks that shutdown flushes the spans. Against PostgreSQL, batch insert spans start their own traces and link to the spans of the requests they store, and the `migrate` command exports its migration spans, without the database URL. Configuration tests reject invalid OTLP endpoints and protocols without printing them. A disposable PostgreSQL test verifies unmigrated startup failure, the independent migration command, and actual handler → recorder → database persistence. |

Tests coordinate with channels. Timeouts are used only as failure guards. CI runs the Makefile's `fmt`, `vet`, `test`, `build`, and `vuln` targets (`make check` locally), with a PostgreSQL service container for the integration tests. Locally, `make db` starts the same PostgreSQL image in Docker.

The v0.6 closeout adds `TestObservabilityExcludesSensitiveData`, which checks text/JSON logs, scraped metrics, and in-memory exported spans together using marker prompts, completions, gateway/provider keys, key hashes, upstream response values, and baggage. Positive assertions ensure requests reached the real adapters and all three telemetry outputs were produced. Success, retries, fallback, authentication/validation failures, and malformed upstream responses are covered. Unexpected response roles, content types, and finish reasons are reported with fixed diagnostics instead of echoing upstream strings. Additional regression tests prove that database server errors and collector responses stay out of diagnostics, and that provider redirects cannot send requests to another destination. Existing PostgreSQL tests verify linked batch writes; existing fake OTLP collector tests verify serialized export and shutdown flushing.

Release verification does not establish a throughput or latency-overhead guarantee; those measurements belong to v0.8. The documented PR 8 dashboard exercise used fake traffic on macOS/Colima. Linux stack behavior and container-image vulnerability scanning are outside `make check`. The optional live `make smoke` passed on 2026-10-10 during the v0.6 closeout: `gpt-4o` reported `gpt-4o-2024-08-06` with 15 input and 2 output tokens for its normal completion, and `claude-opus-5-5` reported the same model name with 19 input and 4 output tokens. Both normal completions ended with `stop`; both forced `length` tests and the gateway's unknown-key, missing-key, and rate-limit tests passed. The smoke harness disables metrics and tracing and uses a fake recorder, so live telemetry export and database persistence are verified separately.

A separate smoke test (`cmd/gateway/smoke_test.go`, build tag `smoke`, run with `make smoke`) uses real provider keys from the environment and the real handler and adapters against the live APIs. It checks a completion and a `length` stop with usage per configured model, plus the 401 and 429 paths. It is manual only: it costs money and needs credentials, so CI never runs it, but `make vet` compiles it so it cannot rot.
