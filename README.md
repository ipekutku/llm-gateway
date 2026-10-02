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

🚧 **Early development** — v0.1 is in progress.

| Component | Status |
|---|---|
| Vendor-neutral types and provider interface (`internal/llm`) | ✅ Done |
| Static model routing (`internal/routing`) | ✅ Done |
| `/v1/chat/completions` handler, validation, and error mapping (`internal/httpapi`) | ✅ Done |
| OpenAI provider adapter (`internal/provider/openai`) | ✅ Done |
| Anthropic provider adapter | ⏳ Next |
| Configuration, server wiring, and end-to-end tests (`cmd/gateway`) | ⏳ Planned |

The gateway cannot be run as a server yet; `cmd/gateway` is wired up in the final v0.1 step, together with configuration and usage instructions. Design decisions are documented in [docs/architecture.md](docs/architecture.md).

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
