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

## Project Status

v0.1 through v0.5 are complete; v0.6 observability is in progress. Each milestone leaves the gateway runnable and tested. Scope and exit criteria live in [docs/ROADMAP.md](docs/ROADMAP.md), and design decisions in [docs/architecture.md](docs/architecture.md).

```text
v0.1  Provider abstraction + routing          ✅
v0.2  Timeouts + retries                      ✅
v0.3  Provider fallback + circuit breakers    ✅
v0.4  Authentication + rate limiting          ✅
v0.5  Usage + cost tracking + PostgreSQL      ✅
v0.6  Prometheus + OpenTelemetry              in progress
v0.7  Redis + multi-instance behavior
v0.8  Load testing + performance work
v0.9  Docker + Kubernetes + Helm
v1.0  AWS deployment with Terraform
```

### v0.6 — Observability (in progress)

Implemented so far (see [Logs, metrics, and traces](#logs-metrics-and-traces)):

* structured logs as text or JSON; every line logged while handling a request carries its request ID and, once authenticated, its client ID, including retry, fallback, and circuit breaker lines
* one outcome line per validated request with status, provider, latency, retry count, and whether a fallback ran
* Prometheus metrics on a separate, unauthenticated listener: request counts by model, status, and error code, and request latency by model, with model labels limited to the configured models
* provider metrics: every upstream attempt by outcome, its latency, errors by upstream status, retries, fallbacks, and each circuit breaker's state
* token and estimated cost metrics per provider and model, matching the usage records
* OpenTelemetry traces exported over OTLP/HTTP when an endpoint is configured: one span per request, routing step, provider, and upstream attempt, with retries and fallbacks as events, and the trace ID in every log line
* traces of usage database writes, linked to the requests they record, and of migrations
* a local Prometheus, Grafana, and Jaeger stack (`make observability`) with a provisioned dashboard

### v0.5 — Usage and Cost Accounting ✅

Per-client usage records and estimated costs, persisted in PostgreSQL:

* token usage keeps the prompt-cache detail providers price differently: cached input read (OpenAI and Anthropic) and written (Anthropic), as part of the input count
* the provider that answered is known for every response, including one served by a fallback
* every request gets a gateway-assigned ID, returned in the `X-Request-ID` header
* cost estimation from per-model prices for input, cached input read and written, and output tokens, with exact integer arithmetic; a model without a price has an unknown cost, never zero
* a PostgreSQL connection pool and usage store, with embedded forward-only migrations protected by an advisory lock, exact cost storage, and integration tests against disposable databases
* an asynchronous recorder with a bounded queue, batch writes, write timeouts, and shutdown draining; queue overflow is logged as a periodic count, failed writes are logged, and accepted records survive request cancellation
* an explicit migration command, startup schema checks, and one usage record for each validated request, including failures and cancellations
* documented SQL reports for usage and estimated cost per client, answering model, and UTC day; see [Querying usage and estimated cost](docs/usage.md)

### v0.4 — Authentication and Rate Limiting ✅

* every request needs a gateway-issued API key, sent as `Authorization: Bearer <key>`; anonymous requests get `401`
* gateway keys are separate from the provider keys, which stay server-side and never reach clients
* only SHA-256 hashes of keys are stored, in a clients file; keys can be disabled
* per-client rate limits: requests per minute with bursts, and concurrent requests, answered with `429` and `Retry-After`

### v0.3 — Provider Failover and Circuit Breaking ✅

* fallback pairs: a configured model can fall back to the other provider's configured model, tried once and never back again
* fallback on provider failures (timeouts, unavailability, rate limiting, server errors), not on rejected requests or client cancellations
* a per-provider time limit, so a primary that hangs leaves the fallback time to answer
* a circuit breaker per provider: after repeated failures the provider is skipped for a cooldown, then probed with a single request
* no retry/fallback loops: retries stay inside each provider, and fallback runs at most once per request

### v0.2 — Timeouts and Retry Policy ✅

* one upstream time budget per request, plus connection-setup limits
* bounded retries for transient failures: `429`, `502`, `503`, `504`, Anthropic's `529`, and failures to connect
* exponential backoff with jitter, honoring the provider's `Retry-After` header
* no retries of timeouts or failures after the request was sent, because a chat completion is not idempotent
* cancellation stops retries immediately; all attempts share the request's time budget

### v0.1 — Provider Abstraction and Routing ✅

* Go HTTP service with an OpenAI-compatible subset of `/v1/chat/completions`
* vendor-neutral request and response types behind a small provider interface
* OpenAI and Anthropic adapters
* static, exact-match model-to-provider routing
* automated tests and continuous integration

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

Requires Go 1.26.9 or later and PostgreSQL. For a disposable local database, use Docker:

```bash
make db
export GATEWAY_DATABASE_URL='postgres://gateway:gateway@127.0.0.1:55432/gateway?sslmode=disable'
go run ./cmd/gateway migrate
```

If the development container is already running, reuse its connection settings. `make db-stop` deletes its data, including recorded usage. Use your own persistent PostgreSQL instance to retain records. The `migrate` command requires only `GATEWAY_DATABASE_URL`; apply migrations before starting the gateway. Startup checks connectivity and that the schema is current, and fails without migrating automatically.

Create a gateway API key for your client and a clients file holding its hash:

```bash
GATEWAY_KEY=$(openssl rand -base64 32)   # give this key to the client
echo "$GATEWAY_KEY"
printf '{"clients": [{"id": "my-app", "key_sha256": "%s"}]}\n' \
  "$(printf %s "$GATEWAY_KEY" | shasum -a 256 | cut -d' ' -f1)" > clients.json
```

Save a pricing file as `prices.json`. Prices are non-negative decimal strings in USD per million tokens, with up to six decimal places, no sign or exponent, and a maximum of $1,000,000 per million tokens. The values below are illustrative; replace them with verified prices for your configured models:

```json
{
  "prices": [
    {"provider": "openai", "model": "gpt-4o", "input": "2.50", "cache_read": "1.25", "cache_write": "0", "output": "10"},
    {"provider": "anthropic", "model": "claude-opus-5-5", "input": "1", "cache_read": "0.10", "cache_write": "2", "output": "5"}
  ]
}
```

The file must contain a `prices` array; every entry requires the provider, configured model name, and all four prices. Unknown fields and duplicate provider/model entries are rejected. A model absent from the file has an unknown cost (`NULL`), while its token usage is still recorded. At startup the gateway logs a warning for each configured model without a price and for each price that matches no configured model, which usually means a typo in the provider or model name. `{"prices": []}` is valid if all costs should be unknown. Prices are read once at startup; restart after changing them.

Then configure at least one provider and start the gateway:

```bash
export GATEWAY_CLIENTS_FILE=clients.json
export GATEWAY_PRICING_FILE=prices.json

export OPENAI_MODEL=gpt-4o
export OPENAI_API_KEY=sk-...            # your OpenAI key

export ANTHROPIC_MODEL=claude-opus-5-5
export ANTHROPIC_API_KEY=sk-ant-...     # your Anthropic key

go run ./cmd/gateway
```

The gateway listens on `127.0.0.1:8080`, serves Prometheus metrics on `http://127.0.0.1:9464/metrics`, and logs the configured models and the number of clients. Stop it with `Ctrl+C` or `SIGTERM`; in-flight requests get up to 5 seconds to finish, then pending handlers and usage writes share an additional 5-second drain budget.

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
| `GATEWAY_DATABASE_URL` | Required. PostgreSQL URL or connection string. Startup requires a reachable database with the current schema; `go run ./cmd/gateway migrate` applies pending migrations. Never log or commit this setting if it contains credentials. For any database not on the local host, add `sslmode=verify-full` (with `sslrootcert` if the server's CA is not in the system store): the default, `prefer`, uses TLS when offered but does not verify the server, so the password and records could be intercepted. |
| `GATEWAY_PRICING_FILE` | Required. Path of the JSON pricing file, read once at startup. Missing model prices produce unknown costs and a startup warning. |
| `OPENAI_MODEL`, `OPENAI_API_KEY` | Enable OpenAI for one model. Set both or neither. |
| `ANTHROPIC_MODEL`, `ANTHROPIC_API_KEY` | Enable Anthropic for one model. Set both or neither. |
| `GATEWAY_ADDR` | Listen address. Default `127.0.0.1:8080`. |
| `GATEWAY_METRICS_ADDR` | Listen address of the unauthenticated Prometheus endpoint, `/metrics`. Default `127.0.0.1:9464`. Must differ from `GATEWAY_ADDR`. See [Logs, metrics, and traces](#logs-metrics-and-traces). |
| `GATEWAY_LOG_FORMAT` | `text` (default) or `json`, written to standard error. See [Logs, metrics, and traces](#logs-metrics-and-traces). |
| `OTEL_EXPORTER_OTLP_ENDPOINT` | Enables tracing: base URL of an OpenTelemetry collector accepting OTLP over HTTP, such as `http://127.0.0.1:4318`. Unset means no tracing. `OTEL_EXPORTER_OTLP_TRACES_ENDPOINT` (the full URL) also enables it, and the other standard `OTEL_*` exporter, sampler, and resource variables apply. See [Traces](#traces). |
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

Startup fails if the clients file is missing, malformed, has unknown fields, or enables no client, if no provider is configured, if only one variable of a pair is set, if both providers use the same model name, if a timeout or delay is not a positive duration, if the attempt count is outside 1–10, if the maximum retry delay is less than the base delay, if the provider timeout is not less than the upstream timeout, if a fallback names a provider that is not configured, or if an OTLP endpoint or protocol is invalid. Error messages name the variables and clients but never print keys, hashes, or other values.

Startup also fails if either accounting setting is absent, the pricing file is invalid, the database cannot be reached, or its migration history is behind, newer, or inconsistent with this binary. Database connectivity and schema checks share a 10-second startup budget. Migrations have a 1-minute budget and run transactionally under an advisory lock.

The model names above are examples. Any model the provider's API accepts can be configured. The automated tests run both adapters against fake servers built from the providers' documented API formats.

Verified against the live APIs with `make smoke` on 2026-10-09, last with the v0.5 adapters, which also read the cache fields of token usage:

| Configured model | Model reported by the provider | Checked |
|---|---|---|
| `gpt-4o` | `gpt-4o-2024-08-06` | completion ending in `stop`, forced `length` stop, token usage |
| `claude-opus-5-5` | `claude-opus-5-5` | completion ending in `stop`, forced `length` stop, token usage |

The smoke test does not store usage. A separate manual run on 2026-10-09 sent live requests through the gateway with PostgreSQL from `make db` and checked the stored records:

| Checked | Result |
|---|---|
| One request to each model | Stored with status 200, token usage, the reported model (`gpt-4o-2024-08-06` for `gpt-4o`), and `cost_usd` matching the configured prices exactly |
| The same ~2,000-token prompt sent to `gpt-4o` three times | First: no cache reads. Second and third: 1,792 of 1,965 input tokens read from OpenAI's prompt cache, charged at the cache-read price, with an estimated cost 45% lower |

Live runs have not covered refusals (`content_filter`), Anthropic prompt-cache reads and writes (the gateway does not request caching), or real upstream failures (retries, fallback, circuit breaking); those are tested only against fakes.

### Supported API

`POST /v1/chat/completions` implements a small subset of the OpenAI Chat Completions format. It is not a full OpenAI-compatible API. Every request needs `Authorization: Bearer <gateway key>`; a missing, unknown, or disabled key gets `401`.

| Request field | Support |
|---|---|
| `model` | Required. Must match a configured model. |
| `messages` | Required. `role` is `system`, `user`, or `assistant`; `content` is a non-empty string. An optional `system` message must come first. |
| `max_tokens` | Optional positive integer. Default `1024`. Sent to OpenAI as `max_completion_tokens` and to Anthropic as `max_tokens`. |
| `stream` | Only `false` or absent. Streaming is not supported. |
| anything else | Accepted but ignored (for example `temperature`, `tools`, `n`). |

The response contains exactly one choice with `finish_reason` `stop`, `length`, or `content_filter`, plus token usage. Every response, including errors, carries an `X-Request-ID` header; quote it when reporting a problem. A successful response's `id` is `chatcmpl-` followed by the same ID. An `X-Request-ID` sent by the client is ignored. Errors use the envelope `{"error": {"message", "type", "code"}}`; see [docs/architecture.md](docs/architecture.md#error-mapping) for the full status mapping.

### Logs, metrics, and traces

Logs go to standard error, as text by default or as one JSON object per line with `GATEWAY_LOG_FORMAT=json`. Every line logged while handling a request carries its `request_id`, the same ID returned in `X-Request-ID`, once the client is authenticated, its `client_id`, and, with tracing enabled, its `trace_id`. That includes the intermediate lines for retries, fallbacks, and circuit breaker changes, so all lines of one request can be found by its ID.

Each request that passes validation ends with exactly one `request completed` line:

```json
{"time":"2026-10-10T14:03:12.418+03:00","level":"INFO","msg":"request completed","model":"gpt-4o","status":200,"provider":"anthropic","latency":2350417000,"retry_count":2,"fallback":true,"request_id":"K5JDK633MTRLLKL7DSKKFGWKRU","client_id":"my-app"}
```

| Field | Meaning |
|---|---|
| `model` | The requested model. With a fallback, `provider` names the provider that actually answered. |
| `status`, `code` | The HTTP status and gateway error code, the same values as the usage record. `499` with `client_closed` means the client went away. `code` is absent on success. |
| `provider`, `upstream_status`, `error` | The provider that answered or failed, and the provider's status and error on failure. |
| `latency` | Time from receiving the request to finishing it; nanoseconds in JSON, a duration such as `2.35s` in text. |
| `retry_count`, `fallback` | Repeated upstream attempts over all providers tried, and whether a fallback provider was called. |

Successes and cancellations log at `INFO`, `4xx` responses at `WARN`, and `5xx` responses at `ERROR`. A request rejected before reaching a provider (`401`, `429`, or an invalid body) logs one `chat completion failed` line instead. Logs never contain prompts, completions, keys, or upstream response bodies.

Prometheus metrics are served on a separate listener, `GATEWAY_METRICS_ADDR`:

```bash
curl -s http://127.0.0.1:9464/metrics | grep '^gateway_'
```

| Metric | Labels | Meaning |
|---|---|---|
| `gateway_requests_total` | `model`, `status`, `code` | Chat completion requests, including authentication, rate-limit, and validation rejections. `code` is empty on success. |
| `gateway_request_duration_seconds` | `model` | Histogram of request latency, from 10 ms to 120 s. |
| `gateway_provider_requests_total` | `provider`, `outcome` | Upstream attempts, retries and fallbacks included; `outcome` is `success`, `error`, `timeout`, or `canceled`. |
| `gateway_provider_request_duration_seconds` | `provider` | Histogram of upstream attempt latency. |
| `gateway_provider_errors_total` | `provider`, `upstream_status` | Failed attempts by provider status; `0` means no usable response. |
| `gateway_provider_retries_total` | `provider` | Repeated attempts. |
| `gateway_provider_fallbacks_total` | `from_provider`, `to_provider` | Requests sent to the fallback provider. |
| `gateway_provider_circuit_state` | `provider` | Circuit breaker state: `0` closed, `1` half-open, `2` open. |
| `gateway_tokens_total` | `provider`, `model`, `type` | Provider-reported tokens; `type` is `input` (uncached), `cache_read`, `cache_write`, or `output`. |
| `gateway_estimated_cost_dollars_total` | `provider`, `model` | Estimated cost from the pricing file, in US dollars. |

`model` is one of the configured models or `unknown`, so model names sent by clients cannot create new series; requests rejected before their body was read are also `unknown`. Token and cost metrics use the configured model of the provider that answered, the name in the pricing file, and match the usage records: a request with unknown cost adds tokens but no cost. The metric cost is a floating-point estimate that resets on restart; the exact picodollar values are in PostgreSQL. Client IDs are never labels; per-client usage is in PostgreSQL. Go runtime (`go_*`) and process (`process_*`) metrics are included. A minimal Prometheus scrape configuration:

```yaml
scrape_configs:
  - job_name: llm-gateway
    static_configs:
      - targets: ["127.0.0.1:9464"]
```

#### Traces

Setting `OTEL_EXPORTER_OTLP_ENDPOINT` makes the gateway export OpenTelemetry traces over OTLP/HTTP (protobuf), for example to a local collector or Jaeger:

```bash
export OTEL_EXPORTER_OTLP_ENDPOINT=http://127.0.0.1:4318
```

Each request is one trace with service name `llm-gateway` (override with `OTEL_SERVICE_NAME`):

```text
POST /v1/chat/completions          request ID, client ID, status, retry count, fallback
└─ route                           requested model; "fallback" event
   ├─ provider openai              circuit breaker and retries; one "retry" event per retry
   │  ├─ chat gpt-4o               one upstream attempt: status, response model, token counts
   │  └─ chat gpt-4o
   └─ provider anthropic           only after a fallback
      └─ chat claude-opus-5-5
```

Failed spans have an error status and an `error.type`. A request rejected before routing (`401`, `429`, invalid body) has only the first span. Spans never contain prompts, completions, keys, or upstream response bodies.

Usage records are written in the background, so each batch insert is a trace of its own, `INSERT usage_records`, with the number of records and of rows inserted. It links to the request spans of the records it stores, so a tracing UI can navigate from a write to its requests. `go run ./cmd/gateway migrate` reads the same `OTEL_*` variables and exports a `migrate` span with one child per migration applied. Database spans never contain record values, SQL parameters, or the database URL.

A client may send a W3C `traceparent` header; the gateway's spans then join the client's trace, and the client's sampling decision applies. The gateway never sends trace headers to OpenAI or Anthropic. Spans are exported in batches in the background; failed exports are logged as `tracing error` warnings and never affect requests. Shutdown flushes pending spans last, after the usage records are written, within a 5-second budget of its own. Only OTLP over HTTP is supported: an endpoint that is not an `http` or `https` URL, or `OTEL_EXPORTER_OTLP_PROTOCOL` other than `http/protobuf`, fails startup. Use `https` for a collector on another host, and put collector credentials in `OTEL_EXPORTER_OTLP_HEADERS`, not in the URL, which can appear in export error logs.

#### Local dashboards

`make observability` starts Prometheus, Grafana, and Jaeger in Docker (Compose files in [`deploy/observability`](deploy/observability)) for a gateway running on the host:

```bash
make observability
export OTEL_EXPORTER_OTLP_ENDPOINT=http://127.0.0.1:4318
go run ./cmd/gateway
```

| Service | Address | Use |
|---|---|---|
| Grafana | http://127.0.0.1:3000 | The provisioned **LLM Gateway** dashboard, the home page; no login |
| Prometheus | http://127.0.0.1:9090 | Scrapes the gateway's `/metrics` every 5 seconds; keeps 2 days |
| Jaeger | http://127.0.0.1:16686 | Traces: search for service `llm-gateway`; the dashboard links here |
| OTLP receiver | http://127.0.0.1:4318 | Jaeger's OTLP/HTTP endpoint, the gateway's `OTEL_EXPORTER_OTLP_ENDPOINT` |

The dashboard shows request volume, error rate, and p50/p95/p99 latency; errors by status and code; upstream attempts by outcome, provider latency, provider error rate, errors by upstream status, retries, fallbacks, and each circuit's state; tokens by type and model; and estimated cost per hour and over the selected range.

All ports are published on loopback only, and Grafana lets anyone who reaches it in as an administrator, so the stack is for local development. If a port is taken, override it, for example `make observability PROMETHEUS_PORT=19090`; the others are `GRAFANA_PORT`, `JAEGER_UI_PORT`, and `OTLP_HTTP_PORT` (then use that port in `OTEL_EXPORTER_OTLP_ENDPOINT`). `make observability-stop` stops the stack and deletes its data. CI does not use it.

Prometheus scrapes `host.docker.internal:9464`, the default `GATEWAY_METRICS_ADDR`. With Docker Desktop or Colima on macOS, that reaches the gateway's loopback listener. On Linux, `host.docker.internal` is the Docker bridge address, so the gateway must listen there instead, for example `GATEWAY_METRICS_ADDR=172.17.0.1:9464`; this has not been tested. For another metrics port, edit [`prometheus.yml`](deploy/observability/prometheus.yml).

The gateway's provider endpoints are fixed, so dashboard data comes from real traffic. Requests with an unknown model or a wrong gateway key fill the request panels without calling a provider; provider, token, and cost panels need completions from the configured providers.

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
* `/metrics` has no authentication; it reveals request counts per configured model and status and provider health, never keys, clients, or content. Keep `GATEWAY_METRICS_ADDR` on loopback or a network only Prometheus can reach.
* With tracing enabled, every request, including rejected ones, produces spans, and a client's `traceparent` decides whether its requests are sampled. Clients cannot see trace data, but anyone who can reach the gateway can add to the collector's load.
* Every rejected key logs one warning, and there is no per-IP limit on unauthenticated requests; anyone who can reach the port can fill the logs. Another reason to keep the gateway behind a proxy.
* A request body must arrive within 30 seconds (otherwise `408`), and idle keep-alive connections close after 2 minutes. Both are fixed.
* Provider endpoints are fixed to the production APIs, so running the gateway needs real API keys and may incur charges. It cannot be pointed at a local fake provider; the automated tests exercise the full request path against fake upstreams instead.
* On reasoning models, thinking counts toward `max_tokens`, so a small limit can end with `length` and little text.
* Usage records cover requests that pass body validation, including unknown models and upstream failures. Authentication, gateway rate-limit, malformed-body, and body-size rejections are excluded. A disconnected client is recorded as status `499` with `client_closed`; this status is never sent as a response.
* Accounting is asynchronous: a full queue drops new records, a failed batch is retried once and then logged and discarded, and a shutdown deadline can lose pending records. Database failure after startup does not fail otherwise successful requests. Records contain metadata, tokens, and estimated costs, never prompt or completion content.
* Usage records are never deleted automatically; the table grows with traffic. Delete old rows on your own schedule; [docs/usage.md](docs/usage.md#retention) shows how.
* Estimated costs cover only provider-reported usage, including a successful generation whose client disconnected before receiving it. Failed retries and a failed primary before fallback may incur charges without reporting usage. Unknown usage and cost are stored as `NULL`, never as fabricated zeros.

### Querying usage

Usage is queried directly in PostgreSQL. [docs/usage.md](docs/usage.md) provides SQL reports per client, answering model, and UTC day, plus lookups using `X-Request-ID`. Reports show known token/cost subtotals alongside counts of unknown values; cache reads and writes are already included in input tokens. An admin HTTP API remains future roadmap work.

## Development

The Makefile defines the verification commands used locally and in GitHub Actions. `make check` runs `fmt` (gofmt), `vet` (including compilation of the smoke tests), `test` (`go test -race -timeout 2m ./...`), `build`, and `vuln` (govulncheck).

### PostgreSQL integration tests

To run the complete suite including the database integration tests, start the disposable PostgreSQL 18 container with Docker:

```bash
make db
export GATEWAY_TEST_DATABASE_URL='postgres://gateway:gateway@127.0.0.1:55432/gateway?sslmode=disable'
make check
```

`make db` prints the connection setting above. Its port defaults to `55432`; use `make db DB_PORT=<port>` and the printed URL if that port is occupied. The credentials are for this disposable local database. `GATEWAY_TEST_DATABASE_URL` is a test setting, not a gateway startup setting. Each integration test creates and drops its own database, so an alternative test server must grant the test user `CREATEDB`.

Run `make test` to run just the tests. Without `GATEWAY_TEST_DATABASE_URL`, database integration tests skip locally; the remaining tests still run. CI supplies a PostgreSQL service and the variable, and fails if it is missing or the database is unreachable.

When finished with the development database:

```bash
make db-stop
unset GATEWAY_TEST_DATABASE_URL
```

Stopping the container deletes its data. Tests use fake provider servers and the recorder tests use a fake store; they require no provider API keys or paid API calls. Initial setup may download Go dependencies and the Docker image, and `make vuln` needs access to the Go vulnerability database.

### Smoke test against the real APIs

The automated tests never call a real provider. To check the adapters against the live APIs, run:

```bash
read -s OPENAI_API_KEY; export OPENAI_API_KEY        # typed silently, kept out of shell history
read -s ANTHROPIC_API_KEY; export ANTHROPIC_API_KEY
export OPENAI_MODEL=gpt-4o ANTHROPIC_MODEL=claude-opus-5-5
make smoke
```

**This makes real, billed API calls** (a few short completions per provider, typically well under one cent). Configure one or both providers. For each model it checks a normal completion and a `length` stop, both with token usage. It also checks that unknown and missing gateway keys get `401`, and that a client over its limit gets `429` with `Retry-After`. It uses fresh random gateway keys and ignores other `GATEWAY_*` settings in the environment. It prints the model each provider reported and fails on any mismatch. It never runs in CI.

The smoke harness injects a fake recorder and an empty pricing table, so it needs no database or pricing file and does not persist usage.

## AI-Assisted Development

The project also serves as an experiment in AI-assisted software engineering.

AI coding agents are used to accelerate implementation and code review, while architecture, engineering decisions, testing strategy, security, reliability, and final code ownership remain human-controlled.
