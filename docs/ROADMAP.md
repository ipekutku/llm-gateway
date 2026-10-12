# LLM Gateway Engineering Roadmap

This document defines the incremental development roadmap for the LLM Gateway.

The project is intentionally built in small, production-quality milestones. Each milestone must leave the repository in a working, testable, and demonstrable state.

The goal is to avoid building a large platform upfront and instead evolve the system only when the previous layer is stable.

---

# Guiding Principles

* Every milestone must produce a usable version of the system.
* `main` must remain buildable and testable.
* Prefer small, reviewable pull requests.
* Avoid speculative abstractions.
* Prefer the Go standard library unless an external dependency provides clear value.
* External systems must be replaceable or testable locally.
* Production concerns should be introduced deliberately rather than all at once.
* Architecture decisions should be documented when meaningful tradeoffs exist.
* AI coding agents may accelerate implementation, but architecture, review, verification, security, and engineering decisions remain human-owned.
* Performance and reliability claims should eventually be backed by measurements.

---

# Milestone 0 — Repository Foundation

**Target version:** pre-v0.1

## Objective

Establish a professional repository and development workflow before implementing product functionality.

## Scope

* Git repository
* Go module
* basic executable entry point
* README
* CI pipeline
* GitHub pull request workflow
* initial documentation structure

## Verification

The repository should pass:

```bash
go vet ./...
go test -race ./...
```

CI should execute the same checks for pull requests and changes to `main`.

## Deliverables

```text
.github/
└── workflows/
    └── ci.yml

cmd/
└── gateway/
    └── main.go

docs/

.gitignore
go.mod
README.md
```

## Exit Criteria

* CI passes on `main`
* repository builds successfully
* README explains project purpose
* development proceeds through branches and pull requests

---

# Milestone 1 — Provider Abstraction and Static Routing

**Target version:** `v0.1.0`

## Objective

Build the smallest useful LLM gateway.

A client sends an LLM request to the gateway, and the gateway routes the request to one of two interchangeable providers.

```text
Application
    │
    ▼
LLM Gateway
    │
    ├── Provider A
    └── Provider B
```

## Scope

### HTTP API

Implement:

```text
POST /v1/chat/completions
```

Support a deliberately small OpenAI-compatible request format.

Example:

```json
{
  "model": "example-model",
  "messages": [
    {
      "role": "user",
      "content": "Explain TCP."
    }
  ],
  "max_tokens": 100
}
```

### Internal LLM Model

Introduce vendor-neutral types such as:

```text
ChatRequest
ChatResponse
Message
Usage
```

Provider-specific request and response types must not leak outside provider packages.

### Provider Interface

Define a small abstraction similar to:

```go
type Provider interface {
    Chat(ctx context.Context, req ChatRequest) (ChatResponse, error)
}
```

### Provider Implementations

Implement two interchangeable providers.

For example:

```text
OpenAI
Anthropic
```

### Static Routing

Initially route models using static configuration.

Conceptually:

```text
model-a -> provider A
model-b -> provider B
```

No dynamic routing decisions are required yet.

### Context Propagation

Incoming request context must propagate through:

```text
HTTP request
    ↓
handler
    ↓
router
    ↓
provider
    ↓
outbound HTTP request
```

### Testing

Provider tests must not require real provider APIs.

Use fake HTTP servers to test cases such as:

```text
200 response
400 response
401 response
429 response
500 response
malformed JSON
request cancellation
slow upstream
```

## Non-Goals

Do not add:

* streaming
* retries
* failover
* circuit breakers
* authentication
* rate limiting
* PostgreSQL
* Redis
* Prometheus
* OpenTelemetry
* Docker
* Kubernetes
* AWS
* MCP

## Exit Criteria

* two providers implement the same abstraction
* model routing works
* `/v1/chat/completions` works
* provider-specific types remain isolated
* tests use mocked/fake provider servers
* context cancellation works
* errors are mapped consistently
* CI is green
* architecture is documented

