# LLM Gateway — Coding Agent Instructions

## Purpose

This repository contains a production-oriented LLM gateway, built primarily in Go, that lets applications access multiple LLM providers through a single API layer.

The project grows incrementally from provider abstraction and static routing toward resilience, authentication, usage and cost accounting, observability, and deployment infrastructure.

Before making non-trivial changes, read:

- `README.md` — project status, supported API, configuration, and developer workflow
- `docs/architecture.md` — implemented architecture, contracts, boundaries, and design decisions
- `docs/ROADMAP.md` — milestone scope, non-goals, and exit criteria

Use `docs/ROADMAP.md` as the single source for milestone planning, scope, and exit criteria; do not maintain separate milestone plan documents. Use the architecture document and code for implemented behavior. Unchecked roadmap criteria are not proof that work is missing. When documentation and implementation disagree, resolve the discrepancy within the task or clearly flag it; do not silently diverge.

## Core Engineering Principles

- Keep the architecture simple and explicit.
- Prefer the smallest coherent change that satisfies the current requirement.
- Favor idiomatic, readable, maintainable Go over cleverness.
- Prefer the standard library and small, well-understood dependencies.
- Avoid premature abstraction and speculative extensibility.
- Keep components independently testable and external systems replaceable in tests.
- Preserve boundaries between the public API, neutral LLM contract, routing, provider adapters, and application wiring.
- Do not add technologies merely for portfolio or CV value.
- Keep every milestone runnable and demonstrable, and `main` buildable and testable.

Go is the primary implementation language. Use another language or runtime only when there is a concrete technical reason that materially improves the system.

## Current Milestone and Scope Control

Always determine the current milestone from `docs/ROADMAP.md`, `README.md`, and the existing repository state before implementing work. Do not assume Milestone 1 or any other milestone remains current indefinitely.

At the time of this update, v0.5 is feature-complete and v0.6 observability is next. The implemented baseline includes:

- `POST /v1/chat/completions` with a documented, limited OpenAI-compatible format
- vendor-neutral request and response types and a small provider interface
- OpenAI and Anthropic adapters
- exact-match static model routing, with an optional fallback from one configured model to the other provider's configured model
- upstream timeouts: one per-request budget set in the handler, plus connection-setup limits on the shared upstream client
- bounded retries of transient upstream failures (`internal/retry`), with backoff, jitter, and `Retry-After`
- a circuit breaker per provider (`internal/breaker`), returning `503 provider_unavailable` when open and no fallback answers
- gateway client authentication (`internal/auth`): Bearer API keys stored only as SHA-256 hashes in a clients file (`GATEWAY_CLIENTS_FILE`), with disabled keys and the client identity carried in the request context
- per-client rate limits (`internal/ratelimit`): a request rate with bursts and a concurrency cap, per process, answered with `429` before the request body is read
- gateway-owned request IDs returned in `X-Request-ID` and carried in context, failure logs, and usage records
- provider identity and separate prompt-cache read/write counts in neutral usage, with cache tokens included in the whole prompt count
- cache-aware cost estimation (`internal/usage`) using exact integer picodollars and JSON prices (`GATEWAY_PRICING_FILE`); unknown usage and prices remain unknown, never fabricated zeros
- required PostgreSQL configuration (`GATEWAY_DATABASE_URL`), a pgx connection pool, embedded forward-only migrations, a `migrate` command, and startup reachability/schema checks (`internal/postgres`)
- asynchronous, best-effort accounting for validated requests, including failures and cancellations, with bounded batching and shutdown draining; documented SQL reports by client, model, and UTC day
- environment configuration plus the clients and pricing files, graceful shutdown, automated tests, and CI with disposable PostgreSQL integration tests

Usage accounting is implemented; clients and hashed keys remain in the clients file, not PostgreSQL. Subsequent milestones introduce observability infrastructure, Redis, performance work, containers, Kubernetes, and AWS. MCP and tool execution come after the core platform is stable. Naming the next milestone does not authorize implementing it without a task.

Stay within the task and current milestone unless explicitly asked to change scope. A roadmap entry is not authorization to implement it opportunistically. Do not add streaming, model aliases, additional modalities, databases, frameworks, services, or infrastructure without a current requirement.

## Architecture and Package Boundaries

The current request path is:

```text
HTTP request → httpapi (auth → rate limit) → routing (fallback) → breaker → retry → provider adapter → upstream HTTP request
                  └──────────────────────────── shared internal/llm types ────────────────────────────┘
                  └─ validated request outcome → usage.Recorder → postgres → PostgreSQL
```

Respect the existing responsibilities:

