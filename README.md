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

### v0.1 — Provider Abstraction and Routing

The first milestone focuses only on the core gateway architecture:

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

Requires Go 1.26 or later. Configure at least one provider and start the gateway:

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

Startup fails if no provider is configured, if only one variable of a pair is set, or if both providers use the same model name. Error messages name the variables but never print their values.

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
* No upstream timeouts or retries yet; a slow provider is waited on until the client disconnects (planned for v0.2).
* No gateway authentication; run it only on a trusted network (planned for v0.4).
* On reasoning models, thinking counts toward `max_tokens`, so a small limit can end with `length` and little text.

## Development

Run the verification suite locally with:

```bash
gofmt -l .
go vet ./...
go test -race ./...
go build ./cmd/gateway
```

The same checks run automatically through GitHub Actions for pull requests and changes to `main`. Tests use local fake provider servers and need no API keys or network access.

## Project Status

🚧 **Early development** — v0.1 is feature-complete.

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
v0.5  Usage + cost tracking
  ↓
v0.6  Prometheus + OpenTelemetry
  ↓
v0.7  PostgreSQL + Redis
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