---

# Milestone 2 — Timeouts and Retry Policy

**Target version:** `v0.2.0`

## Objective

Make communication with unreliable upstream LLM providers safer.

## Scope

### Timeouts

Introduce explicit timeouts for upstream requests.

Potential timeout categories:

```text
request timeout
provider timeout
connection timeout
idle connection timeout
```

Timeout behavior must be configurable.

### Retries

Implement bounded retries for retryable failures.

Potential retryable conditions:

```text
429 Too Many Requests
502 Bad Gateway
503 Service Unavailable
504 Gateway Timeout
temporary network failures
```

Do not retry arbitrary client errors.

### Backoff

Introduce exponential backoff with jitter.

### Request Safety

Clearly document which requests are safe to retry.

## Testing

Test:

* successful first attempt
* successful retry
* retry exhaustion
* non-retryable response
* context cancellation during retry
* timeout exhaustion

## Non-Goals

Do not add:

* fallback between providers
* circuit breakers
* distributed queues
* database persistence

## Exit Criteria

* timeout behavior is deterministic
* retries are bounded
* context cancellation stops retries immediately
* retry behavior is covered by tests
* metrics are not required yet

---

# Milestone 3 — Provider Failover and Circuit Breaking

**Target version:** `v0.3.0`

## Objective

Allow requests to survive provider degradation.

## Scope

### Provider Fallback

Allow one logical model to have:

```text
primary provider
fallback provider
```

Example:

```text
logical-model-x
    │
    ├── Primary: Provider A
    │
    └── Fallback: Provider B
```

### Failure Classification

Define which failures should trigger fallback.

For example:

```text
provider timeout
provider unavailable
rate limit
temporary upstream failure
```

Invalid client requests should not cause fallback.

### Circuit Breaker

Introduce provider health states such as:

```text
closed
open
half-open
```

Avoid continuously sending traffic to a provider that is clearly failing.

### Routing Policy

Routing starts evolving from:

```text
model -> provider
```

toward:

```text
model -> routing policy -> provider
```

## Testing

Test:

* primary provider success
* primary failure + fallback success
* both providers fail
* circuit opens
* circuit remains open
* half-open recovery
* cancellation during fallback

## Exit Criteria

* provider failover works predictably
* circuit breaker behavior is tested
* failures are classified explicitly
* no retry/fallback loops are possible

---

# Milestone 4 — Authentication and Rate Limiting

**Target version:** `v0.4.0`

## Objective

Turn the gateway into a multi-client internal platform rather than an anonymous proxy.

## Scope

### API Keys

Clients authenticate using gateway-issued API keys.

Example:

```text
Authorization: Bearer <gateway-api-key>
```

Do not expose provider credentials to applications.

### Request Identity

Each request should have an identified client or project.

Conceptually:

```text
API key
    ↓
client/project identity
    ↓
gateway policy
```

### Rate Limiting

Introduce rate limiting by client.

Possible dimensions:

```text
requests per second
requests per minute
concurrent requests
```

Start with a simple implementation.

Redis-backed distributed limiting can come later.

### Security

* never log secrets
* compare credentials safely
* validate authorization headers
* return consistent authentication errors

## Testing

Test:

* valid API key
* invalid key
* missing key
* disabled key
* rate limit exceeded
* independent limits between clients

## Exit Criteria

* anonymous access is rejected
* client identity is available throughout request processing
* rate limiting works
* provider credentials remain server-side only

---

# Milestone 5 — Usage and Cost Accounting

**Target version:** `v0.5.0`

**Status:** Feature-complete. Usage accounting is wired into the gateway, with SQL reporting documented in [usage.md](usage.md). Milestone 6 adds metrics, tracing, and dashboards over this baseline.

## Objective

Measure how the gateway is being used and what that usage costs.

## Scope

### Token Usage

Track:

```text
input tokens
cache read input tokens
cache write input tokens
output tokens
total tokens (input + output)
```