```text
cmd/gateway/                 environment, clients/pricing files, wiring, migration command, server lifecycle
internal/llm/                neutral types, Provider interface, shared errors
internal/httpapi/            public wire DTOs, authentication header, validation, responses, error mapping
internal/routing/            model-to-provider routing
internal/retry/              bounded retries around one provider
internal/breaker/            circuit breaker around one provider
internal/auth/               gateway client keys and request identity
internal/ratelimit/          per-client request rate and concurrency limits
internal/usage/              record types, exact pricing, asynchronous recorder
internal/postgres/           pgx pool, embedded migrations, batch usage storage
internal/provider/openai/    OpenAI adapter and private upstream wire types
internal/provider/anthropic/ Anthropic adapter and private upstream wire types
docs/                       architecture and roadmap
.github/workflows/          continuous integration
```

- `internal/llm` imports no other project package and has no wire-format JSON tags.
- Public wire types belong in `httpapi`; upstream wire types stay private to their provider package.
- The HTTP layer depends on `llm.Provider`, not concrete adapters.
- The router implements the same provider interface; avoid adding a second abstraction for the same responsibility.
- Configuration and dependency wiring belong in `cmd/gateway`.
- The HTTP layer accepts a small recorder interface and pricing table, never a PostgreSQL store. `cmd/gateway` owns the database pool and recorder lifecycle.
- Keep provider-specific protocol details out of handlers and routing.

Do not create new top-level directories, provider registries, service layers, or configuration frameworks without a concrete need. Let abstractions follow implemented requirements.

## Public API and Compatibility

The gateway supports a documented subset of Chat Completions, not full OpenAI API or SDK compatibility.

- Validate and normalize public input before routing. Invalid requests must not reach a provider.
- Preserve request-body limits, JSON type checks, message validation, and consistent defaults across providers.
- Preserve model names and message content when forwarding unless a documented translation requires otherwise.
- Keep exact-match routing and unknown-model behavior explicit. Do not silently add aliases, rewriting, or fallback.
- Preserve the documented treatment of unsupported fields: unknown request fields are currently ignored, while `stream: true` is rejected.
- Keep response translation and the gateway-owned error envelope consistent across providers.
- Treat changes to validation, defaults, supported fields, status codes, and error codes as public contract changes. Update tests and documentation together.

Do not advertise support for a capability simply because a provider supports it. Compatibility claims must match implemented behavior and verification.

## Provider Adapters

Keep the provider contract small:

```go
type Provider interface {
    Chat(ctx context.Context, req ChatRequest) (ChatResponse, error)
}
```

Provider implementations must:

- honor context cancellation and deadlines through outbound requests and response reads
- support concurrent calls without mutating requests or their message slices
- reuse HTTP clients and keep per-request state local
- accept injected clients and base URLs for deterministic local tests
- validate constructor inputs and reject invalid internal requests before network I/O
- close response bodies on every path and bound response reads
- validate upstream response structure and translate usage and finish reasons explicitly
- distinguish a valid empty completion from a missing or unusable response
- return upstream failures as `*llm.ProviderError`, preserving wrapped context errors

Do not fabricate missing token usage, silently accept unsupported response shapes, or classify unknown finish reasons as successful completion. Preserve documented provider-specific translation, including system messages and cached-token accounting.

When changing an upstream contract, verify it against official provider documentation and update representative fake-server fixtures. Distinguish fixture-based verification from live API testing; do not claim a model was tested live without evidence.

## Errors and Reliability

- Propagate `context.Context` across all I/O boundaries; never detach upstream request work from cancellation. The usage writer is the deliberate exception described below: accepted accounting records have a separate, bounded lifetime.
- Preserve error causes so callers can inspect them with `errors.Is` and typed matching.
- Distinguish client validation failures, unknown models, upstream failures, and unexpected gateway errors.
- Check context errors before general provider-error classification. A canceled provider operation alone does not prove that the incoming client disconnected.
- Keep public errors safe and stable. Upstream authentication failures indicate gateway/provider configuration problems, not invalid gateway-client credentials.
- Fail startup on invalid or incomplete configuration rather than silently disabling intended routes.
- Preserve bounded graceful shutdown and deterministic cleanup.
- Do not hide failures to make tests or demos pass.

The handler bounds all upstream work for a request with one time budget (`GATEWAY_UPSTREAM_TIMEOUT`). Each adapter is wrapped in a `retry.Provider` and then a `breaker.Breaker`, one breaker per provider shared by its own route and any fallback use. The router's fallback runs at most once, above both, and bounds the primary with `GATEWAY_PROVIDER_TIMEOUT`, a share of the same budget. New resilience layers must derive their deadlines from the request context rather than add independent timers, and must not introduce retry/fallback loops. Server header and shutdown timeouts are separate concerns. The retry-safety rules are documented in `docs/architecture.md`; the same applies to the fallback and breaker classification tables there. Change which failures are retried, fall back, or count against the breaker only deliberately, updating that documentation.

