# Architecture

This document describes the implemented gateway through v0.9 containers and Kubernetes. The request path has client authentication and per-client rate limits, a vendor-neutral provider contract, OpenAI and Anthropic adapters, static routing with provider fallback, upstream timeouts, bounded retries, a circuit breaker per provider, and asynchronous usage persistence with estimated costs. Structured logs, bounded Prometheus metrics, optional OpenTelemetry traces, and a local Grafana dashboard make that behavior observable. With optional Redis, several gateway instances share the rate limits and fall back to their own when Redis is unavailable. A distroless image and a Helm chart run it in Kubernetes, with health probes, a graceful shutdown drain, a migration hook, a disruption budget, and optional autoscaling; a local kind cluster runs the same chart in CI.

## Request flow

```text
HTTP request → httpapi (auth → rate limit) → routing (fallback) → breaker → retry → provider adapter → upstream HTTP request
                  └──────────────────────────── shared internal/llm types ────────────────────────────┘
                  └─ validated request outcome → usage.Recorder → postgres → PostgreSQL
                  └─ every request outcome → metrics ← GET /metrics on a separate listener
                  └─ spans: server request → route → provider → each attempt → OTLP/HTTP exporter (optional)
                  └─ rate limit: in process, or ratelimit.Fallback → Redis (optional), local limits while Redis is unavailable
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
| `internal/ratelimit` | Per-client request rate and concurrency limits, in process or shared through Redis. |
| `internal/usage` | Model prices, cost estimation, the usage record type, and asynchronous recording. |
| `internal/postgres` | PostgreSQL connection pool, schema migrations, and storage of usage records. |
| `internal/metrics` | Prometheus collectors in a private registry, bounded label values, and the `/metrics` handler. |
| `internal/tracing` | OpenTelemetry spans: a server-span HTTP middleware and `llm.Provider` decorators for the route, each provider, and each upstream attempt. |
| `internal/httpapi` | Public wire DTOs, validation, handler, error responses. |
| `internal/provider/openai` | OpenAI Chat Completions client with private wire types. |
| `internal/provider/anthropic` | Anthropic Messages client with private wire types. |
| `cmd/gateway` | Environment configuration, wiring, health probes, and server lifecycle. |
| `cmd/fakeprovider` | Standalone local performance fixture: deterministic OpenAI/Anthropic responses, cancellation-aware fixed delay, and accepted TCP connection counts. Standard library only; not wired into the gateway. |

Rules:

- `llm` imports no other project package. Every other package depends on it.
- Provider-specific request and response types are unexported inside their provider package.
- The HTTP layer never depends on a concrete provider. It depends only on `llm.Provider`.
- `postgres` depends on `usage`; only `cmd/gateway` constructs and owns the database store. The HTTP handler accepts a small recorder interface and a pricing table, without depending on PostgreSQL.
- Only `metrics` imports Prometheus. It depends on `llm`, `breaker` (for the state type), `ratelimit` (for the mode and operation names), and `usage` (for the cost type); none of them depends on it. The HTTP handler accepts a small `Metrics` interface (`ObserveRequest`, `ObserveUsage`); retry, routing, breaker, and the rate limiters expose optional callbacks; `cmd/gateway` connects them and owns the metrics listener.
- Only `cmd/gateway` imports the OpenTelemetry SDK and exporter; it owns the tracer provider and its shutdown. `tracing` creates spans through the OTel API, given a `TracerProvider`, and depends on `llm` and `httpapi` (for the route path). `httpapi`, `retry`, and `routing` import only the OTel API, to annotate the span already in the context (`trace.SpanFromContext`); without tracing that span is a no-op. `postgres` creates its database spans with a `TracerProvider` passed to `Open`, and `usage.Record` carries the request's `trace.SpanContext` for linking. `llm` and the provider adapters do not import OpenTelemetry.
- Only `ratelimit` imports go-redis. `ratelimit.Shared` accepts a `redis.Scripter`, so its caller creates and owns the Redis client.
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

`httpapi.New(provider, authenticator, limiter, upstreamTimeout, accounting, metrics, logger)` returns an `http.Handler` serving `POST /v1/chat/completions`. The limiter is the small `httpapi.Limiter` interface, which `ratelimit.Limiter` implements (see [Rate limiting](#rate-limiting)). `Accounting` supplies the required recorder and pricing table, plus each provider's configured model name. The provider is normally the router. Other methods on the endpoint receive 405 with `Allow: POST`, and other paths receive 404; both use the standard `ServeMux` responses.

The endpoint implements a deliberately small subset of the OpenAI Chat Completions format. It does not claim full API or SDK compatibility. Public wire types are unexported in `httpapi` and are translated to and from the neutral `llm` types.

### Health probes

`cmd/gateway` answers two probe paths on the API listener before any other handler, in `withProbes`:

| Path | `200 ok` | `503 shutting down` |
|---|---|---|
| `GET /livez` | While the process serves HTTP | Never |
| `GET /readyz` | From startup until shutdown begins | Once shutdown begins |

`HEAD` is answered like `GET`; other methods get `405` with `Allow: GET, HEAD`. Bodies are fixed plain text (`Cache-Control: no-store`) and reveal no configuration, dependency, or version detail. Every other path, including `/livez/`, goes to the API handler unchanged.

Decisions:

- **On the API listener, without authentication.** The kubelet probes the pod address without a gateway key. Serving the probes on the metrics listener would force `/metrics` (and pprof, when enabled) onto the pod network just so they can be reached. The probes reveal only that the process is up and whether it is shutting down.
- **Ahead of every other layer.** Probes are answered before the handler tracker, the tracing middleware, request IDs, authentication, rate limits, metrics, and accounting, so they create no span, log line, request ID, metric sample, or usage record. A probe every few seconds per pod would otherwise fill the request metrics, traces, and logs. Like the API handler, a probe sets the 30-second read deadline, so a stalled request body cannot hold the connection.
- **Readiness ignores dependencies.** PostgreSQL, Redis, and the providers are not checked. Accounting is best-effort and never fails a request, rate limits fall back to local ones while Redis is unavailable, and circuit breakers and fallback handle provider failures. Failing readiness because of any of them would remove every instance from service at once and turn a dependency outage into a full gateway outage. Their health is in the logs and metrics instead.
- **Startup checks happen before listening.** The API listener opens only after the configuration, database reachability, and schema checks have passed, so a gateway that answers at all has passed them; readiness needs no separate startup state. A failed check exits the process instead.
- **Readiness follows the shutdown context.** `/readyz` answers `503` once the context whose cancellation starts the HTTP shutdown is done: on `SIGTERM` or `SIGINT`, or when either server fails. No separate flag can disagree with it.

### Request IDs

Every request other than a [health probe](#health-probes), including one rejected with 401, 404, or 405, gets an ID of 26 random base32 characters from `crypto/rand` when it arrives. It is returned in the `X-Request-ID` response header, carried in the request context, and logged as `request_id` on every line logged with that context: the handler's outcome or rejection line and the retry, fallback, and breaker logs. A successful completion's `id` is `chatcmpl-` followed by it. An `X-Request-ID` header sent by the client is ignored: the ID is gateway-owned, so a client cannot make two requests share one or inject arbitrary text into logs. Usage records are keyed by it.

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
- **Rejections are logged once.** A request rejected before reaching a provider (401, 429, an unreadable or oversized body, or a validation failure) logs one `chat completion failed` line with its status, code, and reason instead, and is not recorded in usage. A client that goes away while the rate limiter is deciding, which can happen when the limiter calls Redis, is logged at info and counted as `499 client_closed`, with nothing written; any other limiter error is a `500 internal_error`.
- **No content in logs.** Logs never include credentials, prompt or completion content, or raw upstream bodies. The intermediate retry, fallback, and breaker lines remain, so the steps of a request are visible before its outcome line.
- **Request context on every line.** `httpapi.NewLogHandler` wraps the gateway's root `slog` handler and adds `request_id` and, once the client is authenticated, `client_id` from the record's context. Every layer that logs with the request's context, such as the retry, router, and breaker logs, therefore identifies the request without importing `httpapi` or `auth`. `httpapi.New` wraps a logger that is not already wrapped, so the handler's own logs carry the IDs either way and never twice. Records logged without a request context, such as startup and usage-writer logs, are unchanged.

A slow upstream that exceeds the upstream timeout produces 504 `upstream_timeout`, as long as the client is still connected.

## Provider adapters

Each adapter's constructor takes an API key, a base URL, and an `*http.Client`.

- **Base URL.** An `http` or `https` origin with an optional path prefix and without the endpoint suffix. The adapter appends its endpoint path exactly once, so `https://api.openai.com` and `http://127.0.0.1:9000/proxy/` both work. URLs with credentials, a query, or a fragment are rejected. `cmd/gateway` defaults to the HTTPS provider origins and validates environment overrides more strictly: HTTPS for remote destinations and HTTP only for loopback hosts. Tests inject local `httptest` servers.
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
| `OPENAI_BASE_URL` | Optional origin and path prefix, without `/v1/chat/completions`. Default `https://api.openai.com`. Requires OpenAI to be enabled. |
| `ANTHROPIC_MODEL`, `ANTHROPIC_API_KEY` | Same rule for Anthropic. |
| `ANTHROPIC_BASE_URL` | Optional origin and path prefix, without `/v1/messages`. Default `https://api.anthropic.com`. Requires Anthropic to be enabled. |
| `GATEWAY_CLIENTS_FILE` | Required. Path of the clients file (see below). |
| `GATEWAY_DATABASE_URL` | Required. PostgreSQL connection setting; never logged. Connection and schema checks share a 10-second startup budget. |
| `GATEWAY_PRICING_FILE` | Required. Path of the JSON pricing file, described below. |
| `GATEWAY_ADDR` | Listen address. Default `127.0.0.1:8080`. The gateway serves plain HTTP, so gateway keys would cross the network unencrypted; exposing it beyond the host needs TLS terminated in front of it. |
| `GATEWAY_METRICS_ADDR` | Listen address of the unauthenticated `/metrics` endpoint. Default `127.0.0.1:9464`. It must differ from `GATEWAY_ADDR`; a conflict fails at listen time. |
| `GATEWAY_PPROF` | Optional `true` or `false`, with surrounding whitespace ignored. Unset, blank, or `false` disables profiling. `true` mounts profiling only on the metrics listener; any other value fails configuration validation with a fixed error. |
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
| `GATEWAY_SHUTDOWN_DELAY` | How long both servers keep serving after a shutdown signal, with readiness failing, before their listeners close. A Go duration of zero or more; default `0`. See [Server lifecycle](#server-lifecycle). |
| `GATEWAY_SHUTDOWN_TIMEOUT` | Time limit for the HTTP drain after the delay. Positive; default `5s`. |
| `GATEWAY_REDIS_URL` | Optional `redis://` or `rediss://` URL of the Redis holding shared rate limits (see [Shared limits in Redis](#shared-limits-in-redis)). Absent means in-process limits only. Parsed at startup; another scheme or a URL go-redis cannot parse fails startup with a fixed message that never includes the URL. Never logged. |

- **Absent means unset or blank.** A whitespace-only value counts as absent. Non-blank values, including model names, are used unchanged.
- **Startup fails if no provider is enabled, if a pair is incomplete, or if both providers declare the same model.** All pair errors are reported together. Errors name the variables and never include their values. An enabled route is never silently dropped.
- **Base URLs are operator-controlled.** Environment overrides let the real gateway process call local fake providers for repeatable measurements. Requests cannot choose destinations. URLs must be absolute HTTPS URLs, or HTTP with a case-insensitive `localhost` host or a literal loopback IP (IPv4 `127.0.0.0/8`, IPv6 `::1`, including IPv4-mapped loopback). Validation does not resolve DNS aliases. A host is required; an explicit port must be 1–65535. Credentials, queries (including an empty `?`), and fragments (including an empty `#`) are rejected before starting tracing, opening the database, or creating adapters. Unset or blank values keep production defaults; an override on a disabled provider fails startup. Errors name the setting but never echo URL values or parse errors, and startup logs omit URLs. Remote overrides require HTTPS, but TLS does not make a destination trustworthy: the configured host receives provider credentials and prompts, so the operator must trust it and use synthetic keys for local fakes. The shared upstream client still refuses redirects.
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

`newHandler` builds one shared upstream `http.Client`, one adapter per enabled provider wrapped in a `retry.Provider` and then a `breaker.Breaker`, the routes with their fallbacks, the router, the authenticator from the clients, and the HTTP handler. The rate limiter is built before it by `newLimiter`: the in-process limiter, or with Redis a `ratelimit.Fallback`. A fallback reuses the other provider's wrapped client, so each provider has exactly one breaker, whether a request reaches it through its own model or as a fallback. The startup log includes the fallbacks, timeouts, retry policy, breaker settings, and `rate_limits` (`local` or `shared`).

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
- **Idle connections** are kept for 90 seconds (a fixed constant, `idleConnTimeout`), up to 512 per provider origin (`maxIdleConnsPerHost`) and 1,024 in total (`maxIdleConns`, two origins). Other transport settings, including the proxy from the environment and the unlimited number of active connections per host, are those of `http.DefaultTransport`.
- **Idle pool sized for concurrency, not left at Go's default.** Over HTTP/1.1 each concurrent upstream request holds its own connection. With Go's default of two idle connections per host, every connection returning beyond the second was closed and the next request dialed again, so 500 concurrent requests churned through thousands of short-lived connections and exhausted the laptop's ephemeral ports in the v0.8 load runs. A connection is kept only after concurrent traffic opened it, and for at most 90 seconds, so a larger limit retains a recent peak instead of opening more connections. Above 512 concurrent HTTP/1.1 requests to one origin, connections beyond the limit churn again, as before. HTTP/2 origins multiplex many requests per connection and are barely affected; HTTP/2 is negotiated, not guaranteed for every provider origin or proxy. Measurements before and after are in [performance.md](performance.md#upstream-idle-pool). The active connection count is not capped: a cap would queue requests inside the transport, where only the request deadline bounds the wait.

The v0.8 [connection measurements](performance.md#pool-waits-and-protocol-reuse) first verified the default's churn: a second synchronized 500-request HTTP/1.1 wave opened 498 connections, while a local certificate-trusted HTTP/2 wave reused negotiated connections. With the enlarged pool, the same HTTP/1.1 wave opens none; `TestUpstreamConnectionReuse` asserts that for both protocols. Inbound idle timeouts are unchanged.

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
- **State is per process.** Several gateway instances each keep their own breaker. Sharing it through Redis was considered for v0.7 and deferred; see [Shared limits in Redis](#shared-limits-in-redis).
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
- **Authenticate before anything else.** Authentication and rate limiting run before the body is read, so an unauthenticated or limited request costs the gateway almost nothing and never reaches a provider. Requests to unknown paths or with other methods still get 404 or 405 from the `ServeMux` without authentication; those reveal only which endpoint exists. The [health probes](#health-probes) are also unauthenticated.
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

`Acquire(ctx, clientID)` either admits a request and returns a `release` function, which the caller must call once when the request has finished, or returns a `*ratelimit.Error` naming the client and the limit. The error matches `ratelimit.ErrLimitExceeded`. A client without configured limits returns an error wrapping `ratelimit.ErrUnknownClient`; that indicates a gateway bug, not a client mistake.

The handler depends on the `httpapi.Limiter` interface with this one method, not on `ratelimit.Limiter`, and passes the request context, which carries the request ID and the client's identity. The in-process limiter ignores the context, since it never waits or does I/O; the parameter lets a limiter that does I/O bound and cancel its calls. A `*ratelimit.Error` from any limiter is answered with 429 as below; any other error is a 500 `internal_error`.

Decisions:

- **Reject, never queue.** A limited request fails at once instead of waiting. Waiting would hold connections and request time for a client that is already over its share.
- **A rejected request consumes nothing.** It neither takes a token nor counts as concurrent, so a client retrying too eagerly does not push its own recovery further away.
- **GCRA form of the token bucket.** Instead of a token count, each client stores the time its bucket would be full again. This is equivalent to a token bucket but needs only integer time arithmetic, so refills and `RetryAfter` are exact and tests are deterministic. The one rounding is the time between requests, `1m / RequestsPerMinute`, truncated to whole nanoseconds. When the rate does not divide a minute evenly (for example 7 per minute), the effective rate is higher by less than one nanosecond per request, which is negligible.
- **Fixed clients, per-client locks.** The set of clients is fixed when the limiter is built, so its memory does not grow with traffic, and each client has its own lock, so clients never wait on each other.
- **Values from 1 to 1,000,000.** The bound keeps the arithmetic far from overflow. There is no "unlimited" value.
- **State is per process.** Several gateway instances each enforce their own limits, so a client can use up to N times its limit across N instances. Shared limits are being added in Milestone 7; see [Shared limits in Redis](#shared-limits-in-redis).
- **Retry-After for the request rate.** A `rate_limit_exceeded` response sets `Retry-After` to `RetryAfter` in whole seconds, rounded up and at least 1, so a client that waits that long is admitted. A `concurrency_limit_exceeded` response has no `Retry-After`.
- **Logged at the handler.** Each rejection is logged once with the client and the limit, like any other failed request; the limiters themselves do not log rejections. `ratelimit.Fallback` logs only its changes between shared and local limits.

### Shared limits in Redis

`ratelimit.Shared` keeps each client's request rate and concurrent requests in Redis, so gateway instances using the same Redis and key prefix share one bucket and one set of concurrency slots per client. It implements `httpapi.Limiter` like `ratelimit.Limiter`, with the same limits and the same rejections. `ratelimit.Fallback` combines the two so that a Redis outage degrades to per-instance limits (see [When Redis is unavailable](#when-redis-is-unavailable)). With `GATEWAY_REDIS_URL` set, the gateway uses a `Fallback`; otherwise the in-process limiter alone.

**Wiring.** `cmd/gateway` owns the Redis client, one `redis.Client` for a single endpoint:

| Setting | Value | Reason |
|---|---|---|
| Call timeout (`SharedOptions.Timeout`) | 250 ms | Local and same-zone Redis answers in about a millisecond; a request waits at most this long before the local limits decide it. |
| Retry interval | 1 s | How long local limits are used after a failure before Redis is probed again. |
| Lease | `BodyReadTimeout` (30 s) + `GATEWAY_UPSTREAM_TIMEOUT` + 30 s | The longest a request reads its body and waits upstream, plus time to write the response, so a slot is never freed while its request runs. With the defaults, 3 minutes. |
| `ContextTimeoutEnabled` | on | Context deadlines interrupt blocked reads, so an unresponsive Redis costs at most the call timeout. |
| `MaxRetries` | off (`-1`) | A script whose reply was lost may have run; repeating it would count the request twice. The `Fallback` handles failures instead. |
| `DialerRetries` | 1 | go-redis's default of five dial attempts with backoff would delay the switch to local limits. |
| go-redis logging | `redis.SetLogger` to the gateway's logger at debug | Its own unstructured stderr lines, such as failed dials, would bypass the log format; the `Fallback` already logs each outage once. |

These settings replace any given as URL options. The values are constants, not configuration, until a deployment needs different ones.

- **Startup does not require Redis.** Startup pings Redis within 2 seconds. If it does not answer, the gateway logs `rate limit store unreachable at startup, using local limits` and starts the `Fallback` in local mode (`FallbackSettings.StartLocal`), so the first request probes Redis. Refusing to start would make Redis a single point of failure for restarts and scaling out. PostgreSQL remains required at startup, because accounting needs its schema. An invalid URL still fails startup.
- **One endpoint.** Redis Cluster and Sentinel clients are not used. The hash-tagged keys would allow Cluster later; a managed primary endpoint covers the single-endpoint case.
- **Closed last.** The client is closed after HTTP shutdown and after the handler tracker and recorder drain, so finished handlers have released their leases first. A handler cut off by the shutdown deadline may not release its lease; it then expires.

**What Redis is for.** Redis holds only state that must be shared for several instances to behave as one: rate-limit state. It is not a general store.

| Candidate | Decision | Reason |
|---|---|---|
| Per-client request rate and concurrency | In Redis | Without it, N instances allow N times each limit. |
| Circuit breaker state | Per instance | Per-instance breakers cost at most `GATEWAY_BREAKER_FAILURES` failed requests and one probe per instance and cooldown. Sharing state would let a Redis outage affect provider routing and needs cross-instance probe rules. To revisit with v0.8 measurements. |
| Response caching | Not implemented | No requirement; listed under possible future milestones. |
| Coordination | Not needed | Migrations already serialize on a PostgreSQL advisory lock. |

**Admitting a request.** One Lua script checks and applies both limits atomically, in the in-process limiter's order, so concurrent instances cannot both take the last token or the last slot:

1. It reads the time from Redis `TIME`, in microseconds, so instances whose clocks differ still agree.
2. It removes the client's expired leases (see below), then rejects the request if the remaining leases fill `MaxConcurrent`.
3. It reads the client's theoretical arrival time, the same GCRA state the in-process limiter keeps in memory; a missing key is a full bucket. If the request is over the rate, it returns the exact wait.
4. Otherwise it moves the arrival time one interval later, stored with an expiry at the moment the bucket would be full again, and adds a lease for the request.

A rejection changes nothing but the removal of expired leases, so a rejected request consumes neither a token nor a slot.

**Concurrency slots are leases.** Each admitted request adds a random 26-character token to the client's sorted set of leases, scored by its expiry: arrival plus `SharedOptions.Lease`. The release function removes that token. A request whose release never arrives, because its instance crashed or lost Redis, therefore holds its slot only until the lease expires, instead of for good. The lease must outlast any request, or a slot could be freed while its request still runs; the gateway derives it from the body-read and upstream timeouts plus a 30-second margin. Writing the response has no deadline (the server has no `WriteTimeout`), so a handler can outlive its lease only while writing a large response to a client that does not read it; its slot is then freed early. Responses are small enough to fit the socket buffer, so this needs a client deliberately requesting long completions and not reading them. The set expires when its newest lease would, and Redis deletes it once empty.

**Release.** The release function runs once, whatever the number of calls, and removes the lease within `SharedOptions.Timeout`. It uses the request's context without its cancellation (`context.WithoutCancel`), since a request whose client went away still has to free its slot. If Redis fails or does not answer, the error is dropped and the slot stays held until its lease expires; the limiter does not log, and how such failures become visible is part of the unavailability handling still to come. Release runs synchronously when the handler finishes, so it adds one Redis round trip, bounded by the timeout, to every request.

| Element | Rule |
|---|---|
| Keys | `<prefix>{<client id>}:rate` (arrival time) and `<prefix>{<client id>}:active` (leases), prefix `llm-gateway:ratelimit:` by default. The braces are a Redis Cluster hash tag, so all of a client's keys share a slot and one script can use them. Client IDs contain no braces. |
| Interval | `1m / RequestsPerMinute`, truncated to whole microseconds, the resolution of `TIME`: a rate that does not divide a minute evenly is exceeded by under 1 µs per request. |
| Number precision | Lua numbers are doubles, exact for integers up to 2⁵³; Unix time in microseconds is far below that. Values are written with `%.0f`, since Lua's default formatting would store them in exponent notation, rounded to tenths of a second. |
| Limits | Sent with every call from the instance's own configuration, not stored in Redis. Instances must therefore use the same clients file; otherwise each applies its own limits to the shared state. |
| Script loading | `EVALSHA`, falling back to `EVAL` when Redis does not have the script cached, for example after a restart. |
| Timeout | Each call is bounded by `SharedOptions.Timeout` within the request context's deadline. A failure or timeout is an error that is not a `*ratelimit.Error`; it wraps the context error when the context ended first. The Redis client must be created with `ContextTimeoutEnabled` so that deadlines interrupt a blocked network read. |
| Unknown client | `ErrUnknownClient`, without a Redis call. |

Decisions:

- **A script, not `INCR` with expiry or a transaction.** A fixed-window counter (`INCR` + `EXPIRE`) allows bursts of twice the limit around window edges and would not match the in-process limiter's semantics. `WATCH`/`MULTI` retries under contention. A script runs atomically on the server in one round trip.
- **Redis time, not instance time.** Passing each instance's clock would let a skewed instance refill buckets early or late for every other instance.
- **Leases, not a counter.** An `INCR`/`DECR` count of requests in progress would stay too high forever after an instance crashed between the two. Expiring leases bound that loss by the lease time; the token makes each release remove only its own lease, so a repeated or late release cannot free another request's slot.
- **Redis is trusted, its replies are checked.** Redis must be reachable only by the gateways: whoever can write to it can reset or exhaust any client's limits. A reply the script cannot have produced (an unknown status, or a wait that is not positive or exceeds the burst's tolerance plus one interval) is treated as a store failure, not turned into a decision or a `Retry-After`. Keys are not separated per deployment; deployments that must not share limits use different Redis servers or databases.
- **One script for both limits.** Checking concurrency and rate in separate calls could take a token for a request that is then rejected for its concurrency, or the reverse, and would cost two round trips.
- **The store is `redis.Scripter`.** `NewShared` accepts the small go-redis interface that runs scripts, so the caller creates and owns the client and its lifecycle. Release is a one-line script for the same reason.
- **Dependency.** [go-redis](https://github.com/redis/go-redis) v9 (BSD-2-Clause), the official Redis Go client: connection pooling, `redis://` and `rediss://` URLs, and script caching. A hand-written RESP client would duplicate it; `rueidis` is less widely used. It adds `go.uber.org/atomic`, `zeebo/xxh3`, and `klauspost/cpuid` transitively (`cespare/xxhash` and `x/sys` were already present); `make vuln` covers them. With the client created, the gateway binary grows from 28.6 MB to 39.6 MB (19.1 MB to 26.5 MB stripped with `-ldflags='-s -w'`): a `redis.Client` keeps go-redis's whole command set and its connection-pool, push-notification, and maintenance-notification support, although the gateway runs only scripts. A minimal RESP client would be smaller but would reimplement pooling, TLS, authentication, and timeouts; the size is accepted for now and is a candidate to revisit with v0.9 container images.
- **Redis 8 for local and CI tests.** `make redis` and the CI service run the `redis:8` image, as `make db` pins a PostgreSQL major version. The gateway uses only `EVAL`/`EVALSHA`, `GET`, `SET … PX`, `TIME`, `PEXPIRE`, and the sorted-set commands `ZADD`, `ZREM`, `ZCARD`, and `ZREMRANGEBYSCORE`, which Redis-compatible servers such as Valkey also support.

#### When Redis is unavailable

`ratelimit.Fallback` wraps a `Shared` limiter and an in-process `Limiter` with the same client limits, and implements `httpapi.Limiter`. It is in shared mode while Redis answers and in local mode after Redis fails:

| Event | Result | Mode afterwards |
|---|---|---|
| Redis admits or rejects the request | That decision (`429` on rejection) | Shared; a local-mode probe that Redis answers logs `rate limit store recovered, using shared limits` at info |
| Redis call fails (connection refused, reset, script or server error) | The request is decided by the local limiter | Local; the first failure logs `rate limit store unavailable, using local limits` at warn, with the error and `retry_in` |
| Redis does not answer within `SharedOptions.Timeout` | Same, after waiting for the timeout | Local, as above |
| Local mode, retry interval not yet passed | Decided locally, without calling Redis | Local |
| Local mode, retry interval passed | This one request probes Redis; requests arriving meanwhile are decided locally | Shared if Redis answers; otherwise local for another interval, without another log line |
| The request's context ends during the Redis call | Its context error is returned; the handler records `499 client_closed` and writes nothing | Unchanged; a probe that ended this way lets the next request probe |
| Unknown client | `ErrUnknownClient`, without a Redis call | Unchanged, even if the request was a probe, which then falls to the next request |

- **Releases go back to the limiter that admitted.** A request admitted by Redis releases its Redis lease, and one admitted locally its local slot, whatever the mode is when it finishes. A Redis release that fails leaves the lease to expire, as described above.
- **Local limits are per instance.** In local mode every instance applies each client's full limits by itself, exactly as without Redis: across N instances a client can use up to N times its limits until Redis returns. The local limiter's state is not synchronized with Redis in either direction, so the first requests after a mode change can be admitted or rejected slightly differently than one limiter would.
- **Mode changes are logged once and reported.** Each change is one log line, never one per request, and `FallbackSettings.OnModeChange` receives it under the lock, in order, like the breaker's `OnStateChange`, so metrics can follow it.

Decisions:

- **Fail open to local limits, not closed and not unlimited.** Rejecting every request while Redis is down would make Redis a single point of failure for all traffic, which v0.7 must avoid. Admitting every request would remove the protection limits exist for. The in-process limiter is what the gateway ran before v0.7, so a Redis outage degrades to known behavior.
- **A fixed retry interval with one probe.** After a failure, Redis is not called again for `RetryInterval`, so an unresponsive Redis costs one timeout per interval rather than one per request, and then one request at a time probes it, like the breaker's half-open state. A fixed interval is simpler than exponential backoff, and probes are cheap: one script call.
- **Request cancellation says nothing about Redis.** A Redis call that ends because its request did is neither success nor failure, so a client disconnecting cannot switch the mode.

**v0.8 measurement decisions.** [Performance follow-up](performance.md#redis-and-connection-management-follow-up) measures both store round trips in the full 500-worker path and pool waits in an isolated synchronized burst. Release stays synchronous: moving it changes when concurrency capacity becomes available and requires a separate correctness/latency comparison, not just removal of a measured delay. The pinned client's base pool size is not documented as a total hard cap, but its ordinary script checkout uses a base-size semaphore; observed waits justify a future pool-sizing experiment, not an unmeasured production increase. Keep the Fallback mutex until contention is demonstrated beyond the small local-wrapper benchmark; CPU sampling alone cannot measure blocked lock time. Keep go-redis despite its measured text-symbol footprint, avoiding a custom implementation of trusted I/O boundaries. Stripping is a packaging option for a later milestone, not a change to the current build.

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
- **Connecting.** `Open(ctx, url)` takes a PostgreSQL URL or key/value connection string and pings the server, so an unreachable database fails at once. Pool settings such as `pool_max_conns` go in the URL, so there is no separate pool configuration. Errors never include the URL: pgx redacts passwords from its parse errors only on a best-effort basis, so a malformed URL produces a fixed message instead. TLS is configured in the URL too. pgx's default `sslmode=prefer` encrypts when the server offers TLS but does not verify the server, so the README asks for `sslmode=verify-full` for any database not on the local host; the gateway does not enforce it, because the local development database has no TLS. Two TLS failures have their own messages, since both are configuration mistakes the operator must find: a certificate file the URL names (`sslrootcert`, `sslcert`, `sslkey`) that cannot be read fails with `cannot read a certificate file named in the database URL` and the file system's reason, without the path; and a server certificate that TLS rejects (unknown authority, wrong host name, expired) fails with `database operation failed (tls_certificate)`. Neither falls back to an unverified connection.
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
- **In Kubernetes.** The Helm chart runs the same command as a pre-install and pre-upgrade hook Job (see [Helm chart](#helm-chart)); the pods only check the schema.
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

`internal/metrics` exposes Prometheus metrics in the text format on `GET /metrics` of a separate listener, `GATEWAY_METRICS_ADDR` (default `127.0.0.1:9464`). By default the metrics listener serves nothing else; `GATEWAY_PPROF=true` adds opt-in profiling there. The API listener serves neither metrics nor profiling.

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
| `gateway_ratelimit_store_up` | gauge | none | With Redis configured only: `1` in the rate-limit `Fallback`'s shared mode, `0` in local mode. |
| `gateway_ratelimit_store_call_duration_seconds` | histogram | `operation` | Duration of each rate-limit call to Redis, all outcomes; `operation` is `acquire` or `release`. Buckets from 0.5 ms to 250 ms, the call timeout. |
| `gateway_ratelimit_store_errors_total` | counter | `operation` | Rate-limit calls to Redis that failed or timed out. |

Go runtime (`go_*`) and process (`process_*`) metrics are included.

- **Same values as logs and usage.** A validated request is observed in the same deferred step that logs its outcome line and submits its usage record, so its status and code match both. A rejection is observed where its `chat completion failed` line is logged. Requests to other paths or with other methods (404, 405) and [health probes](#health-probes) are not counted.
- **Bounded labels.** `model` is a configured model name or `unknown`. The requested model is used, so arbitrary names sent by clients, unknown models, and requests rejected before their body was read all become `unknown` and cannot create new series. `status` and `code` come from the gateway's fixed set. Client IDs and request IDs are never labels; per-client usage is in PostgreSQL.
- **Where provider metrics come from.** `cmd/gateway` wraps each adapter with `metrics.Instrument` before the retry layer, so every attempt is timed and counted, including attempts the client never sees. What that wrapper cannot see is reported through optional callbacks in the per-provider settings `cmd/gateway` already builds: `retry.Policy.OnRetry`, `routing.Fallback.OnFallback`, and `breaker.Settings.OnStateChange`. `retry`, `routing`, and `breaker` therefore do not import Prometheus. An open circuit rejects a request without an attempt, so it shows in `gateway_requests_total` as `503 provider_unavailable`, not in the provider counters.
- **Every state change.** `OnStateChange` runs under the breaker's lock, so the gauge receives transitions in order and always ends at the current state. Open becomes half-open lazily, on the first call after the cooldown, so the gauge stays at open until then. Transitions that happen between two scrapes are not visible in the gauge.
- **Series start at zero.** For each configured provider the outcome, retry, and circuit series, and for each configured fallback its fallback series, exist from startup, so rates are defined before the first event.
- **Tokens and cost match usage records.** They are observed in the same deferred step that submits the usage record, from the record's own usage and cost, and only when the record is valid: a record the recorder would drop, such as one with more cached than input tokens, is not counted either. That includes a successful generation whose client went away (`499`), which is billed and recorded. `model` is the configured model of the provider that answered, the name prices use, not the snapshot name the provider reports, so it stays bounded and a fallback is attributed to the fallback's model. Failures without usage add nothing. A request with unknown cost adds its tokens but no cost, never a fabricated zero. `input` excludes cached tokens, unlike `InputTokens` in the usage record, so summing the token types never counts cache tokens twice.
- **Metric cost is an estimate, as a float.** `gateway_estimated_cost_dollars_total` is a float64 sum for dashboards and rates. The exact values are the integer picodollar costs in PostgreSQL; reports and reconciliation use those. Neither is a provider bill. Like all counters, the metrics reset when the process restarts, while usage records persist.
- **Rate-limit store metrics only with Redis.** The three `gateway_ratelimit_store_*` metrics have no series until `cmd/gateway` configures Redis (`AddRateLimitStore`), so a gateway without Redis does not report a store as down. The mode gauge follows `FallbackSettings.OnModeChange`, under the `Fallback`'s lock and in order, like the circuit gauge; its value at startup is the `Fallback`'s initial mode, so an unreachable Redis at startup shows `0` at once. Calls are reported by `SharedOptions.OnCall`: every admission check and release, except an admission check whose request ended first, which says nothing about Redis. A rejection is an answer from Redis, not an error. During an outage only the probes call Redis, about once per retry interval and instance, so the error rate shows that Redis is still down, and the mode gauge how long. Failed releases count as `release` errors; their slots stay held until the lease expires.
- **Private registry.** Collectors are registered in a registry owned by `metrics.Metrics`, not the global default, so tests and any library code cannot interfere with each other.
- **No authentication.** Anyone who can reach the metrics listener can read request counts per model and status, provider health, and aggregate token use and estimated cost per model, but no keys, client IDs, prompts, or completions. The loopback default keeps it local; expose it only to the scraper.
- **Dependency.** `github.com/prometheus/client_golang`, the official Prometheus Go client, is the second third-party dependency. Writing the text exposition format, histograms, and runtime metrics by hand would duplicate a maintained, widely used library. It brings `client_model`, `common`, `procfs`, and `protobuf` transitively; `make vuln` covers them.

## Profiling

`GATEWAY_PPROF=true` enables standard-library `net/http/pprof` diagnostics on the metrics listener. `cmd/gateway` explicitly registers `GET /debug/pprof/` (the index and runtime profiles), `cmdline`, `profile` (CPU), `symbol`, and `trace` on that listener's private mux. Both listeners always use explicit handlers, never `http.DefaultServeMux`: importing `net/http/pprof` registers default-mux routes even when the setting is off. When disabled, all profiling paths return 404; the API listener never exposes them. No new dependency, listener, or profiling sampling rate is added.

Profiling is a privileged, unauthenticated diagnostic capability. Keep the metrics listener on loopback when enabled; a scrape-only network is not necessarily trusted enough. CPU and heap profiles describe execution stacks and allocation sites rather than serialize request bodies or heap objects, but symbols, source paths, goroutine stacks, process arguments, and runtime diagnostics reveal internal details. Captured artifacts should be treated as sensitive, not as sanitized telemetry. Profiling and forced GC can alter performance. CPU profiling is process-global and only one capture can run at a time; block and mutex sampling remain at their existing defaults rather than being enabled opportunistically.

The regular 10-second metrics write timeout is retained. Go's timed CPU, execution-trace, and delta-profile handlers extend the connection's write deadline by the requested duration and observe request cancellation; the existing five-second graceful shutdown can still force-close a longer capture. Profiling work does not pass through chat authentication, rate limiting, usage accounting, or request tracing. See [performance.md](performance.md#profiling-the-gateway) for collection commands and measurement caveats.

## Tracing

Tracing is off unless `OTEL_EXPORTER_OTLP_ENDPOINT` or `OTEL_EXPORTER_OTLP_TRACES_ENDPOINT` is set. Then `cmd/gateway` builds an SDK tracer provider with a batching OTLP/HTTP exporter, and passes it to `newHandler`, which wraps the layers it already builds:

```text
POST /v1/chat/completions   server span      tracing.Handler around httpapi
└─ route                    internal span    tracing.Route around the router
   └─ provider <name>       internal span    tracing.Provider around the breaker (and so the retries)
      └─ chat <model>       client span      tracing.Attempt around the adapter, one per attempt
```

With tracing off nothing is wrapped, and the context carries no span, so the API calls in `httpapi`, `retry`, and `routing` do nothing. [Health probes](#health-probes) are answered outside the server span and are never traced.

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
- **Errors.** A failed span's `error.type` is the PostgreSQL SQLSTATE code (for example `42P01` for a missing table), `timeout`, `canceled`, `tls_certificate` when TLS rejected the server's certificate, or `_OTHER`; its description contains only that category. Context errors are classified first. Driver errors returned by the store omit raw server messages and connection details, which can contain rejected values or credentials, while retaining gateway-authored operation context and migration names. Wrapped causes remain available through `errors.Is` and typed matching. A migration span succeeds once its statements ran, even if the transaction is later rolled back; the `migrate` span then fails.
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
- **Dashboard queries.** Rates use `$__rate_interval` and the overview statistics `$__range`. Latency percentiles come from the histograms with `histogram_quantile`; error rate is the share of requests with a gateway error code, including authentication, rate-limit, and `499` outcomes; provider error rate is the share of attempts that did not succeed; cost is the float metric estimate. The rate-limit store row shows the minimum of `gateway_ratelimit_store_up` per scraped instance, so one instance on local limits shows as local, plus Redis call p50/p99 and errors by operation; it stays empty without Redis. A unit test in `internal/metrics` checks that every metric the dashboard queries is one the gateway exports, so a renamed metric fails CI.
- **Traces stay in Jaeger.** Jaeger 2.22 serves only its v3 query API, which Grafana's Jaeger data source does not use, so traces are explored in the Jaeger UI, linked from the dashboard, rather than in Grafana.
- **Verification.** On 2026-10-10 the stack ran under Colima on macOS against a temporary harness: the real handler, metrics, and OTLP exporter, with fake upstreams adding latency, `503`s, retries, fallbacks, and cache tokens. Every dashboard query returned data in Prometheus, and Jaeger received the request traces with their `retry` and `fallback` events. The Linux Docker Engine path has not been tested.

### Observability decisions

These choices were made for v0.6; each lists the alternative it rejected.

- **Metrics on a separate listener.** `/metrics` is unauthenticated, because Prometheus scrapes without a gateway key. Serving it on the API port would expose it to every API client and make its exposure follow the API's. A separate listener, loopback by default, can be firewalled or bound independently, at the cost of one more listener in startup and shutdown.
- **Local stack in `deploy/observability`.** A new top-level `deploy/` holds deployment artifacts that are not Go code or documentation; the v0.9 Kubernetes work belongs there too. The `Dockerfile` is the exception, at the repository root (see [Container image](#container-image)). Keeping the stack under `docs/` would mix runnable configuration with prose.
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
- `signal.NotifyContext` cancels on `SIGINT` or `SIGTERM`, and shutdown begins: `/readyz` answers `503` at once (see [Health probes](#health-probes)), and one `shutting down` line is logged with the delay.
- For `GATEWAY_SHUTDOWN_DELAY` (default `0`), both servers keep accepting connections and serving every request, including new chat completions. Kubernetes removes a terminating pod from its Service endpoints at the same time as it sends `SIGTERM`, and load balancers notice failing readiness only at their next probe, so requests can still arrive for a few seconds; the delay keeps them from meeting a closed listener. A second signal does not cut it short. From the start of the delay, every response whose headers are written, including those of requests already in flight, carries `Connection: close`, so clients finish their keep-alive connections cleanly and reconnect to instances that are still ready (`closeWhenDraining`). Otherwise a client could send a request on an idle keep-alive connection at the moment the server closes it when the delay ends, and get a connection reset it cannot safely retry for a `POST`. `http.Server.SetKeepAlivesEnabled(false)` was rejected because it also closes idle connections at once, which only moves that race to the start of the delay. If either server fails, the other one stops at once, without the delay.
- After the delay, both listeners close and in-flight requests finish normally. If they are still running after `GATEWAY_SHUTDOWN_TIMEOUT` (default `5s`), the server is closed. That cancels their request contexts, and the cancellation propagates to the upstream calls; their usage is recorded as `499 client_closed`. The process then exits non-zero.
- **The delay is in the process, not a preStop hook.** The distroless runtime image has no `sleep` binary, Kubernetes' built-in sleep action needs a recent cluster version, and an in-process delay works the same under any process manager and can be tested.
- After HTTP shutdown, canceled handlers and the recorder share another fixed 5-second budget (`cleanupTimeout`). It does not follow `GATEWAY_SHUTDOWN_TIMEOUT`: how long accounting takes to drain depends on the database, not on how long requests run, and tying them together would double the worst case. An application-owned handler tracker stops admitting new work and waits for accepted handlers to enqueue their final records before closing the recorder. The queue is then drained or canceled, and the database pool closes afterwards. Accounting cleanup also runs on startup/listen errors after the writer was created. With Redis configured, the Redis client is closed after the recorder, before the database pool.
- The metrics listener is opened right after the API listener, and both start serving together. Both observe the same delay and shut down with the same `GATEWAY_SHUTDOWN_TIMEOUT`; if either server fails, the other is shut down too. The metrics server has a 5-second read timeout and a 10-second write timeout, since it never waits for an upstream. With optional profiling enabled, Go's timed profiling handlers extend only their own write deadlines; shutdown remains bounded and can cancel a capture.
- With tracing enabled, the tracer provider is created before the database is opened and shut down last, after the database pool closes, with a 5-second budget of its own. It therefore flushes the spans of finished requests and of the final usage writes, even when draining the recorder used up its budget. Spans still pending when its budget runs out are lost. The `migrate` command flushes its spans the same way before exiting.
- `http.ErrServerClosed` counts as a normal stop. Configuration, listen, and serve errors are logged and exit with status 1.

The worst-case shutdown is therefore `GATEWAY_SHUTDOWN_DELAY` + `GATEWAY_SHUTDOWN_TIMEOUT` + 5 seconds for handlers and the recorder + 5 seconds for the trace flush, plus closing the Redis client and the database pool, which do not wait on requests. A process manager's kill timeout, such as `terminationGracePeriodSeconds`, must be longer, or the final records and spans are lost.

These limits govern the server's own lifecycle and are separate from the upstream timeouts. `GATEWAY_UPSTREAM_TIMEOUT` defaults to 120 seconds and the shutdown timeout to 5, so a shutdown can cut off a request that is still within its upstream timeout. Raising the shutdown timeout to cover the upstream timeout would make every shutdown as slow as the slowest possible request.

## Container image

The root `Dockerfile` builds two targets for `linux/amd64` and `linux/arm64`: `gateway`, the default, and `fakeprovider`, the local fixture for demonstrations and the Kubernetes demo sidecar. `make image` builds both for the local platform and loads them into Docker; `make image-check`, run in CI, builds both for both platforms without loading or pushing them. `make image-scan` scans each image and platform with Trivy, on every CI run; `make image-publish` pushes both to GHCR from `main`. Milestone 10's bootstrap also [verifies manual arm64 publication to private ECR](aws.md#bootstrap-verification--2026-10-11).

```text
pull request   test, image (build + scan), chart, terraform, kind
merge to main  the same jobs → publish: scan again → docker login (GITHUB_TOKEN) → push amd64+arm64 → ghcr.io/ipekutku/<image>:<sha>
```

```text
build    golang:1.26.9 on the builder's platform → static binaries for the target platform
runtime  gcr.io/distroless/static-debian13:nonroot, USER 65532:65532
├─ fakeprovider   /fakeprovider
└─ gateway        /gateway, GATEWAY_ADDR=:8080, EXPOSE 8080   (default, last stage)
```

Decisions:

- **Distroless `static`, not `scratch` or Alpine.** The binary needs only CA certificates for HTTPS to providers, time zone data, and a user; distroless provides them, maintained by its publishers, with no shell, package manager, or libc. `scratch` would need these copied in by hand, and Alpine adds a shell and package manager the gateway never uses.
- **A numeric non-root user.** The process runs as `65532:65532`, set explicitly rather than inherited from the base image. Kubernetes can verify `runAsNonRoot` only for a numeric user. The binary is owned by root and not writable by the process, which writes no files, so the root filesystem can be read-only.
- **Cross-compiling instead of emulating.** The build stage runs on `$BUILDPLATFORM` and sets `GOOS` and `GOARCH` from the target platform, with `CGO_ENABLED=0`. The runtime stages only copy files, so building both platforms needs no QEMU, in CI or on Apple Silicon. Go module and build caches are BuildKit cache mounts, which also let a build that ran out of memory resume where it stopped.
- **Reproducible inputs.** Both base images are pinned by the digest of their multi-platform index, with the tag kept for readability; updating means changing both. `GOTOOLCHAIN=local` makes the build fail rather than download a different Go toolchain if `go.mod` and the image disagree. `-trimpath` removes local paths from the binary.
- **A stripped binary.** `-ldflags='-s -w'` drops the symbol table and DWARF. Panics, stack traces, and pprof profiles keep function names and lines, which come from the runtime's own tables; only debuggers lose detail. The gateway binary shrinks to 26.4 MB.
- **What the image sets.** Only `GATEWAY_ADDR=:8080`, since the default loopback address is unreachable from outside the container's network namespace. `GATEWAY_METRICS_ADDR` keeps its loopback default: the metrics listener is unauthenticated, so the deployment decides whether to expose it. No other configuration, file, or credential is baked in.
- **No `HEALTHCHECK`.** The image has no HTTP client to run one, Kubernetes ignores the instruction, and the probes are `/livez` and `/readyz` (see [Health probes](#health-probes)).
- **An allowlisted build context.** `.dockerignore` excludes everything except `go.mod`, `go.sum`, `cmd/`, and `internal/`, without tests. `.git`, local planning notes, load-test results, and any local file that may hold credentials never reach the builder, even if they are added later.
- **At the repository root.** The build needs the module root as its context, and a root `Dockerfile` keeps `docker build .` working without `-f`.
- **Scanning with a digest-pinned Trivy image, not its GitHub Action.** In March 2026, compromised credentials were used to repoint Trivy's action tags and publish malicious releases and images for a few hours ([GHSA-69fq-xp46-6x23](https://github.com/aquasecurity/trivy/security/advisories/GHSA-69fq-xp46-6x23)), so a tag alone is not trusted: the Makefile runs `ghcr.io/aquasecurity/trivy:0.75.0` by the digest of its index, identical on GHCR, Docker Hub, and ECR Public when pinned. Each image is exported as a tarball and piped into the scanner, which therefore needs no bind mount (Colima shares only the home directory) and no Docker socket, and gets no credentials. A failed build gives the scanner an empty input, which fails the scan. The vulnerability database is cached in the `llm-gateway-trivy-cache` volume; telemetry and version checks are off.
- **What fails the scan.** HIGH or CRITICAL vulnerabilities with a released fix, in the Debian packages of the distroless base or in the Go binaries' modules and standard library (read from their embedded build information), and secrets in any layer. Unfixed findings are ignored, since nothing can be changed to clear them; there is no ignore file, so a finding is fixed by updating the base image digest, Go, or the module. Trivy reports modules whether or not the code reaches them, so it can fail where `make vuln` passes. A base image past its end of life gets no fix data and would pass silently, which keeps the pinned base on a supported Debian release a manual responsibility.
- **Publishing from `main` to GHCR, with no AWS credentials in GitHub.** The project's SCPs block the GitHub OIDC identity provider that keyless ECR pushes need, and long-lived AWS keys in GitHub are ruled out (see the [runbook](aws.md#ci-images-and-aws-credentials)). The `publish` job runs after every other job on a push to `main`, scans again in its own builder, and only then logs in with the job's `GITHUB_TOKEN` and pushes from the same build cache, so the pushed layers are the scanned ones. Images are tagged with the full commit SHA, never `latest`, and carry the repository source and revision labels and minimal BuildKit provenance.

On 2026-10-11, built with Docker 29.2.1 (containerd image store) under Colima on an Apple M1, the arm64 gateway image was 32.1 MB unpacked (26.4 MB binary, 5.8 MB distroless) and 9.5 MB compressed, and the fake provider image 11.5 MB unpacked. Both images ran as UID 65532 with a read-only root filesystem, all capabilities dropped, and `no-new-privileges`: `migrate` applied the schema to an empty database and nothing on a second run; the gateway, sharing the fake provider's network namespace, answered `/livez`, `/readyz`, and a chat completion that was stored as a usage row; and `docker stop` ended it gracefully with exit status 0. The amd64 image, cross-compiled on arm64, started under emulation. Colima's default 2 GiB VM once ran out of memory compiling the dependencies; the retry resumed from the build cache.

## Helm chart

`deploy/helm/llm-gateway` deploys the gateway image to Kubernetes:

```text
ServiceAccount   automountServiceAccountToken: false
ConfigMap        <release>-pricing: prices.json rendered from values.prices
Deployment       one container; env from values and Secret references; clients Secret, pricing ConfigMap, and optional database CA ConfigMap mounted read-only
Service          ClusterIP; http → 8080, metrics → 9464
Job (hook)       <release>-migrate: gateway migrate, pre-install and pre-upgrade; the optional database CA mounted read-only
PDB              maxUnavailable 1
HPA (optional)   autoscaling/v2 on the gateway container's average CPU utilization
```

Optional `sidecars` run in each pod as native sidecars: init containers with `restartPolicy: Always`.

Values pass through three checks: `values.schema.json` rejects unknown keys, wrong types (prices that are numbers instead of strings, a `fallback` naming the provider itself), and invalid enums; a template check rejects missing Secret references, no enabled provider, a fallback without both providers, a grace period too short for the shutdown settings, and `env` entries that would override a variable the chart sets; the gateway validates its configuration again at startup.

Decisions:

- **Secrets are referenced, never created.** Provider keys, `GATEWAY_DATABASE_URL`, `GATEWAY_REDIS_URL`, and the clients file come from Secrets the operator creates, named in the values. Creating them from values would put credentials in values files, Helm release history, and shell history. Secret values reach the container through `secretKeyRef` and a read-only Secret volume (mode `0440`, readable through `fsGroup`), never through the chart.
- **No PostgreSQL or Redis in the chart.** They are stateful services with their own lifecycle, backups, and credentials; the same chart then serves a local cluster with disposable stores and a cloud deployment with managed ones.
- **The pricing file is the only ConfigMap the chart creates.** It is configuration, not a secret. Its checksum is a pod annotation, so changing prices rolls the pods, which read the file only at startup. The chart cannot see changes to the Secrets it references, so those need `kubectl rollout restart`.
- **Explicit values for what the chart must coordinate, `env` for the rest.** Addresses, files, Secrets, log format, and shutdown settings are values, because the chart wires them to ports, volumes, probes, and the grace period. Other gateway settings (timeouts, retries, breaker, tracing) pass through `env` unchanged, so the chart does not duplicate every variable and its defaults. The chart sets `GATEWAY_ADDR=:8080` and `GATEWAY_METRICS_ADDR=:9464`; `:9464` listens on every interface, IPv4 and IPv6, so Prometheus can scrape the pod.
- **Shutdown values derived from one budget.** `shutdown.delaySeconds` (5) and `shutdown.timeoutSeconds` (25) become `GATEWAY_SHUTDOWN_DELAY` and `GATEWAY_SHUTDOWN_TIMEOUT`, and `terminationGracePeriodSeconds` (45) must cover their sum plus the 10 seconds of cleanup (see [Server lifecycle](#server-lifecycle)); the template refuses less. Seconds are integers, so the chart can check the sum. Requests running longer than delay + timeout are canceled.
- **Probes.** Liveness checks `/livez` every 10 seconds and restarts the container after three failures; readiness checks `/readyz` every 5 seconds and removes the pod after two. The gateway opens its listener only after the database checks, which take at most 10 seconds, so no startup probe is needed.
- **Restricted security context.** Non-root user `65532` (also the image's), group `65532`, `fsGroup` `65532`, `RuntimeDefault` seccomp, no privilege escalation, all capabilities dropped, and a read-only root filesystem: the Kubernetes "restricted" Pod Security Standard. The ServiceAccount mounts no token, because the gateway never calls the Kubernetes API.
- **ClusterIP, no Ingress.** The gateway serves plain HTTP. Exposing it needs TLS terminated in front, which is a deployment decision outside the chart. The metrics port is on the Service too, for in-cluster scraping, and is unauthenticated.
- **Migrations from a hook Job.** A `pre-install,pre-upgrade` hook runs `gateway migrate` with the release's image, and Helm waits for it before applying anything else, so new pods find the schema their startup check requires; with its own URL (below), the serving pods need no right to change the schema. Running migrations in every pod at startup was rejected: it needs DDL rights in the serving role, and a failed migration would crash-loop every pod instead of failing one release. An init container has the same problems, once per pod. The advisory lock and single transaction make repeated and concurrent runs safe. The hook delete policy `before-hook-creation,hook-succeeded` removes a successful Job and keeps a failed one for inspection until the next release replaces it. `backoffLimit: 1` retries once for a briefly unreachable database; `activeDeadlineSeconds: 240` bounds the Job, including its image pull, below Helm's default 5-minute timeout.
- **The migration Job's own identity.** Hooks run before the release's resources exist, so the Job uses the namespace's default ServiceAccount, without its token. Its pods are labeled `app.kubernetes.io/name: llm-gateway-migrate`, so the Deployment's and Service's selectors never match them; `lint.sh` checks this. It has the gateway's security context, except `fsGroup`, which it does not need without volumes. `env` is passed to it too, so tracing settings apply to migrations.
- **A separate migration URL.** `migrations.urlSecret` lets the Job connect as the schema's owner while `database.urlSecret` names a role with only `SELECT` on `schema_migrations` and `INSERT` on `usage_records`, which is all the gateway needs (`INSERT … ON CONFLICT DO NOTHING` needs no other right). Without it, the Job uses the gateway's URL.
- **A database CA bundle from a ConfigMap, named by the URL.** `sslmode=verify-full` checks the server against the image's trust store, which holds only public CAs, so a database with a private CA (Amazon RDS) needs its bundle. `database.caBundle` mounts an operator-created ConfigMap read-only at one fixed path, `/etc/llm-gateway/database-ca/ca.pem`, in both the gateway pods and the migration Job, since either may connect. The certificate is public, so a ConfigMap rather than a Secret. The chart does not rewrite the database URLs, which live in Secrets it never reads; the operator adds `sslrootcert` with that path, so the URL remains the one place TLS is configured, as for a gateway outside Kubernetes. On 2026-10-12 the published image ran `migrate` as user 65532 with a read-only root filesystem against a TLS-only PostgreSQL 18 whose certificate came from a test CA: with the CA mounted at that path it migrated over verified TLS; without `sslrootcert`, with an unrelated CA, or with nothing mounted, it refused to connect. That image logged those failures as `database operation failed (_OTHER)` or `invalid database URL`; they now name the certificate (see [PostgreSQL](#postgresql)).
- **Forward-only upgrades.** A gateway refuses a schema with migrations it does not know, so after an upgrade that adds one, pods of the previous version that restart, and the pods of a `helm rollback` to it, fail startup. Existing pods keep serving until the rollout replaces them. Later migrations must therefore keep working with the previous version's queries until its pods are gone.
- **Two replicas, spread, with a disruption budget.** Two pods by default, so one pod failing, being evicted, or being replaced leaves another serving. A topology spread constraint over `kubernetes.io/hostname` with `maxSkew: 1` and `ScheduleAnyway` prefers different nodes but still schedules on a single-node cluster; `matchLabelKeys: [pod-template-hash]` spreads each rollout's new pods on their own (Kubernetes 1.27 or later; older API servers drop the field). A PodDisruptionBudget with `maxUnavailable: 1` (rather than `minAvailable`, which would need changing with the replica count and can block drains of a one-pod release entirely) lets voluntary evictions take one pod at a time. Spreading over zones is left to the cloud deployment, whose nodes carry zone labels.
- **CPU autoscaling, off by default.** An `autoscaling/v2` HorizontalPodAutoscaler on the `gateway` container's average CPU utilization relative to its CPU request (a `ContainerResource` metric, Kubernetes 1.30 or later), so sidecars neither dilute the measurement nor need CPU requests of their own (default target 70%, 2–6 pods), enabled by `autoscaling.enabled`. It needs the metrics API, which not every cluster has, hence off by default; the template refuses it without a CPU request. CPU is a loose signal: a request spends most of its time waiting on a provider, so a pod can hold many requests in flight at little CPU, and CPU rises mainly with request volume and short or failed requests. Scaling on requests in flight would need the gateway's Prometheus metrics exposed through a custom metrics adapter such as prometheus-adapter; that adds a cluster component and is out of scope.
- **No replica count under autoscaling.** With the HPA enabled, the Deployment omits `replicas`, so an upgrade does not reset the HPA's count to the chart's. Turning autoscaling on in an upgrade removes the field, which the API server defaults to 1, until the HPA raises the Deployment to `minReplicas` within its next sync; keeping `replicas` instead would reset a scaled-out Deployment on every upgrade.
- **Sidecars as native sidecars.** The `sidecars` value adds containers to each pod as init containers with `restartPolicy: Always` (Kubernetes 1.29 or later). They start before the gateway, and on termination the kubelet stops them only after the gateway has exited. As ordinary containers, they would receive `SIGTERM` together with the gateway: a database proxy or the fake provider would go away at the start of the gateway's shutdown delay, while it still accepts requests. The roadmap plan named the value `extraContainers`; the name changed with the semantics. Container names must be unique, and each sidecar brings its own `securityContext` and `resources`; the pod's non-root user and seccomp profile apply to it too.
- **Per-pod rate limits without Redis.** Each pod enforces every client's limits alone (see [Shared limits in Redis](#shared-limits-in-redis)), so N pods admit up to N times a client's limits, and the limit moves with the replica count. `NOTES.txt` warns when a release can have more than one pod without `redis.urlSecret`.
- **Requests without limits.** The default requests (100m CPU, 128 MiB) let the scheduler place the pod. No CPU limit, which would throttle a latency-sensitive proxy; no memory limit by default, since the gateway's memory under load has not been measured in a cluster.
- **Image `llm-gateway:dev` by default.** `make image` builds that local tag; real deployments set a registry and tag. Bootstrap's manual ECR publication does not change the chart defaults.
- **Helm is a developer tool.** It stays out of `go.mod`. `make helm-lint` runs `deploy/helm/lint.sh`, which lints the chart (`--strict`) and renders it with every `ci/*-values.yaml` (minimal and full), then requires each invalid-value case to fail with its expected message. CI installs Helm 4.3.0 for it.

On 2026-10-11 the chart was also installed with Helm 4.3.0 into a temporary kind v0.27.0 cluster (Kubernetes 1.32.2) under Colima, with the minimal CI values, the `llm-gateway:dev` image, and a disposable PostgreSQL migrated by a one-off pod. The gateway pod became ready; a server-side dry run of the `restricted` Pod Security level reported no violation for it; through the Service, the probes answered `200`, a request without a key `401`, and an unknown model `404`, which was stored as a usage row and counted on the metrics port. Upgrading with a price rolled a new ReplicaSet, and the old pod logged `shutting down`, kept serving for the 5-second delay, drained, and exited. The cluster was deleted afterwards.

On the same day, chart 0.2.0 was installed the same way into an empty database, with no manual migration, a `gateway_app` role limited as above in `database.urlSecret`, and the owner's URL in `migrations.urlSecret`. The hook Job migrated, completed, and was deleted, and only then did the gateway pod start; it stored a usage row as `gateway_app`, which could not alter the table. An upgrade whose Job used a wrong password failed after two attempts, kept the failed Job, whose log showed only SQLSTATE `28P01`, and left the Deployment and its pod unchanged; the password appeared in neither the Job's log nor the release history. The next upgrade with the right Secret replaced the failed Job, applied nothing new, and rolled the pods.

Chart 0.3.0 was checked the same way, with metrics-server v0.8.1 added to the cluster. Two pods became ready, and the disruption budget allowed one disruption. Draining the only node of gateway pods evicted one, then refused the second eviction for 40 seconds, because the replacement could not be scheduled on the cordoned node, so one pod kept serving; uncordoning restored both. Enabling autoscaling (2–4 pods) in an upgrade dropped the Deployment to one ready pod until the HPA restored two, as described above. With a 1% target, idle pods sat at 1% and did not scale; about 90 seconds of unknown-model requests from four loops raised average utilization to 27–51%, and the HPA scaled to four pods. An upgrade that changed the log format then rolled all four pods and kept the Deployment at four.

## Local Kubernetes environment

`deploy/kind` holds everything needed to run the chart locally, with synthetic credentials only:

| File | Purpose |
|---|---|
| `cluster.yaml` | One control-plane node, `kindest/node:v1.37.0` pinned by digest, the image kind v0.33.0 was released with. |
| `metrics-server/kustomization.yaml` | metrics-server v0.9.0 from its release manifest, with `--kubelet-insecure-tls`, because kind's kubelets serve self-signed certificates. A kustomization keeps reapplying it idempotent. |
| `stores.yaml` | PostgreSQL 18 and Redis 8, the images of `make db` and `make redis`, without volumes and with a fixed local password. |
| `clients.json` | Three clients with public synthetic keys: `demo` (`synthetic-kind-demo-key`, 600 requests per minute), `load` (`synthetic-kind-load-key`, effectively unlimited, for load), and `limited` (`synthetic-kind-limited-key`, bursts of 10, then 1 per second). |
| `values.yaml` | Chart values: both providers at `http://127.0.0.1:9000`, the fake provider as a sidecar with a 200 ms delay, so requests are in flight during shutdowns, Redis for shared limits, autoscaling from two to four pods, synthetic prices, `pullPolicy: Never`. |
| `check.sh` | Waits for the rollout, port-forwards the Service, requires one completion per provider and a successful request on the metrics port. No timing thresholds. |
| `demo.sh` | The [demonstrations](#demonstrations) (`make kind-demo`), run by hand only. |

`make kind-up` builds both images, creates the `llm-gateway` cluster unless it exists, loads the images (they are never pushed), and starts metrics-server and the stores; `make kind-deploy` creates the Secrets with `--dry-run=client | apply`, so reruns update them, and runs `helm upgrade --install`, whose migration hook prepares the database; `make kind-check` runs `check.sh`; `make kind-down` deletes the cluster. CI runs the first three in a fourth job, and prints the namespace's resources, events, and the gateway pods' logs if they fail.

Decisions:

- **kind, run through `go run`.** kind runs Kubernetes nodes as Docker containers, so it works wherever Docker does, including Colima, and CI needs nothing else. It is a Go program, so the Makefile pins it like `govulncheck`, without an install; `kubectl` and `helm` are installed tools, preinstalled on the CI runner or set up by an action.
- **The fake provider in the pod.** Plain HTTP provider URLs are accepted only on loopback (see [Configuration](#configuration)), so the fake must share each gateway pod's network namespace; the chart's `sidecars` value puts it there, and the provider settings stay the operator-controlled base URLs they are in production. Real provider keys are never needed.
- **Stores beside the chart, not in it.** The same chart that a cloud deployment uses with managed PostgreSQL and Redis gets disposable ones here, created by plain manifests in the same namespace, reached through Secrets like any other.
- **One node.** A laptop VM holds one kind node comfortably; spreading, drains across nodes, and zone failures are not demonstrated locally.

On 2026-10-11, under Colima (2 CPUs, 2 GiB) on an Apple M1, with kubectl 1.37.0 and Helm 4.3.0 run from their official images: `make kind-up` took 4 minutes 40 seconds from nothing, including building the images; `make kind-deploy` installed the release in under 6 seconds with the migration hook; and `make kind-check` passed, with two usage rows stored with costs. The HPA read the gateway container's CPU (1% idle). A pod deleted while requests were sent to it every quarter second kept answering `200` for its 5-second shutdown delay and then refused connections, with no `502`: the fake provider, which stops at once on `SIGTERM`, was still running. Rerunning all three targets changed nothing but the release revision, and `make kind-down` removed the cluster and its kubeconfig entry.

### Demonstrations

`make kind-demo` runs `deploy/kind/demo.sh` on a cluster prepared by `make kind-up kind-deploy`: five scenarios in about twelve minutes, never in CI. Load comes from Vegeta v12.13.0, built pinned for the node's platform and run inside the kind node against the Service's cluster IP, so kube-proxy spreads its connections across the pods as it would for any in-cluster client; a port-forward would pin them to one pod. Requests go to the fake provider sidecar, which answers after 200 ms. Each run writes the Vegeta reports, gateway logs, and a `summary.md` to the gitignored `deploy/kind/results/`. The script asserts nothing about timing: the numbers are a record.

| Scenario | What happens |
|---|---|
| `scale` | 200 requests/s for 180 s as the `load` client, sampling the HPA's CPU measurement and the pods every 15 s. Starts only once the autoscaler is back at two pods: the HPA controller remembers its recent recommendations for five minutes, even for a reinstalled HPA of the same name. |
| `limits` | 30 requests for an unconfigured model in about half a second as the `limited` client, each on a new connection; an admitted request gets `404` and a limited one `429`, without calling a provider. Repeated after a `helm upgrade` that removes Redis, and restored with `make kind-deploy`. Per-pod counts come from each pod's metrics. |
| `sigterm` | One pod receives a readiness probe and a completion about every 0.3 s, directly on its IP, while it is deleted. |
| `rollout` | 50 requests/s for 90 s; `kubectl rollout restart` after 15 s. `ROLLOUT_RATE` and `ROLLOUT_RESTARTS` raise the rate and repeat the restart, for comparing builds. |
| `database` | 20 requests/s for 120 s. After 15 s PostgreSQL is scaled to zero, which deletes its data; after 60 s it is scaled back and `make kind-deploy` reruns the migration hook. Lost records are counted from the gateway logs and the rows of both databases. |

Results of 2026-10-11 (run `20261011T165145Z`; Colima on an Apple M1 with 2 CPUs and 1.9 GiB for the whole cluster, Kubernetes 1.37.0, chart 0.4.0, three gateway pods except where stated):

| Scenario | Result |
|---|---|
| `scale` | From two pods: CPU rose to 44%, then 78% of the request after 47 s, when the HPA added a third pod; with three, it settled at 50–54% and stayed below the 70% target, so a fourth was never added. 36,000 requests, all `200`, p99 205.8 ms. |
| `limits` | With Redis, 10 of 30 admitted (2, 5, and 3 by the three pods) and 20 limited: one bucket for the client across all pods. Without Redis, 29 of 30 admitted (10, 9, and 10): each pod applied the burst of 10 on its own. |
| `sigterm` | Readiness answered `503` from 0.5 s after the delete request; completions kept succeeding until 5.4 s, then connections were refused at 5.5 s. No request failed with an HTTP error. |
| `rollout` | The Deployment reported three new pods ready after 4 s, and the old pods had drained and exited after 10 s. 4,499 of 4,500 requests succeeded; one failed with `connection reset by peer`. Two earlier runs of the same scenario had no failures. The cause is fixed since; see below. |
| `database` | All three pods stayed ready throughout, and all 2,400 requests succeeded with unchanged latency (p99 208.7 ms). The migration hook prepared the new database 5 s after it was ready. Of the 2,400 usage records, 297 were in the deleted database, 1,010 were reported lost in 80 `usage records not persisted` warnings, and 1,087 were stored afterwards; the remaining 6 were written between counting the old database's rows and its deletion. |

What they show, and what they do not:

- **Shutdown drains.** A terminating pod fails readiness at once and keeps serving through its delay, so nothing new is routed to it once the endpoints update, and requests already in flight finish. The run above found the one failure mode left: when the delay ended, the server closed its idle keep-alive connections, and a client that sent a request on one at that moment got a reset, which it cannot safely retry for a `POST`. Responses during the delay now carry `Connection: close` (see [Server lifecycle](#server-lifecycle)). Measured with `ROLLOUT_RATE=200 ROLLOUT_RESTARTS=5` (before) and `ROLLOUT_RESTARTS=10` (after) on the same cluster: before, each of two runs of 42,000 requests across five restarts had one failure (`EOF`); after, two runs of 72,000 requests across ten restarts each had none. Median, p90, and p95 latency stayed at 202, 204, and 206–209 ms; p99 varied between runs on both sides (217 and 249 ms before, 462 and 224 ms after), and one run after the change had a 2.3-second maximum that the other did not, on a VM whose two CPUs also ran the load generator and ten back-to-back rollouts. A client whose connection stays idle for the whole delay can still meet the close, but only by sending at that instant.
- **Accounting losses are in the logs, not the metrics.** During the outage the gateway logged every failed batch with its record count, but there is no metric for lost records, so `gateway_requests_total` alone cannot show them; comparing it with stored rows, or watching the warnings, can.
- **CPU is a coarse scaling signal.** Requests here cost little CPU and mostly wait on the fake provider's 200 ms, so 200 requests/s needed only a third pod.
- **One node, one laptop.** Spreading, drains across nodes, and network partitions are not shown; the load generator, the cluster, and the stores share two CPUs. The numbers demonstrate behavior, not capacity.


## AWS infrastructure

Milestone 10 starts with `deploy/aws/bootstrap`, a persistent Terraform root module
in `eu-north-1`. Terraform 1.16.5 and `hashicorp/aws` 6.68.0 are pinned; the provider
lock file includes Linux amd64 and both Mac architectures. Terraform is a developer
tool, outside `go.mod`, and does not change the gateway or its permissions.
An explicit account ID limits which credentials the provider accepts.

The split gives infrastructure two independent lifetimes and state keys:

| Stack | Resources | State key | Lifetime |
|---|---|---|---|
| `bootstrap` | S3 state bucket, two private ECR repositories | `bootstrap/terraform.tfstate` | Kept between sessions |
| `platform` | VPC, EKS, RDS PostgreSQL, ElastiCache Valkey, and secret containers | `platform/terraform.tfstate` | Created for working sessions, then destroyed |

Both keys use the bootstrap bucket. The network, cluster, and data stores have
live creation, connection, and teardown verification. The gateway,
ingress, and cloud logs have not been deployed. Taggable resources inherit
`Project=llm-gateway` and `Stack=bootstrap` or `Stack=platform`.

The state bucket has versioning, explicit SSE-S3/AES256 encryption, all four public
access blocks, bucket-owner-enforced ownership (ACLs disabled), and a policy denying
non-TLS access by non-service principals. There is no access-granting bucket policy.
`prevent_destroy` guards the bucket while its resource block is present, and
`force_destroy=false` refuses automatic emptying. State versions are retained for
recovery and can contain secrets in later milestones; protect access accordingly.
Additional access logs, data-event trails, and paid metrics are outside this bootstrap.

The initial apply uses local state to create the bucket; the operator then copies
`backend.tf.example` to the gitignored `backend.tf` and migrates state into it.
The S3 backend uses `use_lockfile=true`, without DynamoDB. A fresh checkout must
initialize that backend before applying so it finds existing resources. State,
plans, local variables, and `.terraform/` are gitignored; the provider lock file is
committed. The [runbook](aws.md) describes migration, reconnecting, and key-scoped
backend permissions.

ECR holds `llm-gateway` and `llm-gateway-fakeprovider`, with immutable tags, AES256
encryption, repository-scoped scan-on-push, and one lifecycle rule each retaining
the newest 10 tagged or untagged images. Bootstrap does not change registry-wide
scanning or create repository policies or CI roles. Retention can remove an old
rollback image, so it is a demonstration policy. Basic scans cover OS packages;
Go vulnerabilities still require `make vuln`. Nonempty repositories cannot be
force-deleted. Manual arm64 pushes and completed scans with no findings reported
are [recorded in the runbook](aws.md#bootstrap-verification--2026-10-11). OCI indexes
are resolved to runnable platform image digests for scan queries, excluding
attestations. CI does not publish to ECR: it publishes multi-platform images to GHCR,
because the project cannot create the identity provider a keyless ECR push needs
(see [Container image](#container-image)).

`make terraform-check` runs formatting, backend-free initialization with a read-only
lock file, validation, and mocked tests in a separate CI job. The mocked provider
checks bucket protections, image settings/retention, and account input without
AWS credentials or API calls. These checks do not establish a live apply, remote
state migration, scan result, or image push; those remain manual verification,
separate from the operator-provided bootstrap results in the runbook.

### Platform network and cluster

`deploy/aws/platform` uses the pinned `terraform-aws-modules/vpc` (6.7.3) and
`terraform-aws-modules/eks` (21.29.0) modules, which expand into about 50
resources; anything added later is written as plain resources. It also pins the
`cloudinit`, `null`, `time`, and `tls` providers the EKS module requires.

```text
VPC 10.0.0.0/16, eu-north-1a and eu-north-1b
├─ public  10.0.128.0/24, 10.0.129.0/24   internet gateway; one NAT gateway (in 1a)
├─ private 10.0.0.0/19, 10.0.32.0/19      nodes and data stores; default route via the NAT gateway
└─ S3 gateway endpoint on the private route table
EKS 1.37 (STANDARD upgrade policy): API endpoint public for admin_cidr only, private for the nodes
└─ managed node group: 2 × c7i-flex.large, AL2023 x86_64, on demand, 2–3 nodes
   add-ons: vpc-cni, eks-pod-identity-agent (before nodes), kube-proxy, coredns, metrics-server
```

Network boundaries:

- **Nothing on the internet can open a connection to a node.** Private subnets
  have no route in, and nodes get no public address. Outbound traffic (image
  pulls, AWS APIs) leaves through the NAT gateway; S3 traffic, which includes ECR
  layers for the add-on images, uses the free gateway endpoint instead.
- **The Kubernetes API** is reachable from the internet only from `admin_cidr`,
  which must be /24 or narrower. Nodes and pods reach it through the private endpoint.
- **Security groups** are the EKS module's recommended rules. The cluster group
  accepts HTTPS from nodes. The node group accepts, from the control plane, the
  API (443), kubelet (10250), and common webhook ports (4443, 6443, 8443, 9443,
  10251), and from other nodes, DNS (53) and ephemeral ports (1025–65535). It
  allows all egress. Pods use VPC addresses from the private subnets (the VPC CNI),
  so they share their node's security group.

Decisions:

- **Two AZs, one NAT gateway.** EKS needs subnets in two AZs, as RDS and an ALB
  will. One NAT gateway instead of one per AZ halves its cost; nodes in `1b` lose
  outbound access if `1a` fails, which a session-only demonstration accepts.
- **Private /19 subnets.** The VPC CNI gives every pod a VPC address, so subnets
  are sized for pods, not only nodes; a `c7i-flex.large` holds up to 29 pods.
- **Nodes restricted by the account.** The SCPs deny Spot, and the Free plan
  launches only free-tier-eligible types. `c7i-flex.large` is the smallest of
  those with 4 GiB, enough for the add-ons later PRs install (2 GiB types fit
  11 pods and about 1.4 GiB each). It is x86; the images are built for amd64 and
  arm64, so the planned Graviton nodes would need only `ami_type` and the type.
- **Access entries, not `aws-auth`.** `authentication_mode = "API"` maps IAM
  identities to Kubernetes permissions through the EKS API; the applying
  identity gets cluster admin. There is no ConfigMap to break or lock out.
- **No IRSA; Pod Identity instead.** IRSA needs an IAM OIDC identity provider,
  which the project's SCPs deny. The Pod Identity agent runs as an add-on so
  later add-ons can receive scoped roles. The gateway pod gets no AWS permissions.
- **No billed extras.** No customer-managed KMS key (EKS encrypts Secrets with an
  AWS-owned key, and a session key would linger pending deletion), no
  control-plane logs, and the `STANDARD` upgrade policy, so the cluster never
  moves into extended support at six times the hourly price.
- **Add-on versions** are the newest EKS offers for 1.37 when applied; Kubernetes
  itself is pinned. `vpc-cni` and the Pod Identity agent install before the node
  group, so nodes join with networking ready.

`make aws-up`, `make aws-down`, and `make aws-leftovers` create, destroy, and
check a session; the [runbook](aws.md#platform-sessions) describes them, the
cost, and the [verification](aws.md#platform-verification--2026-10-12).

### Private data stores and secrets

RDS PostgreSQL 18.6 runs on one `db.t4g.micro` with 20 GiB of encrypted gp3
storage. Its subnet group uses the two private subnets, and its security group
allows TCP 5432 only from the node security group. `rds.force_ssl=1` rejects
plaintext connections; clients use `sslmode=verify-full` and the RDS CA bundle.
RDS generates and manages the master password in Secrets Manager: Terraform
stores its secret ARN, not the password. RDS deletes that secret with the instance
([AWS password management](https://docs.aws.amazon.com/AmazonRDS/latest/UserGuide/rds-secrets-manager.html)).

ElastiCache Valkey 9.1 runs on one `cache.t4g.micro` as a replication group
without replicas or cluster mode, compatible with the gateway's single-endpoint
client. Its subnet group uses the same private subnets, and its security group
allows TCP 6379 only from the node security group. Encryption at rest and TLS in
`required` mode are explicit. The 64-character alphanumeric AUTH token is generated
by pinned `hashicorp/random` 3.9.1 and stored in Secrets Manager; both the cache's
token and the secret version use the same value. `SET` replaces an old token on
updates. The token is sensitive but **is present in Terraform state**, including
retained S3 state versions. Only provider keys, clients, and the RDS password stay
out of that state. State read access must be treated as credential access.

Both stores are session resources, with no replicas, backups, final snapshots,
or deletion protection; destruction loses all usage rows and rate-limit state.
They provide no high-availability or disaster-recovery guarantee. Minor engine
upgrades are disabled for repeatable sessions, and extra monitoring/log exports
are off. The node security group also admits any other pod or process on those
nodes; these rules do not distinguish gateway pods from other node workloads.

Terraform creates empty Secrets Manager containers at `llm-gateway/openai-api-key`,
`llm-gateway/anthropic-api-key`, and `llm-gateway/clients`. The operator writes
values through the AWS CLI, outside Terraform, and must repopulate them each
session. Terraform-managed secrets have no recovery window; deletion is
irreversible and allows their names to be reused, once deletion finishes.
Secret consumption by the gateway is subsequent work; it still has no AWS role.

`make aws-check-stores` creates a temporary namespace, a credential Secret, and
a restricted pod that checks RDS with `psql` and Valkey with OpenSSL, then
deletes the namespace. Credentials are transferred through private temporary
files and stdin, never arguments or logs. It verifies the RDS certificate and
hostname. Every Valkey TLS command, including AUTH, travels on an OpenSSL
connection that checks the CA chain and endpoint hostname before sending
application bytes. A `postgres:18` container supplies OpenSSL; a separate
`redis:8` container tests plaintext rejection without receiving the token.
It also checks rejection of plaintext PostgreSQL,
unauthenticated Valkey, and plaintext Valkey, and runs a trivial Lua script.
Connections and pod lifetime are bounded. These checks do not exercise the
gateway's GCRA/lease scripts or two-replica limits. CI tests Redis 8; Valkey 9.1
gateway compatibility still requires live deployment checks. See the
[runbook](aws.md#check-the-private-data-stores) for the live sequence and limitations.

On 2026-10-12, the operator supplied a full store-session verification: 67
resources created in 11 minutes 14 seconds, both nodes Ready, PostgreSQL 18.6
over verified TLS 1.3, and Valkey 9.1.0 passing the certificate, AUTH, and Lua
checks. Plaintext PostgreSQL and unauthenticated Valkey were rejected; plaintext
Valkey gave no response within the check's 10-second bound. The first plan
found an apply-method mismatch on `rds.force_ssl`; making `pending-reboot`
explicit removed it, and the next plan reported no changes with exit code 0.
All 67 resources were then destroyed, and every leftover category was empty.
The [runbook record](aws.md#store-verification--2026-10-12) separates these
results from gateway integration and persistence, which remain unverified.
That live run preceded the change from separate hostname checks to verifying
the actual AUTH connection. A second AWS session passed verified AUTH and PING,
then exposed a Lua reply-parsing bug: a returned string is a RESP bulk string,
not a status reply. The corrected helper and TLS fixture now validate `$2` and
`ok`. The operator's live retry passed all checks on PostgreSQL 18.6 and Valkey
9.1.0, followed by a no-change plan with exit code 0, destruction of all 67
resources, and an empty leftover report. This verifies the final helper on AWS.

`TestAWSStoreCheck` runs the script with fake external commands and real `jq`,
checking credential transfer, failed retrieval/validation, failed pods, namespace
ownership, cleanup failures, and Valkey rejection checks that must not accept
plaintext replies or tool errors. A local TLS server with real OpenSSL verifies
successful command exchange and proves that a mismatched hostname or untrusted
CA prevents any AUTH bytes reaching the server. It needs Bash, jq, and OpenSSL 3,
but no AWS credentials, cluster, or Docker. Client-image tool availability was
checked locally with networking disabled; managed-store connections were
verified separately by the operator's live runs above.

## Testing

Automated tests use synthetic credentials, fake upstream HTTP servers, and fake recorders/stores, with no paid provider calls. PostgreSQL integration tests also need a disposable PostgreSQL, named by `GATEWAY_TEST_DATABASE_URL`, and Redis integration tests a disposable Redis, named by `GATEWAY_TEST_REDIS_URL`; without them they are skipped, except in CI, where they fail.

| Level | What it proves |
|---|---|
| `internal/auth` | Key lookup, invalid, empty, and disabled keys, client validation, and concurrent use. |
| `internal/usage` | Record validation, price parsing (decimal places, bounds, malformed values), costs with and without cache reads and writes, exact picodollar results, inconsistent and overflowing usage, and concurrent use. Recorder tests use a fake store for batching, interval flush, queue overflow with periodic and shutdown drop summaries, failed and timed-out writes, a single retry of a failed batch and none after a timeout, shutdown drain and cancellation, record snapshots, and concurrent Record/Close. |
| `internal/metrics` | Counter and histogram values per label set, model labels limited to configured names under arbitrary requested names, the text format, and runtime metrics; token types with cache tokens kept out of `input`, cost in dollars and none when unknown, attempt outcomes and error statuses through `Instrument`, retry, fallback, and circuit state values, series starting at zero, rate-limit store series absent until Redis is configured and then the mode, call durations by operation, and errors, a nil `Metrics` doing nothing, and every metric queried by the provisioned Grafana dashboard being exported. `breaker`, `retry`, and `routing` tests check that `OnStateChange` reports every transition in order, `OnRetry` runs once per repeated attempt, and `OnFallback` only when falling back. `ratelimit` tests check that `OnCall` reports every Redis call with its operation and duration, a rejection as an answer, and a failure as an error, but not an admission check whose request was canceled. |
| `internal/tracing` | Against an in-memory span recorder: server span names, kinds, status attributes, and error status only for 5xx; client-chosen paths not used as names; a valid incoming `traceparent` becoming the remote parent and a malformed one starting a new trace; `http.ResponseController` still reaching the connection; the exact attribute sets of route, provider, and attempt spans, so no content is added; `error.type` and error status per failure kind; nesting; and a nil `Tracer` wrapping nothing. `httpapi` tests check `trace_id` in logs. |
| `internal/postgres` | Migration file naming; against a real PostgreSQL, each test in its own new database: migrating an empty database, idempotent and concurrent migration, a failing migration leaving no trace, applying only pending migrations, refusing a newer schema; inserting success and failure records with exact costs and NULLs for unknown values, skipping already stored request IDs, rejecting invalid records without writing, all-or-nothing batches, cancellation, and concurrent inserts. Errors never reveal the database password. Without a database, against a loopback server that answers PostgreSQL's TLS request with a certificate from a test CA: `tls_certificate` for `verify-full` with the CA missing from the trust store, an unrelated CA, or a certificate for another host, and for `verify-ca` with an unrelated CA or an expired certificate, but not when the handshake succeeds and the connection fails later; a missing `sslrootcert` file reported as unreadable, matching `fs.ErrNotExist`; and neither error containing the password, address, or path. Spans, recorded in memory: one root client span per non-empty batch with its record and inserted-row counts, links only to sampled request spans, the SQLSTATE as `error.type` on failure, no record values or URL; a `migrate` span with the applied count and a child per migration, and error status on the run and the failing migration. |
| `internal/ratelimit` | Burst, refill, and sustained rates with exact `RetryAfter`, concurrency limits and release, independent clients, and concurrent use, against a manual clock. `Shared`, against a real Redis with a key prefix per test that is deleted afterwards: burst then rejection with a `RetryAfter` within one interval, rejections consuming nothing, refill after waiting `RetryAfter`, independent clients, two limiters with their own connections sharing one bucket and admitting exactly the burst under concurrent calls, a separate prefix being separate state, the keys' expiry and the empty lease set removed after release, and the stored time being an exact integer. Concurrency against Redis: the cap across two instances with `RetryAfter` 0, a slot released on one instance free on the other, independent clients, repeated release removing only its own lease, release after the request was canceled, release bounded by the timeout once Redis stops answering, an unreleased lease expiring, a concurrency rejection consuming no token and a rate rejection holding no slot, and no more than `MaxConcurrent` requests in flight under concurrent calls from two instances. Without Redis: constructor validation, key names and microsecond intervals, unknown clients without a Redis call, a server that never answers bounded by the timeout with an error that is not a rejection, a canceled request, and replies the script cannot produce (unknown status, non-positive or too long wait, wrong length) treated as store failures. `Fallback`, over a store the test can switch between real, refused, and unresponsive Redis, with a manual clock: shared limits and rejections without a mode change; a refused or unresponsive Redis moving to local limits, which then apply, with one warning and one mode report however many requests follow; no Redis call before the retry interval; recovery by the first probe after it, with one info line; a failed probe staying local without another warning; one probe at a time while requests meanwhile are decided locally; request cancellation and unknown clients not changing the mode, and a canceled probe letting the next request probe; releases going to the limiter that admitted, across mode changes; an unknown client's request as a probe not ending local mode. |
| `internal/routing`, `internal/httpapi` | Routing, validation, response translation, and error mapping, using fake providers; in `httpapi` also the `Authorization` header rules, 401 and 429 responses, identity in the provider's context, the limiter receiving the request context with the client and request IDs, a non-limit limiter error answered as 500, a client gone during admission recorded as `499 client_closed` without a response, concurrency slots released on every outcome, `client_id` in logs, a gateway-assigned request ID on every response and in logs, the log handler adding both IDs to records logged with a request's context, and exactly one outcome line per validated request with its status, provider, retry count, and fallback, and none with prompt or completion content. `retry` and `routing` tests check the retry count and fallback recorded in `llm.Stats`. |
| `internal/provider/*` | Wire format, headers, status handling, malformed and oversized responses, transport failure, cancellation, and slow upstreams, against fake provider servers. |
| `cmd/gateway` | Configuration rules, including the log format, clients, required database/pricing settings, strict price parsing, and startup warnings for unpriced models and unused prices. Startup and lifecycle tests inject a fake store. Shutdown connections: before shutdown a keep-alive connection is reused; afterwards the response of a request already in flight and the next one carry `Connection: close`, the next request opens a new connection, and `http.ResponseController` still reaches the connection through the wrapper. Health probes: `/livez` and `/readyz` answered without a key, `HEAD` allowed and other methods `405`, `/readyz` `503` once the shutdown context is done while `/livez` stays `200`, other paths (including chat completions) passed through unchanged, and a stalled probe body bounded by the read deadline; on a running gateway with tracing enabled, probes return no request ID and add no log line, `gateway_` metric sample, exported span, or usage record. Shutdown: the delay and timeout settings and their errors, which never echo a value; readiness failing at the signal while the servers stop only when the delay ends, and at once, without a delay, when a server fails; on a running gateway, during the delay a new connection still gets a completion while `/readyz` answers `503` and an in-flight request finishes with `Connection: close`, the listener closes afterwards, and both are recorded as successes; a request outlasting `GATEWAY_SHUTDOWN_TIMEOUT` is canceled upstream and recorded as `499 client_closed`. Complete-path tests use the real handler, router, and both adapters with fake upstreams: routing, errors, retries, fallback, circuit breaking, authentication, rate limits, cancellation, deadlines, graceful/forced shutdown, and JSON logs in which every retry and outcome line carries the request and client IDs. One outcome line is logged per request, with the right `retry_count` and `fallback`, for success, upstream failure, retry exhaustion, fallback success, and client cancellation. Metrics tests count successes, upstream failures, unknown models, and authentication and validation rejections with bounded labels, and check that `/metrics` is served without a key only on the metrics listener, never on the API port. Provider metrics match retries that recover, retry exhaustion followed by a fallback, and a circuit that opens and then rejects without an attempt. Token and cost metrics equal the usage record of a direct and a fallback request with cache reads and writes; an unpriced model adds tokens but no cost; failures add neither. Accounting tests verify one record per validated outcome, cache-aware and fallback prices, NULL costs for unpriced models, and no writes for pre-validation rejections. Tracing tests use the real handler, router, and adapters with an in-memory span processor: the span tree for success, retries (one span per attempt, one `retry` event per retry), fallback (a `fallback` event and a span per provider), an unknown model, and a rejection before routing; server, attempt, and retry-event attributes; a client's `traceparent` continuing into the gateway while no upstream request carries trace headers; `trace_id` on every log line; and no prompt, completion, gateway key, provider key, or baggage in any span. A startup test exports to a fake OTLP/HTTP collector and checks that shutdown flushes the spans. Against PostgreSQL, batch insert spans start their own traces and link to the spans of the requests they store, and the `migrate` command exports its migration spans, without the database URL. Configuration tests reject invalid OTLP endpoints and protocols without printing them. A disposable PostgreSQL test verifies unmigrated startup failure, the independent migration command, and actual handler → recorder → database persistence. Redis: `GATEWAY_REDIS_URL` parsing for `redis://` and `rediss://`, errors that never include the URL or its password, client options overriding the URL, in-process limits without Redis, startup with an unreachable Redis in local mode with one warning and go-redis lines routed to debug logs, a complete startup with `rate_limits` in the startup log and no password in any log, and, against real Redis, a complete request path whose second request gets `429` from the shared bucket and whose released slot is gone from Redis, with the store metrics counting two admission checks, one release, and no errors; an unreachable Redis shows the store as down and counts the first request's failed check. Multi-instance tests run two complete gateways, each with its own Redis client, limiter, and metrics, against one Redis: a client's request rate and its concurrency slot are shared across them while another client's are separate; through a TCP proxy that is cut and restored, both instances keep serving with their own limits during the outage (one warning each, store gauge `0`), and after the retry interval return to the shared bucket, shown by a request one instance's own limits would have admitted; and a slot held by an instance that closed without releasing it frees itself when its lease expires. |

Tests coordinate with channels. Timeouts are used only as failure guards. CI runs the Makefile's `fmt`, `vet`, `test`, `bench-check`, `build`, and `vuln` targets (`make check` locally), with PostgreSQL and Redis service containers for the integration tests. A second CI job runs `make image-check image-scan` with a buildx `docker-container` builder, building both container images for both platforms without pushing them and scanning each; it checks that the images build and have no fixable HIGH or CRITICAL findings, not that they run. A third runs `make helm-lint` (see [Helm chart](#helm-chart)), and a fourth installs the chart in kind and sends requests through it (see [Local Kubernetes environment](#local-kubernetes-environment)). On pushes to `main`, a last job publishes the images once all others pass (see [Container image](#container-image)). Locally, `make db` and `make redis` start the same images in Docker.

Provider base URL tests cover both providers' defaults, HTTPS overrides and path prefixes, loopback HTTP, invalid destinations and ports, credentials, queries, fragments, and errors that omit URL values. A complete `runWith` startup test reaches both local fake providers using only environment configuration, checks paths, models, credentials, and translated completions, and shuts down normally. Invalid URL configuration is rejected before opening the database. Complete-path redirect tests also use environment-configured destinations and prove that no request reaches the redirect target.

Profiling configuration tests cover default/blank/false, true, and invalid values without echoing them. Complete startup tests check the profile index, runtime profiles, CPU, execution trace, command line, and symbols on both listeners, proving default-off behavior, API isolation, unauthenticated access when enabled, GET routing, unchanged chat authentication, and preserved metrics. A real HTTP CPU-capture test uses a shortened ordinary write timeout to prove that the timed handler extends its deadline and returns a complete gzip profile. Another starts a long CPU capture and verifies that bounded shutdown closes its connection and ends the profiling handler. `TestObservabilityExcludesSensitiveData` still checks logs, metrics, and exported OTel spans: its direct `metrics.Metrics.Handler` has no profiling routes, and no new telemetry fields were added. Opt-in profiling is a separate privileged diagnostic surface, documented above rather than claimed to be sanitized by that marker test.

`cmd/fakeprovider` supports local HTTP measurements without provider access. Its fixed wire fixtures are exercised through both real adapters, including JSON-escaped model names and cancellation over actual HTTP connections. Virtual-time tests check the configured delay and cancellation without arbitrary sleeps; HTTP tests check bounded body parsing, method/path errors, connection reuse versus new connections, and startup/shutdown output without content or credentials. `make build` builds both commands, and the existing `./...` vet, race-test, and vulnerability checks include the fake. See [performance.md](performance.md#local-fake-provider) for commands, synthetic usage, and measurement limitations; it is not a full provider emulator and does not change the production request path.

Request-path benchmarks live alongside their packages' tests: authentication, routing with direct and fallback routes, local rate-limit admission/release (serial and parallel, one or many clients), the fallback wrapper in local mode, Redis admission/release, cost estimation with cache tokens, and the in-process HTTP handler with metrics/tracing off and on. They report allocations and check the measured outcomes. `make bench` measures them locally; `make bench-check` runs each once in CI without performance thresholds. Redis uses the integration-test setting and deletes only its own random key prefix. The handler uses real auth, local limits, routing, and pricing, a fake provider, a discard recorder (the constructor requires one), and a bounded in-memory trace exporter in a test-only SDK fixture. Production SDK ownership remains in `cmd/gateway`. [performance.md](performance.md) records the environment, results, fixture limitations, and pinned `benchstat` comparison commands; the developer tool is run through `go run` and adds no gateway dependency.

`deploy/loadtest` holds synthetic clients/prices, request bodies, Vegeta target templates, a Bash runner, and awk summaries. `make loadtest` builds the real binaries, starts one gateway against the fake, and compares direct/gateway HTTP/1.1 traffic with 500 closed workers at zero and 500 ms provider delay, plus fixed arrivals at 70% of the first run's successful throughput (or an explicit standalone C rate for sweeps). Metrics, JSON logs, real PostgreSQL recording, and opt-in profiling remain enabled; tracing and fallback are off in a clean gateway environment. Redis is off unless explicitly enabled with `LOADTEST_REDIS_URL`, restricted by the development runner to a credential-free loopback Redis URL without options. Each phase creates a unique synthetic client ID to isolate its expiring Redis state and usage rows. Separate fake processes count direct and gateway-upstream accepted connections, including their own warmup/readiness probes. Profiles, raw results, logs, and matching binaries stay in a private gitignored directory. Missing diagnostics fail the runner, HTTP errors remain visible, queue drops are collected through shutdown, and a shared comparison with failed store calls or sampled local mode fails explicitly. The operator supplies disposable stores; the script never migrates or deletes database data or scans/flushes/deletes Redis keys. Configuration-fixture, script-validation, and report-summary regression tests run in `make test`; load itself never runs in CI.

Normal connection-regression tests use two synchronized 500-request waves through the real upstream client, over HTTP/1.1 and certificate-trusted TLS/HTTP/2, and require the second wave to open no new connection; they also verify inbound keep-alives through production server settings. `make performance-probes` adds opt-in, `performance`-tagged Redis pool-stat burst measurements and, with `LOADTEST_VEGETA_BIN` pointing to a completed run's pinned tool, a generator/inbound-server reuse probe. Redis uses test-owned random keys with cleanup; the generator probe isolates server connection settings, not the full handler. `make vet` compiles optional performance probes alongside smoke tests, but CI never runs the tagged bursts or generator. `make binary-size` retains unstripped/stripped builds and raw symbols, attributing text symbols by package/module while excluding BSS/data/metadata and same-address aliases; its awk mapper has a normal regression test. These tools add no production dependency, metric, or configuration.

The load tool is [Vegeta v12.13.0](https://github.com/tsenart/vegeta/tree/v12.13.0) (MIT), installed only into the run directory with `go install ...@version`. Its fixed-arrival and unlimited-rate/fixed-worker modes avoid another runtime or a new gateway dependency. Its transitive CLI dependencies (including t-digest reporting, DNS caching, Prometheus, and HTTP/2 support) are confined to the developer tool; `go.mod`, the gateway's dependency tree, and its binary size are unchanged. Shell/awk orchestration uses existing Go process metrics rather than introducing an observability service. [performance.md](performance.md#running-local-load-scenarios) documents tool prerequisites, measurement windows, socket cooldowns, independent-percentile comparisons, accounting loss, and same-machine limitations. Redis, connection, recorder, and size findings are documented separately from production tuning. The one production change they justified, the [upstream idle pool](#upstream-timeouts), was made in its own PR with before/after runs of the same scenarios.

The v0.6 closeout adds `TestObservabilityExcludesSensitiveData`, which checks text/JSON logs, scraped metrics, and in-memory exported spans together using marker prompts, completions, gateway/provider keys, key hashes, upstream response values, and baggage. Positive assertions ensure requests reached the real adapters and all three telemetry outputs were produced. Success, retries, fallback, authentication/validation failures, and malformed upstream responses are covered. Since v0.7 every case also runs with shared rate limits configured against a Redis that is down and requires a password marker, so the startup warning, the fallback to local limits, go-redis errors, and the store metrics are checked for it too. Unexpected response roles, content types, and finish reasons are reported with fixed diagnostics instead of echoing upstream strings. Additional regression tests prove that database server errors and collector responses stay out of diagnostics, and that provider redirects cannot send requests to another destination. Existing PostgreSQL tests verify linked batch writes; existing fake OTLP collector tests verify serialized export and shutdown flushing.

For v0.7, shared rate limits are verified against a real Redis 8 in CI and locally (`make redis`), including the two-instance and outage tests above. v0.8 measures admission/release latency through the existing store histogram and pool waits in a separate burst probe; these local results do not establish managed-service latency or capacity. Not verified: `rediss://` against a TLS Redis, a managed Redis service, or Redis Cluster/Sentinel (not supported).

Release verification does not establish a throughput or latency-overhead guarantee; the v0.8 measurements in [performance.md](performance.md) are separate, opt-in local runs, not release gates. The documented v0.6 PR 8 dashboard exercise used fake traffic on macOS/Colima. Linux stack behavior and container-image vulnerability scanning are outside `make check`. The optional live `make smoke` passed on 2026-10-10 during the v0.6 closeout: `gpt-4o` reported `gpt-4o-2024-08-06` with 15 input and 2 output tokens for its normal completion, and `claude-opus-5-5` reported the same model name with 19 input and 4 output tokens. Both normal completions ended with `stop`; both forced `length` tests and the gateway's unknown-key, missing-key, and rate-limit tests passed. It passed again on 2026-10-11 for the v0.8 release, after the base URL settings and the larger upstream idle pool, against the default production origins: `gpt-4o` reported `gpt-4o-2024-08-06` with 15 input and 2 output tokens, `claude-opus-5-5` reported the same model name with 19 input and 5 output tokens, both normal completions ended with `stop`, and the same `length`, authentication, and rate-limit tests passed. The smoke harness disables metrics and tracing and uses a fake recorder, so live telemetry export and database persistence are verified separately.

A separate smoke test (`cmd/gateway/smoke_test.go`, build tag `smoke`, run with `make smoke`) uses real provider keys from the environment and the real handler and adapters against the live APIs. It checks a completion and a `length` stop with usage per configured model, plus the 401 and 429 paths. It is manual only: it costs money and needs credentials, so CI never runs it, but `make vet` compiles it so it cannot rot.
