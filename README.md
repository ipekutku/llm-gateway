# llm-gateway

[![CI](https://github.com/ipekutku/llm-gateway/actions/workflows/ci.yml/badge.svg?branch=main)](https://github.com/ipekutku/llm-gateway/actions/workflows/ci.yml?query=branch%3Amain)
[![Go version](https://img.shields.io/github/go-mod/go-version/ipekutku/llm-gateway?logo=go)](go.mod)
[![Latest version tag](https://img.shields.io/github/v/tag/ipekutku/llm-gateway?sort=semver&filter=v*&label=version)](https://github.com/ipekutku/llm-gateway/tags)
[![License](https://img.shields.io/github/license/ipekutku/llm-gateway)](LICENSE)

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

v0.1 through v0.9 are feature-complete; v1.0 (AWS deployment with Terraform) is in progress, with the [state and image repository bootstrap](docs/aws.md), images published from CI, and an [on-demand EKS cluster](docs/aws.md#platform-sessions) in place. Each milestone leaves the gateway runnable and tested. Scope and exit criteria live in [docs/ROADMAP.md](docs/ROADMAP.md), and design decisions in [docs/architecture.md](docs/architecture.md).

```text
v0.1  Provider abstraction + routing          ✅
v0.2  Timeouts + retries                      ✅
v0.3  Provider fallback + circuit breakers    ✅
v0.4  Authentication + rate limiting          ✅
v0.5  Usage + cost tracking + PostgreSQL      ✅
v0.6  Prometheus + OpenTelemetry              ✅
v0.7  Redis + multi-instance behavior        ✅
v0.8  Load testing + performance work          ✅
v0.9  Docker + Kubernetes + Helm               ✅
v1.0  AWS deployment with Terraform
```

### v0.9 — Containers and Kubernetes ✅

Implemented (see [Container image](#container-image), [Kubernetes with Helm](#kubernetes-with-helm), and [Local Kubernetes with kind](#local-kubernetes-with-kind)):

* `/livez` and `/readyz` on the API port, outside authentication and telemetry; readiness fails when shutdown begins and never because of PostgreSQL, Redis, or a provider
* a configurable shutdown: `GATEWAY_SHUTDOWN_DELAY` keeps serving with readiness failing, then `GATEWAY_SHUTDOWN_TIMEOUT` drains the listener
* a distroless, non-root image for amd64 and arm64, built in CI
* a Helm chart: Secret references only, a migration hook Job, a restricted security context, a PodDisruptionBudget, optional CPU autoscaling, and native sidecars
* a local kind cluster with disposable stores and the fake provider as a sidecar, installed and checked in CI, and a scripted demonstration of autoscaling, shared limits across pods, `SIGTERM`, a rolling restart under load, and a database outage (`make kind-demo`)

### v0.8 — Performance Engineering ✅

Implemented (see [Performance](#performance)):

* Go benchmarks for authentication, routing, rate limiting, cost estimation, and the full handler with metrics and tracing (`make bench`); CI runs each once so they keep working
* provider base URLs (`OPENAI_BASE_URL`, `ANTHROPIC_BASE_URL`), with plain HTTP allowed only on loopback, and a local fake provider (`cmd/fakeprovider`) for free, repeatable load
* opt-in pprof on the metrics listener (`GATEWAY_PPROF`), never on the API port
* reproducible load scenarios with Vegeta (`make loadtest`) that compare the gateway against the fake provider directly, with CPU, memory, accounting loss, and profiles
* measurements of Redis costs, connection reuse over HTTP/1.1 and HTTP/2, recorder limits, and binary size
* an upstream idle pool sized for concurrent requests, which removed the connection churn that made the high-concurrency runs fail

### v0.7 — Redis and Distributed Rate Limits ✅

Implemented (see [Shared rate limits with Redis](#shared-rate-limits-with-redis)):

* optional Redis (`GATEWAY_REDIS_URL`) so that gateway instances share each client's request rate and concurrent requests, with the same limits and `429` responses as before
* atomic checks in Redis using Redis time, so instances with different clocks agree, and concurrency slots that expire, so a crashed instance cannot hold them for good
* when Redis fails or is slow, each instance falls back to its own limits with one warning, probes Redis once a second, and resumes shared limits when it answers; startup does not require Redis
* metrics and dashboard panels for the rate-limit mode, Redis call latency, and Redis errors
* circuit breakers stay per instance, and Redis is used for nothing else; the reasons are in the [architecture](docs/architecture.md#shared-limits-in-redis)
* tests with two complete gateway instances against one Redis, including an outage and recovery through a proxy that cuts the connection

### v0.6 — Observability ✅

Implemented (see [Logs, metrics, and traces](#logs-metrics-and-traces)):

* structured logs as text or JSON; every line logged while handling a request carries its request ID and, once authenticated, its client ID, including retry, fallback, and circuit breaker lines
* one outcome line per validated request with status, provider, latency, retry count, and whether a fallback ran
* Prometheus metrics on a separate, unauthenticated listener: request counts by model, status, and error code, and request latency by model, with model labels limited to the configured models
* provider metrics: every upstream attempt by outcome, its latency, errors by upstream status, retries, fallbacks, and each circuit breaker's state
* token and estimated cost metrics per provider and model, matching the usage records
* OpenTelemetry traces exported over OTLP/HTTP when an endpoint is configured: one span per request, routing step, provider, and upstream attempt, with retries and fallbacks as events, and the trace ID in every log line
* traces of usage database writes, linked to the requests they record, and of migrations
* a local Prometheus, Grafana, and Jaeger stack (`make observability`) with a provisioned dashboard
* regression tests across logs, metrics, and exported spans for sensitive-data exclusion, including upstream protocol failures; database and trace-export diagnostics omit raw server messages

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

## Performance

Local synthetic measurements on an Apple M1 (Go 1.26.9), with the load generator, gateway, and fake provider on the same machine:

* **977 successful requests/sec with 500 concurrent clients** for 30 seconds against a fake provider that waits 500 ms, with no errors, a **2.8 ms p95 gateway-minus-baseline difference**, and **88.5 MiB sampled peak RSS**
* **about 21,900 requests/sec with 500 concurrent clients** and no errors for 30 seconds against a fake provider that answers immediately, limited by socket I/O on one machine
* **16,000 requests/sec at a fixed arrival rate** with no errors for 5 seconds; above about 8,000 requests/sec, the usage writer starts dropping records

These are single-run loopback comparisons, not capacity or isolated-overhead guarantees. Before the [upstream idle pool](docs/performance.md#upstream-idle-pool) was enlarged, the same 30-second runs failed with 87–98% errors from upstream connection churn; those results are documented too, not hidden.

`make loadtest` reproduces direct-fake/gateway comparisons, a fixed-arrival check, metrics, and CPU/heap profiles without paid calls. It needs a migrated disposable PostgreSQL database and is not run in CI. See [performance measurements](docs/performance.md#running-local-load-scenarios) for setup, raw results, caveats, and component benchmarks.

The [follow-up measurements](docs/performance.md#redis-and-connection-management-follow-up) compare optional Redis, HTTP/1.1 versus TLS/HTTP/2 reuse, recorder loss, and binary size. `LOADTEST_REDIS_URL` explicitly enables a disposable local Redis comparison; `make performance-probes` and `make binary-size` are opt-in developer tools, not CI load/timing gates. The one production change they justified, the upstream idle pool, has [before/after measurements](docs/performance.md#upstream-idle-pool).

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

The gateway listens on `127.0.0.1:8080`, serves Prometheus metrics on `http://127.0.0.1:9464/metrics`, and logs the configured models and the number of clients. Stop it with `Ctrl+C` or `SIGTERM`; in-flight requests get up to `GATEWAY_SHUTDOWN_TIMEOUT` (default 5 seconds) to finish, then pending handlers and usage writes share an additional 5-second drain budget. See [Shutdown](#shutdown).

Send a request to either model:

```bash
curl -s http://127.0.0.1:18080/v1/chat/completions \
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

curl -s http://127.0.0.1:18080/v1/chat/completions \
  -H "Authorization: Bearer $GATEWAY_KEY" \
  -H 'Content-Type: application/json' \
  -d '{
    "model": "claude-opus-5-5",
    "messages": [{"role": "user", "content": "Explain TCP."}],
    "max_tokens": 200
  }'
```

The `model` field must match a configured model exactly; the request is routed to that provider and the model name is forwarded unchanged.

### Container image

`make image` builds two images for this machine's platform and loads them into Docker: the gateway, `llm-gateway:dev`, and the local fake provider, `llm-gateway-fakeprovider:dev`. `make image-check` builds both for `linux/amd64` and `linux/arm64` without loading or pushing them, as CI does; locally it needs Docker's containerd image store or a buildx `docker-container` builder. `make image-scan` scans each image for each platform with a digest-pinned [Trivy](https://trivy.dev) and fails on HIGH or CRITICAL vulnerabilities that have a fix, and on secrets; it needs only Docker.

CI publishes both images for both platforms to the GitHub Container Registry after every merge to `main` once all other CI jobs have passed, tagged with the full commit SHA: `ghcr.io/ipekutku/llm-gateway:<sha>` and `ghcr.io/ipekutku/llm-gateway-fakeprovider:<sha>`. There is no `latest` tag. Pull requests are scanned, and `main` is scanned again just before publishing. CI pushes with its own short-lived `GITHUB_TOKEN`; GitHub holds no AWS or registry secret. Manual arm64 publication to private ECR repositories is [verified](docs/aws.md#bootstrap-verification--2026-10-11) as well.

The images are based on distroless `static`: a static binary, CA certificates, and time zone data, with no shell or package manager. They run as the numeric user `65532`, need no writable root filesystem or Linux capabilities, and set `GATEWAY_ADDR=:8080`, so the API port can be published. The metrics listener stays on `127.0.0.1:9464` inside the container unless `GATEWAY_METRICS_ADDR` says otherwise. There is no Docker `HEALTHCHECK`; use the [health probes](#supported-api). The gateway image is about 32 MB unpacked and 9.5 MB compressed, most of it the 26 MB gateway binary.

The same image applies migrations (`migrate`) and runs the gateway. Settings come from environment variables and files mounted read-only, which must be readable by user `65532`. This example uses the `make db` database, the synthetic load-test client and prices, and the fake provider instead of real providers:

```bash
make image db
PG_IP=$(docker inspect -f '{{range .NetworkSettings.Networks}}{{.IPAddress}}{{end}}' llm-gateway-postgres)
DB_URL="postgres://gateway:gateway@$PG_IP:5432/gateway?sslmode=disable"
docker run --rm -e GATEWAY_DATABASE_URL="$DB_URL" llm-gateway:dev migrate

# The gateway shares the fake provider's network namespace, so it reaches the
# fake over loopback, the only place plain HTTP providers are allowed. Port
# 18080 avoids a gateway running on the host.
docker run -d --name fakeprovider -p 127.0.0.1:18080:8080 llm-gateway-fakeprovider:dev
docker run -d --name gateway --network container:fakeprovider --read-only --cap-drop ALL \
  -v "$PWD/deploy/loadtest/clients.json:/config/clients.json:ro" \
  -v "$PWD/deploy/loadtest/prices.json:/config/prices.json:ro" \
  -e GATEWAY_CLIENTS_FILE=/config/clients.json -e GATEWAY_PRICING_FILE=/config/prices.json \
  -e GATEWAY_DATABASE_URL="$DB_URL" \
  -e OPENAI_MODEL=local-openai -e OPENAI_API_KEY=synthetic-openai-key -e OPENAI_BASE_URL=http://127.0.0.1:9000 \
  llm-gateway:dev

curl -s http://127.0.0.1:18080/readyz
curl -s http://127.0.0.1:18080/v1/chat/completions \
  -H 'Authorization: Bearer synthetic-loadtest-key' -H 'Content-Type: application/json' \
  -d @deploy/loadtest/openai.json

docker stop gateway fakeprovider && docker rm gateway fakeprovider
```

The request is stored as a synthetic usage row in the development database; `make db-stop` deletes it. For real providers, pass keys from your environment with `-e OPENAI_API_KEY` (no value), so they stay out of shell history; anyone who can run `docker inspect` on the host can still read them. The gateway still serves plain HTTP: publish its port only on loopback, or behind a proxy that terminates TLS.

### Kubernetes with Helm

The chart in [`deploy/helm/llm-gateway`](deploy/helm/llm-gateway) installs a Deployment, a ClusterIP Service with the API and metrics ports, a ServiceAccount without an API token, a ConfigMap holding the pricing file, and a Job that applies database migrations before each install and upgrade. It was tested with Helm 4.3.0. PostgreSQL and Redis are not part of it, and it never creates a Secret: create the Secrets first and name them in the values.

```bash
kubectl create namespace llm-gateway
kubectl -n llm-gateway create secret generic llm-gateway-database \
  --from-literal=url='postgres://gateway:...@db.example.internal:5432/gateway?sslmode=verify-full'
kubectl -n llm-gateway create secret generic llm-gateway-openai --from-literal=api-key="$OPENAI_API_KEY"
kubectl -n llm-gateway create secret generic llm-gateway-clients --from-file=clients.json
```

Then install:

```bash
helm install llm-gateway deploy/helm/llm-gateway -n llm-gateway \
  --set image.repository=registry.example.com/llm-gateway --set image.tag=<tag> \
  --set providers.openai.model=gpt-4o \
  --set providers.openai.apiKeySecret.name=llm-gateway-openai \
  --set clients.secretName=llm-gateway-clients \
  --set database.urlSecret.name=llm-gateway-database
```

Before Helm changes anything else, a hook Job runs `gateway migrate` with the same image and waits for it, so new pods find the schema they check at startup. A Job that fails, after one retry, fails the install or upgrade, leaves the running Deployment unchanged, and is kept for `kubectl logs job/<release>-llm-gateway-migrate` until the next attempt replaces it; a successful one is deleted. Repeated and concurrent runs are safe: each migration is applied once.

By default the Job uses the gateway's own database URL, which then needs the right to change the schema. To keep the gateway's role limited to reading the migration history and inserting usage records, give the Job a separate owner URL with `migrations.urlSecret.name`, and grant the gateway's role its rights in advance:

```sql
CREATE ROLE gateway_app LOGIN PASSWORD '...';
ALTER DEFAULT PRIVILEGES FOR ROLE gateway IN SCHEMA public GRANT SELECT, INSERT ON TABLES TO gateway_app;
```

Here `gateway` owns the schema and runs the migrations, and `gateway_app` is in `database.urlSecret`.

`sslmode=verify-full` checks the database's certificate against the image's trust store, which holds only public CAs. A database whose certificate comes from a private CA, such as Amazon RDS, needs that CA's bundle: put it in a ConfigMap, name it in `database.caBundle`, and point `sslrootcert` in both database URLs at the path where the chart mounts it, in the gateway pods and the migration Job alike:

```bash
curl -fsSLo global-bundle.pem https://truststore.pki.rds.amazonaws.com/global/global-bundle.pem
kubectl -n llm-gateway create configmap rds-ca --from-file=ca.pem=global-bundle.pem
# URL: postgres://...?sslmode=verify-full&sslrootcert=/etc/llm-gateway/database-ca/ca.pem
helm upgrade ... --set database.caBundle.configMapName=rds-ca
```

Without the bundle, or with the wrong one, the connection fails rather than falling back to an unverified one. The chart cannot see changes to the ConfigMap, so restart the Deployment after replacing it.

Migrations are forward-only, and a gateway refuses to start on a schema with migrations it does not know. After an upgrade that adds a migration, pods of the previous version keep serving until they are replaced, but any of them that restarts fails its schema check, and so do the pods of a `helm rollback` to that version: roll forward instead. Set `migrations.enabled=false` to migrate another way, such as with the [container image](#container-image).

[`values.yaml`](deploy/helm/llm-gateway/values.yaml) describes every value; the main ones:

| Value | Default | Description |
|---|---|---|
| `image.repository`, `image.tag` | `llm-gateway`, `dev` | Local defaults: build with `make image` and load into a local cluster, or set a registry and tag the cluster can pull, such as `ghcr.io/ipekutku/llm-gateway` and a commit SHA from `main` (see [Container image](#container-image)). [Manual private ECR publication](docs/aws.md#manually-verify-image-publication) is verified for arm64. |
| `providers.<openai\|anthropic>.model`, `.apiKeySecret` | none | Enable a provider with its model and the Secret (`name`, `key`, default `api-key`) holding its key. At least one is required. `.baseURL` and `.fallback` set `<PROVIDER>_BASE_URL` and `<PROVIDER>_FALLBACK`. |
| `clients.secretName`, `clients.key` | none, `clients.json` | Required. The Secret holding the [clients file](#clients). |
| `database.urlSecret` | none, key `url` | Required. The Secret holding `GATEWAY_DATABASE_URL`. |
| `database.caBundle` | none, key `ca.pem` | Optional. A ConfigMap (`configMapName`, `key`) with the CA that signed the database's certificate, mounted read-only at `/etc/llm-gateway/database-ca/ca.pem` in the gateway and migration pods; name that path in `sslrootcert` (see above). |
| `migrations.enabled` | `true` | Run `gateway migrate` as a pre-install and pre-upgrade hook Job. |
| `migrations.urlSecret` | none, key `url` | Optional. A Secret holding a database URL with the right to change the schema, for the Job only; empty uses `database.urlSecret`. |
| `migrations.backoffLimit`, `.activeDeadlineSeconds` | `1`, `240` | Retries after a failed run, and the Job's time limit including the image pull; keep it below Helm's `--timeout` (default 5 minutes). |
| `redis.urlSecret` | none, key `url` | Optional. The Secret holding `GATEWAY_REDIS_URL`; without it each pod limits on its own. |
| `prices` | `[]` | Price entries as in the pricing file, with prices as quoted strings. |
| `shutdown.delaySeconds`, `.timeoutSeconds`, `.terminationGracePeriodSeconds` | `5`, `25`, `45` | [Shutdown](#shutdown) settings. The grace period must be at least delay + timeout + 10 seconds; requests running longer than delay + timeout are canceled. |
| `env` | `[]` | Further environment variables, such as `GATEWAY_UPSTREAM_TIMEOUT`, retry and breaker settings, or `OTEL_EXPORTER_OTLP_ENDPOINT` for tracing. Use `valueFrom.secretKeyRef` for secrets. Variables the chart sets are rejected. |
| `logFormat` | `json` | `json` or `text`. |
| `replicaCount` | `2` | Pods in the Deployment when autoscaling is off. |
| `autoscaling.enabled`, `.minReplicas`, `.maxReplicas`, `.targetCPUUtilizationPercentage` | `false`, `2`, `6`, `70` | A HorizontalPodAutoscaler on the gateway container's average CPU use relative to `resources.requests.cpu`; sidecars are not counted. Needs the cluster's metrics API (metrics-server) and Kubernetes 1.30 or later. |
| `podDisruptionBudget.maxUnavailable` | `1` | Pods that voluntary disruptions, such as node drains, may evict at a time; a number or a percentage such as `"25%"`. |
| `resources` | requests `100m` CPU, `128Mi` memory | Container resources; no limits are set by default. |
| `sidecars` | `[]` | Containers that run next to the gateway, such as a database proxy, as native sidecars: they start first and stop only after the gateway has exited, so they serve through its shutdown. Each is a container spec with its own `securityContext` and `resources`. Needs Kubernetes 1.29 or later. |

The chart runs two pods by default and spreads them across nodes where it can (`ScheduleAnyway`, so a single-node cluster still runs both). A PodDisruptionBudget lets a drain evict only one at a time, so one keeps serving while the other is replaced; every evicted pod shuts down gracefully. With `autoscaling.enabled`, the HPA keeps between `minReplicas` and `maxReplicas` pods and the chart stops setting the Deployment's replica count, so upgrades leave the autoscaler's count alone. Switching autoscaling on in an upgrade briefly scales the Deployment to one pod, until the HPA raises it to `minReplicas` within seconds; do it outside busy periods. CPU is a loose signal for the gateway, whose requests mostly wait on providers: a pod can be saturated with in-flight requests while its CPU stays low. Scaling on requests in flight would need a custom metrics adapter, which the chart does not include.

Without Redis, each pod enforces every client's rate and concurrency limits on its own, so a release with N pods admits up to N times a client's limits, and the effective limit changes as the autoscaler adds or removes pods. Set `redis.urlSecret` to share the limits across all pods (see [Shared rate limits](#shared-rate-limits-with-redis)); the install notes warn when there is more than one pod without it.

The pods run as user `65532` with a read-only root filesystem, no privilege escalation, all capabilities dropped, and the `RuntimeDefault` seccomp profile, which meets the Kubernetes "restricted" Pod Security Standard. Liveness probes `/livez` and readiness `/readyz`; readiness does not depend on PostgreSQL, Redis, or the providers. A change to `prices` rolls the pods; a change to a referenced Secret does not, so restart them afterwards with `kubectl rollout restart deployment`. A JSON schema and template checks reject unknown values, mistyped prices, missing Secrets, and a grace period too short for the shutdown settings before anything is applied.

The gateway serves plain HTTP, and the Service is ClusterIP only, with no Ingress. Put TLS in front of the gateway before exposing it outside the cluster. Metrics listen on the pod network at port `9464` (Service port `service.metricsPort`) without authentication, so anything in the cluster that can reach the pods can read them; pprof stays off unless `GATEWAY_PPROF=true` is set in `env`, which would expose it the same way.

### Local Kubernetes with kind

[`deploy/kind`](deploy/kind) runs the chart in a local [kind](https://kind.sigs.k8s.io) cluster, with nothing but synthetic credentials. Each gateway pod gets the [fake provider](#container-image) as a sidecar on `127.0.0.1:9000`, and both providers point at it, so no request leaves the cluster. It needs Docker, Go, `kubectl`, and `helm`; kind itself runs pinned through `go run`.

```bash
make kind-up       # build both images, create the cluster, load the images, start metrics-server, PostgreSQL, and Redis
make kind-deploy   # create the synthetic Secrets and install the chart; the migration hook prepares the database
make kind-check    # one completion per provider through the Service, and the metrics port
make kind-down     # delete the cluster and everything in it
```

`make kind-demo` then demonstrates autoscaling, shared limits across pods, `SIGTERM` to one pod, a rolling restart under load, and a PostgreSQL outage, in about twelve minutes; results go to `deploy/kind/results/`. See [demonstrations](docs/architecture.md#demonstrations) for what each does and the recorded results.

All four setup targets can be run again: `kind-up` keeps an existing cluster and reloads the images, and `kind-deploy` upgrades the release. After changing Go code, run `make kind-up kind-deploy` and restart the pods (`kubectl --context kind-llm-gateway -n llm-gateway rollout restart deployment/llm-gateway`), since the image tag stays `dev`.

The cluster has one node running Kubernetes 1.37 (`kind-llm-gateway` context, `llm-gateway` namespace). The release uses [`deploy/kind/values.yaml`](deploy/kind/values.yaml): two to four pods under the autoscaler, shared limits in Redis, and synthetic prices. PostgreSQL and Redis have no volumes and a fixed password, so their data disappears with their pods; they are for local demonstrations only. To send requests yourself:

```bash
kubectl --context kind-llm-gateway -n llm-gateway port-forward service/llm-gateway 18080:8080
curl -s http://127.0.0.1:18080/v1/chat/completions \
  -H 'Authorization: Bearer synthetic-kind-demo-key' -H 'Content-Type: application/json' \
  -d '{"model": "local-openai", "messages": [{"role": "user", "content": "hello"}]}'
```

The `demo` client (`deploy/kind/clients.json`, key `synthetic-kind-demo-key`) may send 600 requests per minute, bursts of 20, and 10 at a time; `load` (`synthetic-kind-load-key`) is effectively unlimited, and `limited` (`synthetic-kind-limited-key`) gets bursts of 10, then one request per second. The keys are public: never reuse them, the values, or the stores outside this cluster. With Colima, give the VM at least 4 GiB of memory (`colima start --memory 4`); the default 2 GiB is enough for the cluster but leaves little room for building the images.

### AWS infrastructure

[`deploy/aws/bootstrap`](deploy/aws/bootstrap) provisions a private, versioned,
encrypted S3 state bucket and two private ECR repositories in `eu-north-1`, kept
between sessions. [`deploy/aws/platform`](deploy/aws/platform) creates a VPC and
an EKS 1.37 cluster with two nodes for a working session: `make aws-up` creates
it (about 13 minutes, about $0.30 per hour while it exists), `make aws-down`
destroys it, and `make aws-leftovers` fails while anything billable remains in
the Region. The [platform verification](docs/aws.md#platform-verification--2026-10-12)
covers a full create, inspect, and destroy cycle. `make terraform-check` validates
both modules with mocked plans, without contacting AWS; Terraform 1.16.5 is required. The [AWS runbook](docs/aws.md) covers account
setup and costs, the first local apply, migration into S3 with native locking,
and manually pushing the local images. [Live bootstrap verification](docs/aws.md#bootstrap-verification--2026-10-11)
confirmed a no-change plan, encrypted/versioned remote state, and both arm64 image
pushes with completed basic scans and no findings reported. CI publishes images
to GHCR rather than ECR, because the AWS project does not allow the GitHub OIDC
identity provider that keyless ECR pushes need (see the [runbook](docs/aws.md#ci-images-and-aws-credentials)).
AWS gateway deployment is not implemented yet.

### Configuration

| Variable | Description |
|---|---|
| `GATEWAY_CLIENTS_FILE` | Required. Path of the JSON file listing the gateway's clients (see below). |
| `GATEWAY_DATABASE_URL` | Required. PostgreSQL URL or connection string. Startup requires a reachable database with the current schema; `go run ./cmd/gateway migrate` applies pending migrations. Never log or commit this setting if it contains credentials. For any database not on the local host, add `sslmode=verify-full` (with `sslrootcert` if the server's CA is not in the system store): the default, `prefer`, uses TLS when offered but does not verify the server, so the password and records could be intercepted. |
| `GATEWAY_PRICING_FILE` | Required. Path of the JSON pricing file, read once at startup. Missing model prices produce unknown costs and a startup warning. |
| `OPENAI_MODEL`, `OPENAI_API_KEY` | Enable OpenAI for one model. Set both or neither. |
| `OPENAI_BASE_URL` | Optional provider origin with an optional path prefix, without `/v1/chat/completions`. Default `https://api.openai.com`. See [Provider destinations](#provider-destinations). |
| `ANTHROPIC_MODEL`, `ANTHROPIC_API_KEY` | Enable Anthropic for one model. Set both or neither. |
| `ANTHROPIC_BASE_URL` | Optional provider origin with an optional path prefix, without `/v1/messages`. Default `https://api.anthropic.com`. See [Provider destinations](#provider-destinations). |
| `GATEWAY_ADDR` | Listen address. Default `127.0.0.1:8080`. |
| `GATEWAY_METRICS_ADDR` | Listen address of the unauthenticated Prometheus endpoint, `/metrics`. Default `127.0.0.1:9464`. Must differ from `GATEWAY_ADDR`. See [Logs, metrics, and traces](#logs-metrics-and-traces). |
| `GATEWAY_PPROF` | Optional `true` or `false` (default). Enables `/debug/pprof/` on the metrics listener only. Unset or blank disables it; other values fail startup. See [Profiling](#profiling). |
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
| `GATEWAY_SHUTDOWN_DELAY` | After `SIGTERM` or `SIGINT`, how long the gateway keeps serving, with `/readyz` answering `503`, before it stops accepting connections. Gives a load balancer or Kubernetes time to stop sending requests. Default `0`. |
| `GATEWAY_SHUTDOWN_TIMEOUT` | After the delay, how long requests already running may take to finish before they are canceled. Default `5s`. See [Shutdown](#shutdown). |
| `GATEWAY_REDIS_URL` | Optional. A `redis://` or `rediss://` URL of a Redis that holds the clients' rate limits, so that several gateway instances share them. Unset means each instance limits on its own. See [Shared rate limits](#shared-rate-limits-with-redis). Treat it as a secret if it contains a password; for any Redis not on the local host, use `rediss://` (TLS). |

For example, to fall back from OpenAI to Anthropic:

```bash
export OPENAI_FALLBACK=anthropic
```

A request for `gpt-4o` that OpenAI cannot serve is then answered by `claude-opus-5-5`; the response's `model` field shows which model answered. While a provider is skipped and no fallback can answer, requests fail with `503 provider_unavailable`.

### Shutdown

On `SIGTERM` or `SIGINT`, `/readyz` answers `503` at once, and the gateway keeps serving everything else for `GATEWAY_SHUTDOWN_DELAY`, while asking each client to close its connection after its response (`Connection: close`), so clients reconnect elsewhere before the listener closes. It then stops accepting connections and lets running requests finish within `GATEWAY_SHUTDOWN_TIMEOUT`; requests still running after that are canceled, and their usage is recorded as `499 client_closed`. Pending usage records and spans are then written, within 5 seconds each. The whole shutdown takes at most the delay plus the timeout plus about 10 seconds, so a process manager's kill timeout, such as Kubernetes' `terminationGracePeriodSeconds`, should be longer than that.

The shutdown timeout is usually much shorter than `GATEWAY_UPSTREAM_TIMEOUT` (default `120s`), so a slow completion can be cut off by a shutdown even though it was still within its own time limit. A second signal during the delay does not shorten it.

### Provider destinations

Provider base URLs are operator-controlled settings, read once at startup, never chosen by a request. Unset or whitespace-only values keep the production defaults. Overrides require the corresponding model and API key. Use HTTPS for any remote destination; plain HTTP is accepted only for `localhost` (case-insensitive) or a literal loopback IP address, including `127.0.0.1` and `::1`. DNS aliases that happen to resolve to loopback are not accepted for HTTP. URLs must have a host and, if present, a port from 1–65535, with no credentials, query (even an empty `?`), or fragment (even an empty `#`). An optional path prefix is supported; the adapter appends its endpoint path.

For free local traffic, start the bundled [fake provider](docs/performance.md#local-fake-provider) in a separate terminal:

```bash
go run ./cmd/fakeprovider -delay=50ms
```

Then point the gateway at it:

```bash
export OPENAI_MODEL=local-model
export OPENAI_API_KEY=synthetic-local-key
export OPENAI_BASE_URL=http://127.0.0.1:9000
go run ./cmd/gateway
```

The fake serves both provider formats with fixed text and synthetic usage, never making real API calls. The gateway still needs its clients and pricing files and a migrated PostgreSQL database. Anthropic works the same way with `ANTHROPIC_BASE_URL` and `POST /v1/messages`. Use synthetic provider keys for fakes. An override receives the configured provider key and prompts, so trust the destination before pointing real credentials at it. Redirects remain disabled, and URLs are never included in configuration errors or startup logs.

### Shared rate limits with Redis

Each gateway instance enforces the clients' limits on its own, so N instances allow a client up to N times its limits. With `GATEWAY_REDIS_URL` set, the instances share each client's request rate and concurrent requests through Redis instead:

```bash
make redis    # disposable local Redis on port 56379, without persistence
export GATEWAY_REDIS_URL='redis://127.0.0.1:56379/0'
go run ./cmd/gateway
```

The startup log then shows `rate_limits=shared`. All instances must use the same Redis and the same clients file: each sends its own limits with every check.

Redis only holds rate-limit state, and the gateway does not depend on it to serve requests:

* If Redis fails or takes longer than 250 ms to answer, the gateway logs one warning and switches to its own per-instance limits, as without Redis. It tries Redis again after a second, and logs once more when Redis answers and shared limits resume.
* If Redis does not answer at startup, the gateway logs a warning and starts with per-instance limits instead of failing.
* A request holds its concurrency slot in Redis for at most the body-read time, the upstream timeout, and 30 seconds more, so slots held by an instance that crashed free themselves.

Keep Redis reachable only by the gateways: anyone who can write to it can reset or use up any client's limits. Protect it with a password, and use `rediss://` for any Redis not on the local host, since `redis://` sends the password unencrypted. Keys are not separated per deployment, so gateways that must not share limits, such as staging and production, need different Redis servers or databases (the `/<db>` at the end of the URL).

Redis Cluster and Sentinel are not supported; use one Redis endpoint, such as a managed primary. URL options that change retries are overridden: the gateway turns off go-redis's command retries, because a retried rate-limit check could count a request twice.

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

An invalid provider base URL, or an override for a disabled provider, fails configuration validation before any database or upstream connection is opened. Errors name `OPENAI_BASE_URL` or `ANTHROPIC_BASE_URL`, never their values.

The model names above are examples. Any model the provider's API accepts can be configured. The automated tests run both adapters against fake servers built from the providers' documented API formats.

Verified against the live APIs with `make smoke` on 2026-10-11 for the v0.8 release, after the base URL settings and the larger upstream idle pool, and before that on 2026-10-10 during the v0.6 closeout. The smoke harness ignores `OPENAI_BASE_URL` and `ANTHROPIC_BASE_URL`, so it always uses the default production origins through the gateway's real upstream client. Both times, both models passed normal completion and forced `length` tests with token usage; gateway authentication (unknown and missing keys) and rate limiting also passed:

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

The API listener also serves two health probes, without a gateway key:

| Probe | Response |
|---|---|
| `GET /livez` | `200 ok` while the process serves HTTP |
| `GET /readyz` | `200 ok` after startup, `503 shutting down` once shutdown begins |

Readiness does not depend on PostgreSQL, Redis, or the providers: the gateway keeps answering while they are unavailable, and their state is in the logs and metrics. Probes return no `X-Request-ID` and produce no logs, metrics, spans, or usage records. See [health probes](docs/architecture.md#health-probes) for the reasons.

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
| `gateway_ratelimit_store_up` | none | With `GATEWAY_REDIS_URL` only: `1` while rate limits are shared through Redis, `0` while this instance uses its own limits because Redis failed. |
| `gateway_ratelimit_store_call_duration_seconds` | `operation` | With Redis only: histogram of rate-limit calls to Redis, from 0.5 ms to 250 ms; `operation` is `acquire` (the admission check) or `release` (freeing the request's slot). |
| `gateway_ratelimit_store_errors_total` | `operation` | With Redis only: failed or timed-out calls to Redis. |

`model` is one of the configured models or `unknown`, so model names sent by clients cannot create new series; requests rejected before their body was read are also `unknown`. Token and cost metrics use the configured model of the provider that answered, the name in the pricing file, and match the usage records: a request with unknown cost adds tokens but no cost. The metric cost is a floating-point estimate that resets on restart; the exact picodollar values are in PostgreSQL. Client IDs are never labels; per-client usage is in PostgreSQL. Go runtime (`go_*`) and process (`process_*`) metrics are included. A minimal Prometheus scrape configuration:

```yaml
scrape_configs:
  - job_name: llm-gateway
    static_configs:
      - targets: ["127.0.0.1:9464"]
```

#### Profiling

For local performance work, enable Go's profiling handlers before starting the gateway:

```bash
export GATEWAY_PPROF=true
go run ./cmd/gateway
```

In another terminal, collect a CPU profile while sending traffic (use the [local fake provider](docs/performance.md#local-fake-provider) to avoid paid calls), or inspect heap allocations and goroutines:

```bash
go tool pprof 'http://127.0.0.1:9464/debug/pprof/profile?seconds=10'
go tool pprof 'http://127.0.0.1:9464/debug/pprof/heap'
curl -s 'http://127.0.0.1:9464/debug/pprof/goroutine?debug=1'
```

Use `top` in the pprof prompt, or add `-top` to print a report without entering it. Replace the port if `GATEWAY_METRICS_ADDR` differs. `GET /debug/pprof/` lists the available profiles; when disabled, these paths return `404`. They are never mounted on the API listener. `GATEWAY_PPROF=false` (or unsetting it) disables them after a restart.

These endpoints have **no authentication**. Keep the metrics listener on loopback when profiling; do not expose it publicly or assume that a network suitable for Prometheus is suitable for profiling. CPU and heap profiles describe code locations and allocation sites rather than dump prompt/completion bodies, but diagnostics also expose stack traces, file paths, symbols, and process arguments (`cmdline`). Treat captured profiles as sensitive. Anyone with access can start profiling or trigger GC, affecting the process's performance. Profiling is opt-in diagnostics, not part of the content-exclusion guarantee for normal logs, metrics, and exported spans.

No block or mutex sampling is enabled automatically; those profiles can be empty. Timed profile handlers extend their response write deadlines without changing `/metrics` timeouts, and the existing bounded shutdown still applies. See [profiling notes](docs/performance.md#profiling-the-gateway) for measurement limits.

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

A client may send a W3C `traceparent` header; the gateway's spans then join the client's trace, and the client's sampling decision applies. The gateway never sends trace headers to OpenAI or Anthropic. Spans are exported in batches in the background; failed exports are logged as `tracing error` warnings and never affect requests. Those warnings report a safe failure category, omitting exporter URLs and collector responses; use the collector's own diagnostics for details. Shutdown flushes pending spans last, after the usage records are written, within a 5-second budget of its own. Only OTLP over HTTP is supported: an endpoint that is not an `http` or `https` URL, or `OTEL_EXPORTER_OTLP_PROTOCOL` other than `http/protobuf`, fails startup. Use `https` for a collector on another host, and put collector credentials in `OTEL_EXPORTER_OTLP_HEADERS`, not in the URL.

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

The dashboard shows request volume, error rate, and p50/p95/p99 latency; errors by status and code; upstream attempts by outcome, provider latency, provider error rate, errors by upstream status, retries, fallbacks, and each circuit's state; tokens by type and model; estimated cost per hour and over the selected range; and, with Redis, each instance's rate-limit mode (shared or local), Redis call latency, and Redis call errors.

All ports are published on loopback only, and Grafana lets anyone who reaches it in as an administrator, so the stack is for local development. If a port is taken, override it, for example `make observability PROMETHEUS_PORT=19090`; the others are `GRAFANA_PORT`, `JAEGER_UI_PORT`, and `OTLP_HTTP_PORT` (then use that port in `OTEL_EXPORTER_OTLP_ENDPOINT`). `make observability-stop` stops the stack and deletes its data. CI does not use it.

Prometheus scrapes `host.docker.internal:9464`, the default `GATEWAY_METRICS_ADDR`. With Docker Desktop or Colima on macOS, that reaches the gateway's loopback listener. On Linux, `host.docker.internal` is the Docker bridge address, so the gateway must listen there instead, for example `GATEWAY_METRICS_ADDR=172.17.0.1:9464`; this has not been tested. For another metrics port, edit [`prometheus.yml`](deploy/observability/prometheus.yml).

Requests with an unknown model or a wrong gateway key fill the request panels without calling a provider; provider, token, and cost panels need completions from the configured providers. Provider destinations default to the production APIs; [base URL overrides](#provider-destinations) allow local fake traffic instead. Cost panels also need matching prices; fake usage and estimated costs do not represent real charges. The rate-limit store panels need `GATEWAY_REDIS_URL` (for example `make redis`) and requests from an authenticated client, since the limits are checked after authentication.

### Limitations

* One model per provider, matched by exact name; no aliases or wildcards.
* Text only: no streaming, tool calls, images, or multiple choices.
* Retries can occasionally produce a duplicate, separately billed generation: a `502` or `504` from a provider's proxy may arrive after the model already answered. Other retried failures (`429`, `503`, `529`, connection failures) happen before the model runs. See [retry safety](docs/architecture.md#what-is-retried).
* TLS handshake failures are not retried.
* With a fallback configured, a request can be answered by a different model than the one requested. A primary that times out is canceled, but may still bill for the partial generation.
* Fallback works only between the two configured models; there are no logical model names yet.
* Circuit breaker state is kept per gateway process; several instances do not share it, even with Redis. See [architecture](docs/architecture.md#shared-limits-in-redis) for why.
* A request whose upstream timeout expires may still be billed by the provider for the work done before it was canceled.
* The gateway serves plain HTTP and listens on loopback by default. Gateway keys would cross the network unencrypted, so expose it beyond the host only behind a proxy that terminates TLS.
* Without `GATEWAY_REDIS_URL`, and while Redis is unavailable, rate limits and concurrency counts are kept per gateway process: N instances allow a client up to N times its limits. Each rate-limit check with Redis adds one Redis round trip to a request, and releasing its slot another.
* Clients are read from a file at startup; changing them requires a restart. Each client has one key, so rotating a key briefly means replacing it.
* `/metrics` has no authentication; it reveals request counts per configured model and status and provider health, never keys, clients, or content. Keep `GATEWAY_METRICS_ADDR` on loopback or a network only Prometheus can reach.
* With `GATEWAY_PPROF=true`, the same metrics listener exposes unauthenticated profiling and process diagnostics. Keep it on loopback for profiling; these outputs reveal internal details and are not covered by normal telemetry's content-exclusion guarantee.
* With tracing enabled, every request except a health probe, including rejected ones, produces spans, and a client's `traceparent` decides whether its requests are sampled. Clients cannot see trace data, but anyone who can reach the gateway can add to the collector's load.
* Every rejected key logs one warning, and there is no per-IP limit on unauthenticated requests; anyone who can reach the port can fill the logs. Another reason to keep the gateway behind a proxy.
* A request body must arrive within 30 seconds (otherwise `408`), and idle keep-alive connections close after 2 minutes. Both are fixed.
* Up to 512 idle upstream connections are kept per provider origin, for 90 seconds. Over HTTP/1.1, more than 512 concurrent requests to one origin close their extra connections after use, and later requests connect again. HTTP/2 origins share connections and are not affected. Both values are fixed.
* Provider endpoints default to the production APIs, where requests may incur charges. For local fake providers, use [base URL overrides](#provider-destinations) with synthetic keys. Remote overrides require HTTPS and receive the configured provider key and prompts; only use trusted destinations.
* The gateway does not follow provider HTTP redirects, to keep keys and prompts at the configured destination. A redirect is an upstream failure (`502 upstream_error` unless a configured fallback answers).
* On reasoning models, thinking counts toward `max_tokens`, so a small limit can end with `length` and little text.
* Usage records cover requests that pass body validation, including unknown models and upstream failures. Authentication, gateway rate-limit, malformed-body, and body-size rejections are excluded. A disconnected client is recorded as status `499` with `client_closed`; this status is never sent as a response.
* Accounting is asynchronous: a full queue drops new records, a failed batch is retried once and then logged and discarded, and a shutdown deadline can lose pending records. Database failure after startup does not fail otherwise successful requests. Records contain metadata, tokens, and estimated costs, never prompt or completion content.
* Usage records lost to a database outage are logged with their count (`usage records not persisted`), but no metric counts them.
* Usage records are never deleted automatically; the table grows with traffic. Delete old rows on your own schedule; [docs/usage.md](docs/usage.md#retention) shows how.
* Estimated costs cover only provider-reported usage, including a successful generation whose client disconnected before receiving it. Failed retries and a failed primary before fallback may incur charges without reporting usage. Unknown usage and cost are stored as `NULL`, never as fabricated zeros.

### Querying usage

Usage is queried directly in PostgreSQL. [docs/usage.md](docs/usage.md) provides SQL reports per client, answering model, and UTC day, plus lookups using `X-Request-ID`. Reports show known token/cost subtotals alongside counts of unknown values; cache reads and writes are already included in input tokens. An admin HTTP API remains future roadmap work.

## Development

The Makefile defines the verification commands used locally and in GitHub Actions. `make check` runs `fmt` (gofmt), `vet` (including compilation of the smoke and optional performance probes), `test` (`go test -race -timeout 2m ./...`), `bench-check` (each benchmark once), `build`, and `vuln` (govulncheck). A separate CI job runs `make image-check image-scan`, which builds the [container images](#container-image) for amd64 and arm64 and scans them; it needs Docker, so it is not part of `make check`. On `main`, a final job scans the images again and publishes them with `make image-publish` once every other job has passed. Another job runs `make helm-lint`, which lints and renders the [Helm chart](#kubernetes-with-helm) with the values in its `ci/` directory and checks that invalid values are rejected; locally it needs `helm` on `PATH`. A fourth job runs `make kind-up kind-deploy kind-check`: it installs the chart in a [kind cluster](#local-kubernetes-with-kind) and sends one completion per provider through it. A fifth runs `make terraform-check` for the [AWS modules](#aws-infrastructure), without AWS credentials.

`make bench` measures request-path components with allocation counts. It uses in-process fixtures and synthetic credentials; only the shared rate-limiter benchmark contacts Redis, gated on `GATEWAY_TEST_REDIS_URL` as below. [Performance measurements](docs/performance.md) describes the fixtures, baseline results, and comparisons with pinned `benchstat`. CI runs `make bench-check` to exercise the fixtures, without timing thresholds.

The v0.6 release checks include `TestObservabilityExcludesSensitiveData`: the real handler, router, resilience layers, and adapters call fake upstreams while text/JSON logs, Prometheus output, and exported spans are checked together. It covers both providers, retries, fallback, admission errors, malformed upstream responses, and unexpected protocol field values. Separate tests cover provider redirect isolation, collector error responses, and database errors containing rejected values. No paid calls are needed. Database diagnostics retain operation context and SQLSTATE while omitting server messages and connection details.

Before tagging, run `make check` with the disposable database and Redis enabled as below and require green CI on the merged commit. This verifies behavior and known Go vulnerabilities, not throughput or latency overhead; `make loadtest` is a separate opt-in measurement target. The local stack was exercised with fake traffic during PR 8 (see [verification](docs/architecture.md#local-observability-stack)); the Linux Docker path and container-image vulnerability scanning are not covered by `make check`. The optional live `make smoke` passed for both providers on 2026-10-11 for the v0.8 release, and on 2026-10-10 during the v0.6 closeout. That harness does not enable metrics or tracing or persist usage.

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

### Redis integration tests

The shared rate-limit tests, including a complete request path with limits in Redis, need Redis. Start a disposable Redis 8 container without persistence:

```bash
make redis
export GATEWAY_TEST_REDIS_URL='redis://127.0.0.1:56379/0'
make check
```

`make redis` prints the setting above. Its port defaults to `56379`; use `make redis REDIS_PORT=<port>` and the printed URL if that port is occupied. Like `GATEWAY_TEST_DATABASE_URL`, it is a test setting; the gateway reads `GATEWAY_REDIS_URL`. Each test writes only keys under a random prefix of its own and deletes them when it ends, so it never clears other data. Without the variable, the Redis tests skip locally; CI supplies a Redis service and fails if it is missing.

When finished:

```bash
make redis-stop
unset GATEWAY_TEST_REDIS_URL
```

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