When implementing retries or failover, make failure classification, attempt limits, backoff, and total time budgets explicit. Cancellation must stop further work promptly. Consider duplicate generations and upstream charges; document retry safety rather than assuming generation requests are idempotent. Avoid nested retry/fallback loops.

## Security

Treat client requests, configuration, and upstream responses as untrusted input.

- Never commit or log API keys, authorization headers, or other credentials.
- Keep provider credentials server-side and separate from gateway-client credentials. Never forward a gateway key upstream or expose a provider key to clients.
- Store gateway keys only as hashes; never log keys, hashes, or `Authorization` header values. Authenticate and rate-limit before reading request bodies, and keep unknown and disabled keys indistinguishable in public responses.
- Never include secrets, raw upstream bodies, or prompt/completion content in public errors or diagnostic logs.
- Configuration errors should identify invalid settings without printing secret values.
- Preserve request and response size limits and validate external input before using it.
- Keep upstream destinations under server control; do not allow request data to choose arbitrary URLs or forward credentials to arbitrary hosts.
- Use HTTPS for production provider endpoints; local HTTP fake servers are appropriate for tests.
- Preserve the loopback default while the gateway serves plain HTTP: gateway keys would cross the network unencrypted. Do not imply the gateway is safe for public exposure without TLS terminated in front of it.
- Do not weaken validation or security boundaries for convenience.

If later milestones introduce tool execution or infrastructure access, design explicit authorization, validation, credential isolation, and least-privilege access. Do not execute model-generated commands as part of ordinary gateway request handling.

## Observability and Accounting

Make important runtime behavior diagnosable using the facilities appropriate to the milestone.

The current gateway uses `log/slog` and logs request failures at the HTTP boundary. Preserve useful error categories, request ID, client, provider, and model context, and upstream status without duplicating the same failure at every layer.

Usage records persist request latency, outcomes, provider-reported tokens, and estimated costs, but not prompt or completion content. Submit one record per request that passes body validation; authentication, gateway rate-limit, body-read, and validation rejections are excluded. Cancellations use accounting status `499 client_closed`, never a sent response. Keep unknown usage and cost as NULL, and do not add cache subsets to total input tokens again.

The background writer intentionally owns a context separate from requests, so cancellation does not discard an already accepted record. Each insert and shutdown drain remain bounded. Preserve the lifecycle order: finish or cancel HTTP work, wait for accepted handlers to enqueue their records, drain the recorder within its budget, then close the pool. Full queues and failed writes are logged and can lose records; database failures after startup must not fail otherwise successful requests.

SQL reporting is documented in `docs/usage.md`; an admin HTTP API, retention automation, and moving clients/keys into PostgreSQL are not implemented. As the roadmap introduces them, expose request and provider latency, retry and fallback activity, token usage, estimated cost, metrics, and traces. Do not add Prometheus, OpenTelemetry, or dashboards without a current requirement.

Keep provider-reported usage distinct from estimated pricing. Do not invent missing usage or present unverified costs as exact charges. Reliability and performance claims should be backed by reproducible measurements.

## Testing

Write tests for meaningful behavior, at the appropriate boundary:

- Unit tests for routing, validation, configuration, normalization, and error mapping.
- Provider tests with `httptest.Server` for wire formats, headers, response translation, and upstream failures.
- Complete request-path tests using the real handler, router, and adapters with fake upstream servers.
- Lifecycle and concurrency tests for cancellation, deadlines, graceful shutdown, and shared provider use.
- Pricing and recorder unit tests with a fake store, and PostgreSQL integration tests for migrations, storage, and complete accounting paths. Gate local database tests on `GATEWAY_TEST_DATABASE_URL`; fail rather than skip when it is missing in CI. Each database test must create and drop its own database, never clear a shared or operator database.

Automated tests must not require real provider credentials, external provider access, or paid API calls. Use synthetic credentials and local fake servers. Live smoke tests are optional and never a CI requirement.

Cover success and failure paths relevant to the change: malformed and oversized data, upstream status codes, transport failures, slow responses, cancellation during body reads, and context/error propagation. Add regression tests for bug fixes when practical.

Use channels or other explicit synchronization for concurrent tests. Use bounded deadlines as failure guards rather than arbitrary sleeps as the main synchronization mechanism. Ensure cleanup releases blocked handlers and goroutines.

