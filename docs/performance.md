# Performance measurements

Milestone 8 starts with component benchmarks. These numbers describe in-process work per operation; HTTP load results, latency percentiles, memory under load, profiles, and connection-management measurements remain future work in this milestone. No production behavior has been optimized from these results.

## Local fake provider

`cmd/fakeprovider` is a standalone, standard-library-only development server for free, repeatable HTTP measurements. It never calls a real provider and needs no keys, PostgreSQL, Redis, or configuration files:

```bash
go run ./cmd/fakeprovider -delay=50ms
```

It listens on `127.0.0.1:9000` by default. `-addr` changes the listen address (`127.0.0.1:0` selects a free port, printed at startup); `-delay` is a non-negative Go duration and defaults to zero. There is no authentication or TLS: keep it on loopback and use synthetic keys, never real credentials or sensitive prompts.

| Endpoint | Fixed response |
|---|---|
| `POST /v1/chat/completions` | OpenAI shape: one assistant choice, `stop`, 12 prompt and 3 completion tokens, 15 total, zero cache reads. |
| `POST /v1/messages` | Anthropic shape: one assistant text block, `end_turn`, 12 input and 3 output tokens, zero cache reads or writes. |

Both return `synthetic completion` and echo the requested model. IDs and other metadata are fixed, so identical model requests produce identical response bytes. The delay starts after the body is decoded and is canceled when the request disconnects or the process stops. It is a minimum simulated wait, not a guarantee of end-to-end latency: JSON work, socket I/O, and scheduling add time.