Input is the whole prompt; cache reads and writes are subsets of it, not additional tokens. Unknown usage remains unknown rather than being fabricated.

### Cost Calculation

Maintain provider/model pricing metadata.

Calculate estimated request cost.

Prices are loaded from the required JSON pricing file, with separate input, cache-read, cache-write, and output rates. Estimation uses exact integer picodollar arithmetic and the answering provider's configured model price. A missing price produces unknown cost, never zero; historical costs are not recalculated when prices change.

Conceptually:

```text
provider response
    ↓
usage normalization
    ↓
pricing lookup
    ↓
cost calculation
```

### Request Records

Store usage information associated with:

```text
request ID
client
model
provider
token usage
estimated cost
latency
success/failure
timestamp
```

The handler submits one record for every request that passes body validation, including success, unknown model, upstream failure, timeout, and client cancellation. Authentication, gateway rate-limit, and body/validation rejections are excluded. A bounded asynchronous writer persists metadata only; overload, failed writes, and shutdown deadlines can lose records. Persisted usage is not an exact bill or a complete traffic audit.

### PostgreSQL

PostgreSQL stores usage records and schema migration history. The database is required, and startup checks reachability and schema compatibility before listening. An explicit `migrate` command applies embedded, forward-only migrations under an advisory lock. Database outages after startup affect accounting, not the upstream response.