Never weaken, delete, or bypass a meaningful test to make a change pass. Keep fixtures faithful to the upstream contract and test observable behavior rather than private implementation details.

## Dependencies

Before adding a dependency, determine whether the standard library or an existing dependency is sufficient. The current implementation uses the standard library for HTTP, JSON, logging, configuration, and tests, plus `pgx/v5` and `pgxpool` for PostgreSQL access.

For new dependencies, prefer active maintenance, a small API surface, clear licensing, and minimal transitive cost. Provider SDKs, web frameworks, and resilience libraries require a concrete benefit; do not add them by default.

Use the Go version declared in `go.mod` and reflected in CI. Do not change the toolchain or introduce another runtime incidentally.

## Documentation

Update relevant documentation when changing:

- architecture or package boundaries
- public API behavior or provider translation
- configuration, security assumptions, or developer workflow
- timeout, retry, fallback, or shutdown behavior
- usage accounting, persistence, deployment, or major dependencies
- milestone status or acceptance criteria

Keep `README.md` focused on usage and supported behavior, `docs/architecture.md` on implemented decisions, and `docs/ROADMAP.md` on staged scope. Use ADRs for meaningful long-term trade-offs, not trivial implementation choices.

## Development Workflow

Before implementation:

1. Read relevant documentation and inspect the existing implementation.
2. Determine milestone scope and applicable constraints.
3. Understand current contracts and tests.
4. Choose the smallest coherent implementation.

During implementation:

1. Keep changes focused and preserve behavior unless the task intentionally changes it.
2. Add or update meaningful tests and documentation.
3. Avoid unrelated refactors and speculative future features.
4. Preserve user changes already present in the worktree.

After implementation:

1. Format changed Go files.
2. Run relevant tests and the verification required by the change.
3. Run static checks and integration checks where applicable.
4. Review the diff for unnecessary changes, secrets, and generated artifacts.
5. Report what changed, what was verified, and any remaining limitations.

## Commands

Use the Makefile; it is the single list of verification commands, and CI runs its targets.

Format changed Go files with `gofmt -w <files>`. The CI-equivalent checks are:

```bash
make check   # fmt, vet, test, build, vuln
```

The individual targets are `make fmt`, `make vet`, `make test` (`go test -race -timeout 2m ./...`), `make build`, and `make vuln` (the only one that needs network access). `make fmt` fails on any unformatted file, so its exit status is enough.

`make smoke` runs `cmd/gateway/smoke_test.go` (build tag `smoke`) against the real provider APIs with keys from the environment. It costs money, so run it only when the developer asks, never in CI or by default. When adapter or wire-format behavior changes, suggest it to the developer and report whether it was run. Keep it compiling: `make vet` vets it.

Run locally with provider configuration, clients and pricing files, and a reachable migrated PostgreSQL database, as described in the README:

```bash
go run ./cmd/gateway
```

`go run ./cmd/gateway migrate` needs only `GATEWAY_DATABASE_URL`. Normal startup checks the schema but never migrates automatically. `make db` starts disposable local PostgreSQL; set `GATEWAY_TEST_DATABASE_URL` from its output to include integration tests in `make check`. `make db-stop` deletes that container's data, so do not use it as persistent storage.

Keep the Makefile, `.github/workflows/ci.yml`, and documented commands aligned. Documentation-only changes need content and reference checks; do not claim code tests ran when they did not.

## Git and AI-Assisted Development

- Keep changes and pull requests small enough to review, with one clear purpose.
- Use clear commit messages; suggested prefixes are `feat:`, `fix:`, `test:`, `docs:`, `refactor:`, `chore:`, and `ci:`.
- Do not commit secrets, local configuration containing credentials, generated temporary files, or unnecessary IDE files.
- Architectural ownership and final review remain with the developer. AI-generated code must meet the same standards as other contributions.
- For ambiguous decisions, prefer the simpler design, preserve current boundaries, avoid unnecessary dependencies, and document meaningful trade-offs.
- Present trade-offs before large, irreversible architectural changes.

## Definition of Done

A change is complete when, where applicable, it:

- satisfies the requested behavior within milestone scope
- preserves API contracts and architectural boundaries, or documents intentional changes
- handles errors, cancellation, resource cleanup, and concurrent use correctly
- includes meaningful tests and passes formatting, static checks, tests, and build checks
- preserves credential isolation, validation, and safe diagnostics
- avoids unnecessary dependencies and unrelated changes
- updates relevant documentation
- leaves the gateway runnable and demonstrable

Compilation alone is not completion. Report unverified behavior and remaining limitations explicitly.
