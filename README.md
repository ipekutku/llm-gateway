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

### v0.3 — Provider Failover and Circuit Breaking

The third milestone lets requests survive the degradation of one provider. It is feature-complete:

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

Requires Go 1.26.9 or later. Configure at least one provider and start the gateway:

```bash
export OPENAI_MODEL=gpt-4o
export OPENAI_API_KEY=sk-...            # your OpenAI key

export ANTHROPIC_MODEL=claude-opus-5-5
export ANTHROPIC_API_KEY=sk-ant-...     # your Anthropic key

go run ./cmd/gateway
```

The gateway listens on `127.0.0.1:8080` and logs the configured models. Stop it with `Ctrl+C` or `SIGTERM`; in-flight requests get up to 5 seconds to finish.

Send a request to either model:

```bash
curl -s http://127.0.0.1:8080/v1/chat/completions \
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
| `OPENAI_MODEL`, `OPENAI_API_KEY` | Enable OpenAI for one model. Set both or neither. |
| `ANTHROPIC_MODEL`, `ANTHROPIC_API_KEY` | Enable Anthropic for one model. Set both or neither. |
| `GATEWAY_ADDR` | Listen address. Default `127.0.0.1:8080`. |
| `GATEWAY_UPSTREAM_TIMEOUT` | Time limit for all upstream work on one request, as a Go duration such as `90s` or `2m`. Default `120s`. If it expires, the client gets `504 upstream_timeout`. |
| `GATEWAY_UPSTREAM_CONNECT_TIMEOUT` | Time limit for connecting to a provider (TCP dial and TLS handshake). Default `10s`. |
| `GATEWAY_RETRY_MAX_ATTEMPTS` | Total attempts per request, including the first, from `1` to `10`. Default `3`. Set `1` to disable retries. |
| `GATEWAY_RETRY_BASE_DELAY` | Longest wait before the first retry; it doubles for each further retry. The actual wait is random up to this value. Default `500ms`. |
| `GATEWAY_RETRY_MAX_DELAY` | Cap on that doubling wait. Default `8s`. A provider's `Retry-After` can still ask for longer. |
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

Startup fails if no provider is configured, if only one variable of a pair is set, if both providers use the same model name, if a timeout or delay is not a positive duration, if the attempt count is outside 1–10, if the maximum retry delay is less than the base delay, if the provider timeout is not less than the upstream timeout, or if a fallback names a provider that is not configured. Error messages name the variables but never print their values.

The model names above are examples. Any model the provider's API accepts can be configured. Both adapters are tested against fake servers built from the providers' documented API formats; they have not yet been verified against the live APIs.

### Supported API

`POST /v1/chat/completions` implements a small subset of the OpenAI Chat Completions format. It is not a full OpenAI-compatible API.

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
* No gateway authentication; run it only on a trusted network (planned for v0.4).
* Provider endpoints are fixed to the production APIs, so running the gateway needs real API keys and may incur charges. It cannot be pointed at a local fake provider; the automated tests exercise the full request path against fake upstreams instead.
* On reasoning models, thinking counts toward `max_tokens`, so a small limit can end with `length` and little text.

## Development

Run the verification suite locally with:

```bash
gofmt -l .
go vet ./...
go test -race -timeout 2m ./...
go build ./cmd/gateway
go run golang.org/x/vuln/cmd/govulncheck@v1.8.0 ./...
```

The same checks run automatically through GitHub Actions for pull requests and changes to `main`. Tests use local fake provider servers and need no API keys or network access; only the vulnerability check downloads its tool and the vulnerability database.

## Project Status

🚧 **Early development** — v0.3 is feature-complete.

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
