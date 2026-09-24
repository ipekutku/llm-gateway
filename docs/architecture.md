# Architecture

This document describes the architecture as implemented. It grows with each Milestone 1 pull request; sections for the HTTP API, provider adapters, and configuration are added as those parts land.

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
| `internal/httpapi` | *(planned)* Public wire DTOs, validation, handler, error responses. |
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

`ProviderError` implements `Unwrap`, so `errors.Is(err, context.Canceled)` and `errors.Is(err, context.DeadlineExceeded)` still work through it. It never carries credentials or raw upstream bodies. The mapping to HTTP status codes will be documented with the HTTP layer.
