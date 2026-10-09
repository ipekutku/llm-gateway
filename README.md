# llm-gateway

[![CI](https://github.com/ipekutku/llm-gateway/actions/workflows/ci.yml/badge.svg)](https://github.com/ipekutku/llm-gateway/actions/workflows/ci.yml)

A production-oriented LLM gateway and AI platform built primarily in Go.

The project explores how applications can interact with multiple LLM providers through a single, reliable API layer.

```text
Application
    │
    ▼
LLM Gateway
    │
    ├── Provider A
    └── Provider B
```

## Goals

The long-term goal is to build infrastructure similar to an internal AI platform used by production applications, with capabilities such as:

* provider abstraction and model routing
* provider failover and resilience
* authentication and rate limiting
* token usage and cost tracking
* metrics and distributed tracing
* persistent platform data
* containerized and Kubernetes deployment
* infrastructure-as-code and cloud deployment

The project is intentionally developed **incrementally**. Each milestone should leave the repository in a working and testable state rather than introducing the entire platform at once.

## Current Milestone

### v0.4 — Authentication and Rate Limiting

The fourth milestone turns the gateway from an anonymous proxy into a multi-client service. It is feature-complete:

* every request needs a gateway-issued API key, sent as `Authorization: Bearer <key>`; anonymous requests get `401`
* gateway keys are separate from the provider keys, which stay server-side and never reach clients
* only SHA-256 hashes of keys are stored, in a clients file; keys can be disabled
* a client identity attached to every request and to its failure logs
* per-client rate limits: requests per minute with bursts, and concurrent requests, answered with `429` and `Retry-After`

### v0.3 — Provider Failover and Circuit Breaking ✅

The third milestone let requests survive the degradation of one provider:

* fallback pairs: a configured model can fall back to the other provider's configured model, tried once and never back again
* fallback on provider failures (timeouts, unavailability, rate limiting, server errors), not on rejected requests or client cancellations
* a per-provider time limit, so a primary that hangs leaves the fallback time to answer
* a circuit breaker per provider: after repeated failures the provider is skipped for a cooldown, then probed with a single request
* no retry/fallback loops: retries stay inside each provider, and fallback runs at most once per request

Requests may be answered by a different model than requested; the response's `model` field always names the model that answered.

### v0.2 — Timeouts and Retry Policy ✅

The second milestone made calls to unreliable upstream providers safer:

* explicit, configurable upstream timeouts: one time budget per request, plus connection-setup limits
* bounded retries for transient failures: `429`, `502`, `503`, `504`, Anthropic's `529`, and failures to connect (`502` and `504` can come from a provider's proxy after the request reached the model, a small duplicate-generation risk)
* exponential backoff with jitter, honoring the provider's `Retry-After` header
* no retries of timeouts or failures after the request was sent, because a chat completion is not idempotent and a retry could produce a second, separately billed generation
* cancellation stops retries immediately; all attempts share the request's time budget

### v0.1 — Provider Abstraction and Routing ✅

The first milestone focused only on the core gateway architecture:

* Go HTTP service
* OpenAI-compatible `/v1/chat/completions` endpoint
* vendor-neutral request and response models
* interchangeable LLM provider interface
* two provider implementations
* static model-to-provider routing
* automated tests
* continuous integration

Features such as retries, failover, authentication, databases, observability, Kubernetes, and cloud deployment are intentionally deferred to later milestones.

## Engineering Principles

This project prioritizes:

* idiomatic and maintainable Go
* simple architecture over speculative abstractions
* clear provider boundaries
* strong automated testing
* context propagation and cancellation
* minimal dependencies
* small, reviewable changes
* production-oriented error handling
* measurable reliability and performance as the project evolves

## Running Locally

Requires Go 1.26.9 or later. First create a gateway API key for your client and a clients file holding its hash:

```bash
GATEWAY_KEY=$(openssl rand -base64 32)   # give this key to the client
echo "$GATEWAY_KEY"
printf '{"clients": [{"id": "my-app", "key_sha256": "%s"}]}\n' \
  "$(printf %s "$GATEWAY_KEY" | shasum -a 256 | cut -d' ' -f1)" > clients.json
```

Then configure at least one provider and start the gateway:

```bash
export GATEWAY_CLIENTS_FILE=clients.json

export OPENAI_MODEL=gpt-4o
export OPENAI_API_KEY=sk-...            # your OpenAI key

export ANTHROPIC_MODEL=claude-opus-5-5
export ANTHROPIC_API_KEY=sk-ant-...     # your Anthropic key

go run ./cmd/gateway
```

The gateway listens on `127.0.0.1:8080` and logs the configured models and the number of clients. Stop it with `Ctrl+C` or `SIGTERM`; in-flight requests get up to 5 seconds to finish.

Send a request to either model:

```bash
curl -s http://127.0.0.1:8080/v1/chat/completions \
  -H "Authorization: Bearer $GATEWAY_KEY" \
  -H 'Content-Type: application/json' \
  -d '{
    "model": "gpt-4o",
    "messages": [
      {"role": "system", "content": "Answer concisely."},
      {"role": "user", "content": "Explain TCP."}
    ],
    "max_tokens": 200
  }'

curl -s http://127.0.0.1:8080/v1/chat/completions \
  -H "Authorization: Bearer $GATEWAY_KEY" \
  -H 'Content-Type: application/json' \
  -d '{
    "model": "claude-opus-5-5",
    "messages": [{"role": "user", "content": "Explain TCP."}],
    "max_tokens": 200
  }'
```

The `model` field must match a configured model exactly; the request is routed to that provider and the model name is forwarded unchanged.

### Configuration

| Variable | Description |
|---|---|
| `GATEWAY_CLIENTS_FILE` | Required. Path of the JSON file listing the gateway's clients (see below). |
| `OPENAI_MODEL`, `OPENAI_API_KEY` | Enable OpenAI for one model. Set both or neither. |
| `ANTHROPIC_MODEL`, `ANTHROPIC_API_KEY` | Enable Anthropic for one model. Set both or neither. |
| `GATEWAY_ADDR` | Listen address. Default `127.0.0.1:8080`. |
| `GATEWAY_UPSTREAM_TIMEOUT` | Time limit for all upstream work on one request, as a Go duration such as `90s` or `2m`. Default `120s`. If it expires, the client gets `504 upstream_timeout`. |
| `GATEWAY_UPSTREAM_CONNECT_TIMEOUT` | Time limit for connecting to a provider (TCP dial and TLS handshake). Default `10s`. |
| `GATEWAY_RETRY_MAX_ATTEMPTS` | Total attempts per request, including the first, from `1` to `10`. Default `3`. Set `1` to disable retries. |
| `GATEWAY_RETRY_BASE_DELAY` | Longest wait before the first retry; it doubles for each further retry. The actual wait is random up to this value. Default `500ms`. |
| `GATEWAY_RETRY_MAX_DELAY` | Cap on that doubling wait. Default `8s`. If a provider's `Retry-After` asks for longer, the gateway stops retrying and uses the fallback, or returns the error, instead of waiting. |
| `OPENAI_FALLBACK` | Set to `anthropic` to send failed `OPENAI_MODEL` requests to `ANTHROPIC_MODEL`. |
| `ANTHROPIC_FALLBACK` | Set to `openai` to send failed `ANTHROPIC_MODEL` requests to `OPENAI_MODEL`. |
| `GATEWAY_PROVIDER_TIMEOUT` | Time a provider with a fallback gets before the fallback takes over. Default half of `GATEWAY_UPSTREAM_TIMEOUT` (`60s`). |
| `GATEWAY_BREAKER_FAILURES` | Failed requests in a row after which a provider is skipped, from `1` to `100`. Default `5`. |
| `GATEWAY_BREAKER_COOLDOWN` | How long a provider is skipped before one test request is let through. Default `30s`. |

For example, to fall back from OpenAI to Anthropic:

```bash
export OPENAI_FALLBACK=anthropic
```

A request for `gpt-4o` that OpenAI cannot serve is then answered by `claude-opus-5-5`; the response's `model` field shows which model answered. While a provider is skipped and no fallback can answer, requests fail with `503 provider_unavailable`.

### Clients

The clients file lists every client allowed to use the gateway, with the SHA-256 hash of its key and optional limits:

```json
{
  "clients": [
    {"id": "my-app", "key_sha256": "<hash>"},
    {"id": "batch-jobs", "key_sha256": "<hash>", "requests_per_minute": 120, "burst": 20, "max_concurrent": 10},
    {"id": "old-app", "key_sha256": "<hash>", "disabled": true}
  ]
}
```

| Field | Description |
|---|---|
| `id` | Client name used in logs: letters, digits, `-`, `_`, or `.`, up to 64 characters. |
| `key_sha256` | Hex SHA-256 hash of the client's key. The gateway never stores the key itself. |
| `disabled` | `true` rejects the key. Default `false`. |
| `requests_per_minute` | Sustained request rate. Default `60`. |
| `burst` | Requests allowed at once after a quiet period. Default `10`. |
| `max_concurrent` | Requests in progress at the same time. Default `5`. |

Generate keys with a secure random source, as above; the hash protects only random keys, not memorable passwords. A request over its client's rate gets `429 rate_limit_exceeded` with `Retry-After`; over its concurrency limit, `429 concurrency_limit_exceeded`. The file is read once at startup; restart the gateway to add, change, or disable clients.

Startup fails if the clients file is missing, malformed, has unknown fields, or enables no client, if no provider is configured, if only one variable of a pair is set, if both providers use the same model name, if a timeout or delay is not a positive duration, if the attempt count is outside 1–10, if the maximum retry delay is less than the base delay, if the provider timeout is not less than the upstream timeout, or if a fallback names a provider that is not configured. Error messages name the variables and clients but never print keys, hashes, or other values.

The model names above are examples. Any model the provider's API accepts can be configured. Both adapters are tested against fake servers built from the providers' documented API formats; they have not yet been verified against the live APIs.

### Supported API

`POST /v1/chat/completions` implements a small subset of the OpenAI Chat Completions format. It is not a full OpenAI-compatible API. Every request needs `Authorization: Bearer <gateway key>`; a missing, unknown, or disabled key gets `401`.

| Request field | Support |
|---|---|
| `model` | Required. Must match a configured model. |
| `messages` | Required. `role` is `system`, `user`, or `assistant`; `content` is a non-empty string. An optional `system` message must come first. |
| `max_tokens` | Optional positive integer. Default `1024`. Sent to OpenAI as `max_completion_tokens` and to Anthropic as `max_tokens`. |
| `stream` | Only `false` or absent. Streaming is not supported. |
| anything else | Accepted but ignored (for example `temperature`, `tools`, `n`). |

The response contains exactly one choice with `finish_reason` `stop`, `length`, or `content_filter`, plus token usage. Errors use the envelope `{"error": {"message", "type", "code"}}`; see [docs/architecture.md](docs/architecture.md#error-mapping) for the full status mapping.

### Limitations

* One model per provider, matched by exact name; no aliases or wildcards.
* Text only: no streaming, tool calls, images, or multiple choices.
* Retries can occasionally produce a duplicate, separately billed generation: a `502` or `504` from a provider's proxy may arrive after the model already answered. Other retried failures (`429`, `503`, `529`, connection failures) happen before the model runs. See [retry safety](docs/architecture.md#what-is-retried).
* TLS handshake failures are not retried.
* With a fallback configured, a request can be answered by a different model than the one requested. A primary that times out is canceled, but may still bill for the partial generation.
* Fallback works only between the two configured models; there are no logical model names yet.
* Circuit breaker state is kept per gateway process; several instances do not share it (planned for v0.7).
* A request whose upstream timeout expires may still be billed by the provider for the work done before it was canceled.
* The gateway serves plain HTTP and listens on loopback by default. Gateway keys would cross the network unencrypted, so expose it beyond the host only behind a proxy that terminates TLS.
* Rate limits and concurrency counts are kept per gateway process; several instances do not share them (planned for v0.7).
* Clients are read from a file at startup; changing them requires a restart. Each client has one key, so rotating a key briefly means replacing it.
* Every rejected key logs one warning, and there is no per-IP limit on unauthenticated requests; anyone who can reach the port can fill the logs. Another reason to keep the gateway behind a proxy.
* A request body must arrive within 30 seconds (otherwise `408`), and idle keep-alive connections close after 2 minutes. Both are fixed.
* Provider endpoints are fixed to the production APIs, so running the gateway needs real API keys and may incur charges. It cannot be pointed at a local fake provider; the automated tests exercise the full request path against fake upstreams instead.
* On reasoning models, thinking counts toward `max_tokens`, so a small limit can end with `length` and little text.

## Development

Run the verification suite locally with:

```bash
make check
```

It runs the same checks as GitHub Actions, one `make` target each: `fmt` (gofmt), `vet`, `test` (`go test -race`), `build`, and `vuln` (govulncheck). CI runs these targets for pull requests and changes to `main`. Tests use local fake provider servers and need no API keys or network access; only the vulnerability check downloads its tool and the vulnerability database.

### Smoke test against the real APIs

The automated tests never call a real provider. To check the adapters against the live APIs, run:

```bash
read -s OPENAI_API_KEY; export OPENAI_API_KEY        # typed silently, kept out of shell history
read -s ANTHROPIC_API_KEY; export ANTHROPIC_API_KEY
export OPENAI_MODEL=gpt-4o ANTHROPIC_MODEL=claude-opus-5-5
make smoke
```

**This makes real, billed API calls** (a few short completions per provider, typically well under one cent). Configure one or both providers. For each model it checks a normal completion and a `length` stop, both with token usage. It also checks that unknown and missing gateway keys get `401`, and that a client over its limit gets `429` with `Retry-After`. It uses fresh random gateway keys and ignores other `GATEWAY_*` settings in the environment. It prints the model each provider reported and fails on any mismatch. It never runs in CI.

## Project Status

🚧 **Early development** — v0.4 is feature-complete.

### v0.4

| Component | Status |
|---|---|
| Client API keys and identity (`internal/auth`) | ✅ Done |
| Per-client rate limiting (`internal/ratelimit`) | ✅ Done |
| Clients file configuration, wiring, end-to-end tests, and error mapping (`cmd/gateway`, `internal/httpapi`) | ✅ Done |

### v0.3

| Component | Status |
|---|---|
| Circuit breaker per provider (`internal/breaker`) | ✅ Done |
| Provider fallback and failure classification (`internal/routing`) | ✅ Done |
| Failover configuration, wiring, end-to-end tests, and error mapping (`cmd/gateway`, `internal/httpapi`) | ✅ Done |

### v0.2

| Component | Status |
|---|---|
| Upstream timeouts: request time budget and connection-setup limits (`internal/httpapi`, `cmd/gateway`) | ✅ Done |
| Retry policy: failure classification, backoff with jitter, `Retry-After` (`internal/retry`) | ✅ Done |
| Retry configuration, wiring, and end-to-end tests (`cmd/gateway`) | ✅ Done |

### v0.1

| Component | Status |
|---|---|
| Vendor-neutral types and provider interface (`internal/llm`) | ✅ Done |
| Static model routing (`internal/routing`) | ✅ Done |
| `/v1/chat/completions` handler, validation, and error mapping (`internal/httpapi`) | ✅ Done |
| OpenAI provider adapter (`internal/provider/openai`) | ✅ Done |
| Anthropic provider adapter (`internal/provider/anthropic`) | ✅ Done |
| Configuration, server wiring, and end-to-end tests (`cmd/gateway`) | ✅ Done |

Design decisions are documented in [docs/architecture.md](docs/architecture.md).

## Planned Evolution

```text
v0.1  Provider abstraction + routing
  ↓
v0.2  Timeouts + retries
  ↓
v0.3  Provider fallback + circuit breakers
  ↓
v0.4  Authentication + rate limiting
  ↓
v0.5  Usage + cost tracking + PostgreSQL
  ↓
v0.6  Prometheus + OpenTelemetry
  ↓
v0.7  Redis + multi-instance behavior
  ↓
v0.8  Load testing + performance work
  ↓
v0.9  Docker + Kubernetes + Helm
  ↓
v1.0  AWS deployment with Terraform
```

## AI-Assisted Development

The project also serves as an experiment in AI-assisted software engineering.

AI coding agents are used to accelerate implementation and code review, while architecture, engineering decisions, testing strategy, security, reliability, and final code ownership remain human-controlled.