With the fake running, configure the gateway in another terminal (keep the required gateway clients/pricing files and migrated PostgreSQL settings from the [README](../README.md#running-locally)):

```bash
export OPENAI_MODEL=local-openai OPENAI_API_KEY=synthetic-openai-key
export ANTHROPIC_MODEL=local-anthropic ANTHROPIC_API_KEY=synthetic-anthropic-key
export OPENAI_BASE_URL=http://127.0.0.1:9000
export ANTHROPIC_BASE_URL=http://127.0.0.1:9000
go run ./cmd/gateway
```

The gateway still authenticates and rate-limits its clients and records validated requests. Use `{"prices":[]}` for unknown costs, or explicitly synthetic prices for cost-path measurements; fixture token counts and estimated costs do not represent real usage or charges. Request the exact configured model name through the gateway. To check the fake directly, without the gateway:

```bash
curl -s http://127.0.0.1:9000/v1/chat/completions \
  -H 'Content-Type: application/json' \
  -d '{"model":"local-openai","messages":[{"role":"user","content":"synthetic prompt"}],"max_completion_tokens":32}'
```

Stop with `Ctrl+C` or `SIGTERM`. Pending delays are canceled, shutdown is bounded to five seconds, and the final line reports `connections=N`: cumulative accepted TCP connections, not requests or active connections. Keep-alive requests share a connection. Restart the fake for a separate connection-count comparison; there are no per-request logs, and output contains no keys, prompts, or completions.

This fixture is not a provider emulator: only `model` and `stream` affect handling. It requires one JSON object with a non-blank string model, rejects `stream: true`, malformed/trailing JSON and bodies above 1 MiB, and ignores other request fields. It does not tokenize messages, enforce generation limits, simulate cache behavior, refusals, rate limits, retries, streaming, or real provider authentication. Tests call it through both real adapters to check response translation and usage, including cancellation through actual HTTP connections. Delay boundaries are tested with Go's virtual-time `testing/synctest`, not wall-clock sleeps. Connection tests check reuse and new connections, and lifecycle tests verify the shutdown count and content exclusion.

The current fake uses loopback HTTP/1.1, not production HTTPS/HTTP/2. The fake and load generator can themselves become bottlenecks, especially when they share the gateway's machine. This PR supplies the fixture only; reproducible load scenarios, profiles, and end-to-end baseline results are still pending.

## Running benchmarks

```bash
make bench
```

All benchmarks report `ns/op`, `B/op`, and `allocs/op`. Setup is outside the measured loop. Authentication, routing, pricing, and handler benchmarks use `b.Loop`; local limiter benchmarks use `b.N` and `RunParallel` for comparison at the same client distribution. Outcomes are checked so an accidental rejection or failure cannot appear as an improvement.

Only `BenchmarkShared` contacts a service. It uses the same Redis gate and cleanup as integration tests:

```bash
make redis    # reuse the container if it is already running
export GATEWAY_TEST_REDIS_URL='redis://127.0.0.1:56379/0'
make bench
```

Without the setting, the Redis benchmark skips locally and fails in CI. Each run has a random prefix and deletes only its own keys. Redis admission and synchronous release both run inside the timed operation; the pool and Lua scripts are warmed before timing. A failed release also fails the benchmark. No provider credentials, PostgreSQL, or paid API calls are needed.

`make bench-check` runs each benchmark once (`-benchtime=1x`), including Redis when configured. It is part of `make check` and CI, with no timing thresholds. Those one-iteration timings include cold-path noise and must not be used as measurements.

## Fixture boundaries

- Authentication hashes a synthetic 46-character key and looks it up in a one-client map, with valid and unknown keys of the same length.
- Routing uses an immediate fake provider. `direct` has no fallback; `fallback_configured` measures a successful primary with its timeout context; `fallback_taken` returns an immediate primary 503 and a successful fallback. No retry or network time is included.
- Local limiter operations are successful `Acquire` plus release, with one hot client or 256 clients. Parallel workers cycle through the clients with different starting offsets. The real local clock and locks run; the test widens bucket tolerance before timing to cover the calibrated iteration count, even above the production maximum burst. This prevents fast loops from measuring rate rejections. It does not measure quota exhaustion or rejection behavior.
- `fallback_local` wraps the same local limiter, between Redis probes. Its test clock is fixed and its next probe is in the future, so it never contacts Redis. The difference includes the wrapper mutex and mode check in degraded local mode; shared mode takes another lock after Redis answers and is not measured here.
- Pricing includes the provider/model lookup and exact cost arithmetic for 1,000 input and 200 output tokens. The cached case has 600 cache-read and 100 cache-write tokens, already included in input.
- The handler decodes a 142-byte request with system and user messages and `max_tokens: 200`, then encodes a fixed text completion with reported usage and estimated cost. It includes auth, real local admission/release, routing, request IDs, and validation. Requests cycle over 256 clients at maximum legal limits so sustained admission does not throttle the loop. The recorder discards records because `httpapi.New` requires one; queueing and PostgreSQL writes are excluded. Retry, breaker, adapter serialization, socket I/O, and upstream generation are excluded.
- Handler metrics include real request, attempt, token, and cost collectors. Tracing always samples four spans per request (server, route, provider, attempt). A synchronous in-memory exporter counts spans and retains only the last, so memory stays bounded. This measures instrumentation and synchronous export, not the production batch processor, OTLP serialization, or collector I/O. The fixture checks that all four spans were exported.
- Handler timings include creating an `httptest` request/response recorder and buffering the response. Logs use `slog.DiscardHandler`, so log formatting and output are excluded. These are not wire latency or production allocation counts.

## Baseline environment

Measured on 2026-10-10 with this PR's benchmark fixtures over parent commit `f83edf8` (v0.7 baseline), without production Go changes:

| Setting | Value |
|---|---|
| CPU | Apple M1, 8 logical CPUs |
| RAM | 16 GiB |
| OS | macOS 26.6.2 (25G83), darwin/arm64 |
| Go | go1.26.9 |
| GOMAXPROCS | 8 (benchmark suffix `-8`) |
| Parallelism | Default `RunParallel`: 8 workers |
| Redis | Redis 8.10.2 in the existing Colima Docker container, host loopback port 56379 |
| Sampling | Default 1-second benchmark duration, 10 samples, race detector off |

The machine also had existing development containers running. The Redis result includes the host-to-VM hop and cannot be treated as same-process or production Redis latency. Laptop scheduling, background work, and thermal state can change results; rerun comparisons on the same machine and configuration.

## Component results

Command used, with `GATEWAY_TEST_REDIS_URL` set to the local container:

```bash
go test -run '^$' -bench . -benchmem -count=10 ./...
```

Medians of ten samples, summarized with the pinned `benchstat` below. Time ranges are benchstat's relative 95% confidence ranges; allocation columns are medians. Names omit the `Benchmark` prefix and `-8` suffix.

| Package | Benchmark | ns/op | Range | B/op | allocs/op |
|---|---|---:|---:|---:|---:|
| auth | Authenticate/valid | 93.61 | ±2% | 48 | 1 |
| auth | Authenticate/unknown | 80.53 | ±3% | 48 | 1 |
| routing | Route/direct | 28.57 | ±2% | 0 | 0 |
| routing | Route/fallback_configured | 290.3 | ±2% | 272 | 4 |
| routing | Route/fallback_taken | 544.2 | ±2% | 840 | 11 |
| ratelimit | Limiter/serial/hot_client/local | 107.3 | ±2% | 40 | 2 |
| ratelimit | Limiter/serial/hot_client/fallback_local | 121.1 | ±2% | 40 | 2 |
| ratelimit | Limiter/serial/many_clients/local | 116.6 | ±2% | 40 | 2 |
| ratelimit | Limiter/serial/many_clients/fallback_local | 124.1 | ±3% | 40 | 2 |
| ratelimit | Limiter/parallel/hot_client/local | 304.3 | ±4% | 40 | 2 |
| ratelimit | Limiter/parallel/hot_client/fallback_local | 352.3 | ±16% | 40 | 2 |
| ratelimit | Limiter/parallel/many_clients/local | 64.14 | ±9% | 40 | 2 |
| ratelimit | Limiter/parallel/many_clients/fallback_local | 159.7 | ±9% | 40 | 2 |
| ratelimit | Shared | 885,300 | ±6% | 1,904 | 43 |
| usage | PricingCost/uncached | 19.98 | ±3% | 0 | 0 |
| usage | PricingCost/cached | 19.68 | ±1% | 0 | 0 |
| httpapi | Handler/metrics_false/tracing_false | 8,173 | ±2% | 11,915 | 91 |
| httpapi | Handler/metrics_false/tracing_true | 13,330 | ±3% | 20,859 | 148 |
| httpapi | Handler/metrics_true/tracing_false | 8,908 | ±3% | 11,916 | 92 |
| httpapi | Handler/metrics_true/tracing_true | 14,210 | ±5% | 20,862 | 149 |

The fallback wrapper adds about 96 ns/op in the parallel, many-client fixture (159.7 versus 64.14 ns/op), with the same 40 bytes and two allocations. Its shared mutex limits how much spreading work across per-client locks helps. This difference includes the mode check and the fixed-clock fixture described above; it is not an isolated mutex latency or a measurement of healthy Redis mode. The hot-client wrapper result is more variable (±16%), since the local limiter already contends on one client's lock. Whole-request load measurements and profiles are still needed before deciding whether a change is justified.

The handler median is 8.17 µs with both telemetry components off, 8.91 µs with metrics, and 14.21 µs with metrics and sampled synchronous in-memory tracing. Redis admission plus release is about 0.885 ms through Colima. These results do not establish RPS capacity, p95 gateway overhead, memory under concurrent HTTP load, or live-provider performance.

## Comparing changes

Use at least ten samples per revision, with identical fixtures, Go version, `GOMAXPROCS`, Redis placement, and benchmark flags. Run measurements separately from builds, race tests, and other load. Preserve raw output outside the repository:

```bash
GOMAXPROCS=8 go test -run '^$' -bench . -benchmem -count=10 ./... > /tmp/gateway-before.txt
# Apply the change, then run the identical command:
GOMAXPROCS=8 go test -run '^$' -bench . -benchmem -count=10 ./... > /tmp/gateway-after.txt
go run golang.org/x/perf/cmd/benchstat@v0.0.0-20260908200009-22c9c6c9d4da \
  /tmp/gateway-before.txt /tmp/gateway-after.txt
```

Export `GATEWAY_TEST_REDIS_URL` for both runs to include Redis. The pinned [benchstat documentation](https://pkg.go.dev/golang.org/x/perf@v0.0.0-20260908200009-22c9c6c9d4da/cmd/benchstat) explains medians, confidence intervals, and significance. Its first use downloads a developer tool through `go run`; it does not change `go.mod` or the gateway binary. A single-file invocation summarizes a baseline. For contention checks, also compare `-cpu=1,8`, keeping both revisions' settings equal. Parallel `ns/op` is aggregate elapsed time per operation, not an individual request's latency percentile.