API clients and hashed keys remain in the clients file; pricing metadata remains in the pricing file. Usage records store `client_id` as plain text with no client foreign key. Moving clients or keys to PostgreSQL, down migrations, retention automation, and an admin HTTP API are out of scope. Admin APIs remain in the [future list](#possible-future-milestones).

## Database Engineering

Add:

* schema migrations
* connection pooling
* database configuration
* migration workflow
* integration tests

## Exit Criteria

* [x] request usage is persisted — the real PostgreSQL accounting test verifies handler → background writer → database, with the documented best-effort limits
* [x] costs are calculated — cache-aware, exact estimation is wired into the handler; missing prices remain NULL
* [x] usage can be queried — [SQL reports](usage.md) cover clients, answering models, UTC days, and request IDs, including unknown-value coverage
* [x] database migrations are reproducible — embedded migrations and the independent `migrate` command are tested for fresh, repeated, concurrent, and incompatible migration histories
* [x] tests can run against a disposable database — `make db` supports local tests; CI provides PostgreSQL and requires the database setting; each integration test creates and drops its own database

Implemented decisions and test boundaries are described in [architecture.md](architecture.md#cost-estimation). Local configuration and verification commands are in the [README](../README.md#running-locally).

---

# Milestone 6 — Observability

**Target version:** `v0.6.0`

**Status:** Feature-complete. Structured logging, Prometheus metrics, optional OpenTelemetry tracing, and the local Grafana dashboard are implemented. Milestone 7 is next. Release tagging follows merged-commit CI and the [release checks](../README.md#development).

## Objective

Make the gateway observable as a production service.

## Scope

### Structured Logging

Introduce structured logs.

Useful fields:

```text
request_id
client_id
model
provider
latency
status
error_type
retry_count
fallback
```

Secrets and prompt content should not be logged by default.

### Prometheus Metrics

Potential metrics:

```text
gateway_requests_total
gateway_request_duration_seconds
provider_requests_total
provider_request_duration_seconds
provider_errors_total
provider_retries_total
provider_fallback_total
provider_circuit_state
token_usage_total
estimated_cost_total
```

### OpenTelemetry

Trace:

```text
incoming request
    ↓
routing
    ↓
provider call
    ↓
retry/fallback
    ↓
database operation
```

Propagate trace context where appropriate.

### Grafana

Create useful dashboards.

Potential views:

```text
request volume
p50/p95/p99 latency
error rate
provider latency
provider error rate
fallback rate
token consumption
cost
```

The implemented log fields, metric names, and the decisions behind deviations from the names above are in [architecture.md](architecture.md#metrics) and its [observability decisions](architecture.md#observability-decisions).

## Exit Criteria

* [x] logs are structured — text or JSON, request/client/trace context, and one outcome line per validated request; request-path tests cover retries, fallback, failures, and cancellation
* [x] Prometheus metrics are exposed — a separate loopback-default listener exports bounded request/provider labels, resilience activity, tokens, and estimated costs; scenario tests verify the values
* [x] distributed traces are generated — optional OTLP/HTTP export, incoming trace context, per-attempt spans and retry/fallback events, plus linked asynchronous database writes and migration spans; fake-collector and PostgreSQL tests verify export and linkage
* [x] Grafana dashboards are documented — the provisioned local stack, commands, panels, and platform limitations are documented in the [README](../README.md#local-dashboards); metric-query coverage is tested and PR 8's fake-traffic verification is recorded in the [architecture](architecture.md#local-observability-stack)
* [x] sensitive data is excluded by default — combined logs/metrics/exported-span marker tests cover both providers, success, retries, fallback, and rejection/protocol errors; additional tests guard database/collector diagnostics and provider redirect isolation

---

# Milestone 7 — Redis and Distributed Platform Behavior

**Target version:** `v0.7.0`

**Status:** Feature-complete. Per-client rate limits and concurrency limits can be shared through Redis (`GATEWAY_REDIS_URL`), with per-instance limits while Redis is unavailable; store metrics and dashboard panels show which applies. Circuit breakers stay per instance by decision. Milestone 8 is next. Release tagging follows merged-commit CI and the [release checks](../README.md#development).

## Objective

Prepare the gateway for multiple running instances.

## Scope

Introduce Redis only where distributed shared state provides clear value.

Potential uses:

```text
distributed rate limiting
shared circuit breaker state
short-lived caching
coordination
```

Do not use Redis as a default storage layer for everything.

### Distributed Rate Limiting

Move local limits to Redis-backed limits where appropriate.

### Failure Handling

The gateway must define behavior when Redis becomes unavailable.

Avoid turning Redis into an unnecessary single point of failure.

## Testing

Test:

* multiple gateway instances
* consistent shared rate limits
* Redis failure
* recovery after Redis outage

The implemented responsibilities, failure behavior, and the decisions behind what stays out of Redis are in [architecture.md](architecture.md#shared-limits-in-redis).

## Exit Criteria

* [x] horizontal replicas behave consistently — complete-gateway tests run two instances against one Redis: a client's request rate and concurrent requests are shared across them, other clients stay independent, and a crashed instance's slots free themselves when their lease expires
* [x] Redis responsibilities are clearly documented — Redis holds only rate-limit state; circuit breaker state, caching, and coordination stay out, each with its reason, in the [architecture](architecture.md#shared-limits-in-redis); setup and limits are in the [README](../README.md#shared-rate-limits-with-redis)
* [x] Redis failure behavior is predictable — a documented [event table](architecture.md#when-redis-is-unavailable): a failed or slow Redis switches each instance to its own limits with one warning, one probe per second restores shared limits, and startup does not require Redis; unit tests with a switchable store and two-instance tests through a TCP proxy that is cut and restored verify outage and recovery, and `gateway_ratelimit_store_*` metrics show the mode, latency, and errors

---

# Milestone 8 — Performance Engineering

**Target version:** `v0.8.0`

**Status:** Feature-complete. Request-path benchmarks, reproducible end-to-end load scenarios, CPU/heap captures, and [measurements](performance.md) cover throughput, percentile comparison differences, memory, errors, accounting loss, Redis admission/release and pool waits, HTTP/1.1/TLS/HTTP/2 reuse, inbound keep-alives, and binary size. The one production change they justified, a larger upstream idle pool, removed the HTTP/1.1 connection churn that made the high-concurrency runs fail, with [before/after measurements](performance.md#upstream-idle-pool). Milestone 9 is next. Release tagging follows merged-commit CI and the [release checks](../README.md#development).

## Objective

Measure gateway overhead and identify scaling limits.

## Scope

### Benchmarks

Use Go benchmarks for important internal components.

Potential targets:

```text
routing
authentication
rate limiting
request serialization
cost calculation
```

### Load Testing

Introduce a tool such as:

```text
k6
Vegeta
wrk
```

Measure:

```text
requests per second
p50 latency
p95 latency
p99 latency
error rate
CPU usage
memory usage
gateway overhead
```

### Profiling

Use:

```text
pprof
```

where useful.

### Connection Management

Review:

```text
HTTP connection pooling
keep-alives
idle connections
provider client reuse
```

## Deliverables

Create reproducible performance scenarios.

Example:

```text
1 instance
500 concurrent clients
fake upstream provider
N requests
```

Document results rather than making unsupported claims.

## Exit Criteria

README or benchmark documentation contains measurable results.

Example:

```text
X requests/sec
Y ms p95 gateway overhead
Z MiB memory under load
```

* [x] README or benchmark documentation contains measurable results — the [README](../README.md#performance) and [performance.md](performance.md) report, for one instance with 500 concurrent clients and a 500 ms fake provider over 30 seconds, 977 successful requests/sec, a 2.8 ms p95 gateway-minus-baseline difference, and 88.5 MiB sampled peak RSS; plus about 21,900 requests/sec against an immediate fake provider, a fixed-arrival sweep up to 16,000 requests/sec, component benchmarks, and the environment, commands, raw run IDs, and limitations of each measurement. The scaling limits found are documented with them: upstream HTTP/1.1 connection churn (fixed, with before/after runs), the usage writer dropping records above about 8,000 requests/sec on the test machine, Redis admission and release costs and pool waits, socket I/O and the synchronous outcome log at saturation, and same-machine generator limits, each unchanged limit with its reason

---

# Milestone 9 — Containerization and Kubernetes

**Target version:** `v0.9.0`

**Status:** Feature-complete. A multi-platform distroless image, a Helm chart with a migration hook, health probes, a configurable shutdown drain, a disruption budget, CPU autoscaling, and native sidecars run in a local kind cluster, installed and checked in CI. A scripted [demonstration](architecture.md#demonstrations) covers autoscaling, shared limits across pods, `SIGTERM`, a rolling restart under load, and a database outage. Milestone 10 is next. Release tagging follows merged-commit CI and the [release checks](../README.md#development).

## Objective

Package and operate the gateway as a cloud-native service.

## Scope

### Docker

Create a production-oriented Docker image.

Consider:

```text
multi-stage build
minimal runtime image
non-root user
health checks
small image size
```

### Kubernetes

Introduce Kubernetes manifests through Helm.

Resources may include:

```text
Deployment
Service
ConfigMap
Secret references
ServiceAccount
HorizontalPodAutoscaler
PodDisruptionBudget
```

### Health Endpoints

Separate:

```text
liveness
readiness
```

Readiness should consider dependencies appropriately.

### Graceful Shutdown

Handle:

```text
SIGTERM
connection draining
in-flight requests
database shutdown
telemetry shutdown
```

### Helm

Make key deployment parameters configurable.

## Exit Criteria

* [x] gateway runs locally in Kubernetes — `make kind-up kind-deploy kind-check` runs it in a single-node kind cluster with disposable PostgreSQL and Redis and the fake provider as a sidecar, locally and in a CI job that sends one completion per provider through the Service and checks the metrics port
* [x] Helm installation works — `deploy/helm/llm-gateway` installs with a pre-install/pre-upgrade migration Job; `make helm-lint` lints and renders it in CI and checks that invalid values are rejected; install, upgrade, a failed migration, and recovery were verified on kind
* [x] horizontal scaling works — two pods by default with a preference for different nodes, a PodDisruptionBudget, and an optional CPU HorizontalPodAutoscaler; the demonstration scaled from two to three pods at 200 requests/s with no failed request, and Redis kept one client's burst of 10 across three pods (10 of 30 admitted; 29 without Redis)
* [x] graceful shutdown is demonstrated — `GATEWAY_SHUTDOWN_DELAY` and `GATEWAY_SHUTDOWN_TIMEOUT` with lifecycle tests; after `SIGTERM` a pod failed readiness within 0.5 s and answered completions for its 5-second delay before closing its listener, and a rolling restart under 50 requests/s completed 4,499 of 4,500 requests; the one failure, a reset keep-alive connection as an old pod closed its listener, led to `Connection: close` during the delay, after which 144,000 requests across 20 restarts at 200 requests/s all succeeded (see the [demonstration](architecture.md#demonstrations))
* [x] health checks behave correctly — `/livez` and `/readyz` are tested without telemetry; readiness ignores dependencies, and with PostgreSQL deleted for a minute all pods stayed ready and every request succeeded, while lost usage records were logged

---

# Milestone 10 — AWS Infrastructure

**Target version:** `v1.0.0`

**Status:** In progress. The [Terraform bootstrap](aws.md) defines the persistent S3 state bucket and private ECR repositories, with credential-free validation in CI. [Manual bootstrap verification](aws.md#bootstrap-verification--2026-10-11) confirmed a no-change plan, encrypted/versioned S3 state, and both arm64 image pushes with completed basic scans and no findings reported. CI scans both images for amd64 and arm64 on every pull request and publishes them to GHCR from `main`, tagged with the commit SHA. It uses no AWS credentials, since the project cannot create a GitHub OIDC identity provider ([why](aws.md#ci-images-and-aws-credentials)). The [platform stack](aws.md#platform-sessions) creates a two-AZ VPC and an EKS 1.37 cluster with two nodes for each working session and destroys it afterwards; a [live session](aws.md#platform-verification--2026-10-12) verified creation, nodes, add-ons, and teardown. Private RDS PostgreSQL 18, single-node ElastiCache Valkey, and Secrets Manager containers have [live creation and store TLS/authentication checks](aws.md#store-verification--2026-10-12), a corrected no-change plan, and complete teardown of all 67 platform resources with no checked leftovers. Gateway integration, deployment, and ingress are not implemented yet.

## Objective

Deploy the complete platform to AWS using Infrastructure as Code.

## Scope

### Terraform

Provision infrastructure reproducibly.

Potential AWS components:

```text
VPC
public/private subnets
EKS
RDS PostgreSQL
ElastiCache Redis
Application Load Balancer
IAM
Secrets Manager
CloudWatch integration
DNS/TLS where appropriate
```

### EKS

Deploy the gateway through Helm.

### IAM

Prefer workload identity / IAM roles rather than static AWS credentials.

### Secrets

Provider API keys and sensitive platform credentials should use a managed secret solution.

### Networking

Document:

```text
ingress path
private dependencies
security groups
network boundaries
```

### RDS

Use managed PostgreSQL.

### ElastiCache

Use managed Redis where required.

### CI/CD

Extend GitHub Actions to support controlled deployment.

Potential flow:

```text
Pull Request
    ↓
test
    ↓
lint
    ↓
build
    ↓
container scan
    ↓
merge
    ↓
container publish
    ↓
deployment
```

Avoid automatic production deployment unless the process is intentionally designed for it.

## Exit Criteria

* infrastructure can be created through Terraform
* gateway runs on EKS
* PostgreSQL runs on RDS
* Redis runs on ElastiCache
* secrets are not stored in Git
* deployment is reproducible
* architecture and operational documentation are complete

---

# Milestone 11 — MCP and Tool Execution

**Target version:** post-v1.0

## Objective

Extend the platform beyond simple model requests into controlled tool-enabled AI execution.

This milestone should only begin once the core gateway is stable.

## Potential Scope

### MCP

Support integration with MCP-compatible servers.

Conceptually:

```text
Application
    ↓
LLM Gateway
    ↓
Model
    ↓
Tool request
    ↓
MCP / tool layer
    ↓
external capability
```

### Tool Registry

Maintain available tools and capabilities.

### Authorization

Tool access should depend on client identity and policy.

### Security

Pay particular attention to:

```text
prompt injection
tool authorization
credential isolation
network access
input validation
output validation
auditability
```

### Auditing

Record tool execution metadata.

## Exit Criteria

Tool execution is controlled, authenticated, observable, and auditable.

---

# Possible Future Milestones

These should not be scheduled until there is a clear reason to build them.

Potential directions include:

* semantic or policy-based model routing
* latency-aware routing
* cost-aware routing
* provider health scoring
* streaming responses
* embeddings APIs
* image/model modality support
* model aliases
* request caching
* semantic caching
* per-team budgets
* quotas
* admin APIs
* policy engine
* audit logs
* usage analytics
* multi-region deployment
* disaster recovery
* provider credential rotation
* Kubernetes operators
* admission policies
* chaos testing

The presence of an idea here does **not** mean it should automatically be implemented.

---

# Version Overview

| Version   | Milestone                   | Main Engineering Theme   |
| --------- | --------------------------- | ------------------------ |
| pre-v0.1  | Repository Foundation       | Engineering workflow     |
| v0.1      | Providers + Routing         | Software architecture    |
| v0.2      | Timeout + Retry             | Resilience               |
| v0.3      | Failover + Circuit Breaking | Reliability              |
| v0.4      | Auth + Rate Limiting        | Platform security        |
| v0.5      | Usage + Cost + PostgreSQL   | Persistence              |
| v0.6      | Metrics + Tracing           | Observability            |
| v0.7      | Redis                       | Distributed state        |
| v0.8      | Load Testing                | Performance engineering  |
| v0.9      | Docker + Kubernetes + Helm  | Platform operations      |
| v1.0      | AWS + Terraform             | Cloud infrastructure     |
| post-v1.0 | MCP + Tools                 | AI platform capabilities |

---

# Pull Request Philosophy

A milestone should be composed of multiple small pull requests rather than one large implementation.

For example, Milestone 1 may be implemented as:

```text
PR 1 — minimal HTTP service
PR 2 — internal LLM types
PR 3 — provider interface
PR 4 — static router
PR 5 — first provider adapter
PR 6 — second provider adapter
PR 7 — chat completions handler
PR 8 — integration tests
PR 9 — documentation and cleanup
```

Every pull request should ideally:

* have one clear purpose
* contain appropriate tests
* pass CI
* avoid unrelated refactoring
* leave the repository working
* be understandable during code review

---

# AI-Assisted Development Workflow

AI coding agents are part of the development process, but they do not replace engineering ownership.

A typical change should follow this flow:

```text
Engineering task
      ↓
Agent investigates repository
      ↓
Agent proposes implementation
      ↓
Human evaluates architecture
      ↓
Agent implements small change
      ↓
Automated verification
      ↓
Independent agent review
      ↓
Human reviews diff
      ↓
Pull request
      ↓
CI
      ↓
Merge
```

Claude Code and Codex may be assigned different roles.

For example:

```text
Claude Code
    ↓
primary implementation

Codex
    ↓
independent code review

Developer
    ↓
architecture + final decision

CI
    ↓
objective verification
```

The goal is not to maximize the amount of generated code.

The goal is to use AI to increase engineering throughput while maintaining understanding, quality, and ownership.

---

# Definition of Done

A feature is not complete simply because it works locally.

Depending on the milestone, completion should include:

* implementation
* automated tests
* error handling
* documentation
* CI verification
* security review where relevant
* observability where relevant
* performance validation where relevant
* architecture documentation for meaningful decisions

The repository should always tell a clear engineering story:

> start simple, measure, identify the next limitation, and introduce complexity only when that limitation justifies it.
