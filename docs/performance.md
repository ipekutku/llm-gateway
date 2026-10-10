# Performance measurements

Milestone 8 measures component costs and the real HTTP request path. The local load scenarios below measure throughput, latency percentiles, CPU, memory, and accounting loss. Redis comparisons and connection-management measurements remain separate work. No production behavior has been optimized from these results.

## Running local load scenarios

`make loadtest` builds the real gateway and fake provider and installs [Vegeta v12.13.0](https://github.com/tsenart/vegeta/tree/v12.13.0) into a run-local directory. It adds nothing to `go.mod` or the production binary and does not install a global tool. Requires Go, Bash, awk, sed, curl 7.84+ (for transfer pacing), and a **disposable, already migrated PostgreSQL database**. Initial tool installation needs network access. This target is deliberately outside `make check` and CI.

```bash
make db     # reuse the container if already running
export GATEWAY_DATABASE_URL='postgres://gateway:gateway@127.0.0.1:55432/gateway?sslmode=disable'
go run ./cmd/gateway migrate
make loadtest
# Measure the other adapter separately, not concurrently:
LOADTEST_PROVIDER=anthropic make loadtest
```

The script never migrates, clears, or deletes the supplied database. It persists synthetic `loadtest` usage rows, potentially hundreds of thousands per run: do not use an operator/production database. Retention is your responsibility. For isolated comparisons, supply a fresh database of your own and migrate it first. Both models have explicitly synthetic prices to exercise known-cost accounting; neither the tokens nor prices represent real charges. The fixture key `synthetic-loadtest-key` is public and development-only, with only its SHA-256 hash in `clients.json`; never deploy these files as real client configuration.

All destinations are fixed to `127.0.0.1`. The gateway runs in a clean environment: both provider keys are synthetic, both base URLs point at the fake, local rate limits are used, tracing and fallback are off, and retry/breaker/recorder settings stay at production defaults. Metrics, JSON outcome logging, and PostgreSQL writes remain enabled. Inherited provider, Redis, OTLP, proxy, and resilience settings cannot alter gateway configuration. There are no paid calls and no need for `make smoke`.

The runner uses HTTP/1.1 with keep-alive and reads/discards response bodies. Each direct-fake/gateway pair gets a fresh gateway and fake:

| Scenario | Fake delay | Load model | Purpose |
|---|---:|---|---|
| A — zero delay | 0 | 500 fixed workers, unlimited rate | Observe throughput and CPU at one high-concurrency point; not a sweep proving maximum capacity. |
| B — simulated generation | 500 ms | 500 fixed workers, unlimited rate | Compare latency and observe memory while requests wait on the provider. |
| C — fixed arrivals | 0 | 70% of A's successful throughput, rounded to integer RPS | Cross-check latency without a fixed-worker closed loop. |

Warmup is 3 seconds at 100 RPS **on each path**, not at full concurrency. Each measured attack lasts 30 seconds plus response drain. C starts 500 workers and can grow to 10,000; that cap is a safety bound, not a concurrency target. The summary fails if either path's issued rate misses C's target by more than 5%, so the cap cannot silently turn it into a closed test. Closed tests can hide stalls through coordinated omission; fixed arrivals are a cross-check, not a guarantee that a same-machine generator never falls behind.

Keep these settings identical across comparisons:

| Setting | Default | Notes |
|---|---:|---|
| `LOADTEST_PROVIDER` | `openai` | `openai` or `anthropic`; one route per invocation. |
| `LOADTEST_SCENARIO` | `all` | `all`, `A`, `B`, or `C`; select a pair for targeted comparisons. |
| `LOADTEST_OPEN_RATE` | derived | Only for standalone C: required positive integer RPS. `all` always derives it from A. |
| `LOADTEST_SECONDS` | 30 | Integer, 1–3600; must exceed profile duration. |
| `LOADTEST_WARMUP_SECONDS` | 3 | Integer, 1–60. |
| `LOADTEST_PROFILE_SECONDS` | 10 | Integer, 1–60; CPU capture during each gateway attack. |
| `LOADTEST_WORKERS` | 500 | Integer, 1–10000; fixed concurrency for A/B. |
| `LOADTEST_CPUS` | 8 | `GOMAXPROCS` for gateway, fake, and Vegeta; set explicitly on another machine. |
| `LOADTEST_COOLDOWN_SECONDS` | 35 | Integer, 1–300; before each pair so macOS's default 30-second TIME_WAIT does not contaminate the next phase's initial socket budget. Increase for other OS settings. |
| `LOADTEST_FAKE_PORT` | 19000 | Unused loopback port. |
| `LOADTEST_GATEWAY_PORT` | 18080 | Unused loopback port. |
| `LOADTEST_METRICS_PORT` | 19464 | Unused, distinct loopback port. |

The fixture uses the maximum legal request rate and burst (1,000,000 each) and a concurrency limit of 10,000. These are not unlimited: longer/faster runs may hit the rate limit. Always inspect status codes. The script raises its own soft file-descriptor limit if needed but never changes system socket settings. HTTP/1.1 connection churn can exhaust ephemeral ports on macOS; errors, retries, and breaker activity then describe a laptop socket limit as well as gateway work. Do not silently change the transport or OS to improve the baseline.

For a fast wiring check only, not a performance result:

```bash
LOADTEST_SECONDS=3 LOADTEST_WARMUP_SECONDS=1 LOADTEST_PROFILE_SECONDS=1 \
  LOADTEST_WORKERS=20 make loadtest
```

### Artifacts and interpretation

Each invocation creates a private, gitignored directory under `deploy/loadtest/results/`, with commit/dirty tracked-file status, OS/Go/tool settings, exact targets, and matching binaries. Each scenario retains warmup and measured binary results, text/JSON Vegeta reports, gateway/fake logs, before/after metrics, approximately one-second raw samples, CPU/heap profiles and top-ten reports, and `summary.md`.

The metrics sampler and heap capture reuse one connection so ephemeral-port exhaustion does not stop later scrapes. CPU capture opens a separate connection at the start of load. Missing reports, metrics, or profiles fail the runner instead of fabricating zeros. HTTP errors get a prominent warning, not an error-free capacity claim. Cleanup stops only the runner's child processes, never existing gateways or containers. No database is automatically deleted.

Successful RPS is Vegeta's successful responses divided by attack plus drain time, not issued RPS. Percentiles include **all** responses: fast 503s can improve p95 while useful throughput collapses. Gateway-minus-baseline percentile differences compare independent distributions; they are not percentiles of paired per-request overhead. At closed concurrency the paths issue different rates, further limiting subtraction at saturation.

CPU is a process-counter delta in core-seconds over the gateway measurement and sampler completion (roughly one second after attack). RSS/goroutines are sampled peaks, not exact maxima or live heap. GC cycles/aggregate pauses and allocated bytes are counter deltas. Profiles cover the first configured seconds and add overhead; they exclude CPU used by Vegeta, the fake, and PostgreSQL. Queue-full drops and failed-batch counts include warmup and final shutdown. Zero failed batches does not prove complete accounting: queue drops are separate; inspect other persistence/shutdown warnings too.

The generator, fake, and gateway compete on one machine, with PostgreSQL potentially across a VM. Small payloads, one client, fixed models, loopback HTTP/1.1, file logging, and tracing/Redis off are not production traffic. Preserve raw artifacts for comparison but treat profiles and logs as internal diagnostics; never commit them. Keep fixtures/configuration unchanged during a run. Run separately from builds, race tests, and other load, and repeat measurements before drawing optimization conclusions.

## HTTP baseline results

Measured on 2026-10-11 (Europe/Istanbul), using gateway/fake binaries from `dd8f4fa` (merged profiling PR) plus this PR's development fixtures. No production Go code changed. These are single runs, not confidence intervals or production capacity guarantees.

| Setting | Value |
|---|---|
| Machine | Apple M1, 8 logical CPUs, 16 GiB RAM |
| OS / Go | macOS 26.6.2 (25G83), darwin/arm64; Go 1.26.9 |
| Process settings | `GOMAXPROCS=8` for all three processes; shell FD limit 1,048,575 |
| PostgreSQL | 18.6, existing Colima Docker VM, host loopback port 55432; task-owned fresh migrated database |
| Load tool | Vegeta module v12.13.0, same machine; HTTP/1.1 keep-alive, response capture disabled |
| Gateway | One process, one synthetic client, OpenAI route; both models configured, local limits, metrics/JSON logs/real recorder on, Redis/tracing/fallback off |
| Sampling | Default 30-second attacks, 3-second warmup per path, 10-second CPU capture; 35-second cooldown before each pair; race detector off |

Other development containers remained running. No system socket settings or upstream connection-pool settings were changed. The full run used:

```bash
# GATEWAY_DATABASE_URL pointed at the task-owned migrated database.
make loadtest
```

### Default 30-second runs

Successful throughput and all-response latency percentiles, rounded from raw Vegeta JSON. CPU/RSS/drop values describe only the gateway; dashes mean not measured for the direct fake, not zero.

| Scenario / path | Successful RPS | p50 ms | p95 ms | p99 ms | Errors | CPU core-s | Peak RSS MiB | Queue drops |
|---|---:|---:|---:|---:|---:|---:|---:|---:|
| A direct | 57,585.59 | 3.783 | 13.458 | 23.632 | 0% | — | — | — |
| A gateway | 708.49 | 8.707 | 38.470 | 65.988 | 97.9196% | 85.694 | 93.45 | 968,083 |
| B direct | 978.80 | 501.888 | 507.780 | 521.403 | 0% | — | — | — |
| B gateway | 752.40 | 8.494 | 502.848 | 511.400 | 92.4472% | 27.657 | 91.36 | 262,171 |
| C direct | 496.03 | 0.346 | 0.666 | 1.241 | 0% | — | — | — |
| C gateway | 496.01 | 0.633 | 1.214 | 1.996 | 0% | 8.162 | 31.20 | 0 |

A issued 1,022,807 gateway requests: 21,278 returned 200 and the rest 502/503. B issued 298,895: 22,575 returned 200 and the rest failed. Their high error rates make them **saturation findings, not usable capacity results**. B's p95 difference of **−4.931 ms is not negative gateway overhead**: fast failures changed its latency distribution. C issued 14,880 requests per path, all 200; its target was 496 RPS (70% of A's *successful* throughput, not 70% of proven maximum capacity), and both paths issued 496.03 RPS. Its p95 comparison difference was +0.548 ms. No run logged failed database batches; queue-full losses are nevertheless substantial in A/B.

Provider connection failures exhausted retries and opened the circuit in A/B. The laptop has an ephemeral-port range of 49152–65535 (16,384 ports) and a 30-second default TIME_WAIT lifetime; a read-only socket snapshot during A counted 15,785 TIME_WAIT connections. Together with the stock two-idle-connections-per-host transport, these observations point to HTTP/1.1 connection churn/socket pressure, not a demonstrated gateway CPU ceiling. That attribution is an inference to verify in the next connection-management PR, especially against TLS/HTTP/2; these loopback results do not establish real-provider performance. Recorder loss is also visible, and useful throughput must not be confused with complete accounting.

The first ten seconds of A's CPU profile contained 26.49 sampled CPU seconds over 10.14 seconds wall time. Top flat entries:

| Function | Flat CPU seconds | Sample share |
|---|---:|---:|
| `syscall.rawsyscalln` | 21.25 | 80.22% |
| `runtime.kevent` | 0.57 | 2.15% |
| `runtime.usleep` | 0.56 | 2.11% |
| `runtime.madvise` | 0.49 | 1.85% |

This profile includes the early transition to predominantly failing requests, with request logging still active. A flat syscall entry alone does not isolate connection setup versus file/socket writes, nor prove auth, pricing, or JSON needs optimization. CPU and heap captures parsed successfully for all three scenarios. Keep the matching binary when inspecting cumulative callers:

```bash
go tool pprof -top -cum <run-directory>/bin/gateway <run-directory>/A-zero-delay/cpu.pprof
```

### Shorter error-free 500-worker comparison

To observe the slow-provider path before the socket-pressure failure, a separate pair used the same environment, 500 workers, 500 ms fake delay, 3-second warmup, and a shorter 10-second attack with a 5-second CPU capture:

```bash
LOADTEST_SCENARIO=B LOADTEST_SECONDS=10 LOADTEST_PROFILE_SECONDS=5 make loadtest
```

| Path | Successful RPS | p50 ms | p95 ms | p99 ms | Errors | Requests |
|---|---:|---:|---:|---:|---:|---:|
| Direct fake | 948.00 | 501.393 | 511.123 | 620.727 | 0% | 10,001 |
| Gateway | 947.40 | 501.483 | 529.720 | 560.767 | 0% | 10,001 |

The observed p95 gateway-minus-baseline difference was **18.596 ms**; sampled gateway peak RSS was **89.48 MiB**, with 2,020 peak goroutines, 2.658 CPU core-seconds, 17 GC cycles / 4.993 ms aggregate pauses, and 246.69 MiB allocated. No retries, open circuit, queue drops, or failed batches were observed; a PostgreSQL query over the measured request window confirmed all 10,001 records, each status 200. This demonstrates roughly **947 successful requests/sec with 500 workers over ten seconds**, not sustainable 30-second capacity: the longer run above failed. It is a single-run distribution comparison, not an isolated overhead guarantee. In particular, the p50 difference was only +0.090 ms and p99 difference was −59.960 ms; independent scheduling/tail noise makes subtraction an imperfect estimate and demands repeated runs before optimization conclusions.

The Anthropic target files were also checked through both direct and gateway paths in a separate, three-second, 20-worker B run (`LOADTEST_PROVIDER=anthropic LOADTEST_SCENARIO=B LOADTEST_SECONDS=3 LOADTEST_WARMUP_SECONDS=1 LOADTEST_PROFILE_SECONDS=1 LOADTEST_WORKERS=20 make loadtest`). Each path completed 121 requests, all 200, with no queue drops or failed batches and valid CPU/heap captures. That verifies wiring, not Anthropic capacity; the performance tables above describe the OpenAI route only. `make smoke` was not run.

The headline results deliberately retain both healthy and saturated runs. No provider access was needed. Raw reports/profiles/logs remain local and gitignored; the task-owned measurement database was removed after verification, without stopping shared development containers. Redis comparisons, connection-reuse/TLS/HTTP/2 verification, recorder saturation characterization, and binary-size analysis remain later milestone work.

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

The current fake uses loopback HTTP/1.1, not production HTTPS/HTTP/2. The fake and load generator can themselves become bottlenecks, especially when they share the gateway's machine. The load runner uses this fixture for comparisons; it does not remove those limitations.

## Profiling the gateway

Set `GATEWAY_PPROF=true` before starting the gateway. The default is off, and profiles are exposed only on the metrics listener, never the API port. Keep that listener on loopback: these endpoints are unauthenticated, expose internal process details, and allow callers to trigger profiling and GC. See the [README security warning](../README.md#profiling).

With synthetic traffic running against the [local fake provider](#local-fake-provider), collect a CPU report or heap-allocation report from another terminal:

```bash
go tool pprof -top 'http://127.0.0.1:9464/debug/pprof/profile?seconds=10'
go tool pprof -top 'http://127.0.0.1:9464/debug/pprof/heap'
curl -s 'http://127.0.0.1:9464/debug/pprof/goroutine?debug=1'
```

Without `-top`, `go tool pprof` opens an interactive prompt. CPU capture is process-global, so run one capture at a time. The profile describes this gateway process, not the fake server, load generator, or database. Heap profiles sample allocation sites and live allocated bytes, not total RSS; use the existing process and Go metrics for memory under load. Stack and symbol data are internal details, and the `cmdline` endpoint exposes process arguments: treat profile artifacts as sensitive even though CPU/heap profiles do not dump raw heap objects or prompt/completion bodies.

Profiling changes the workload and can add overhead. Block and mutex sampling are not enabled automatically, so those profiles can be empty. Timed captures extend their response write deadline; they do not relax normal `/metrics` timeouts or the bounded shutdown. A capture longer than the shutdown budget can be cut short. Disable the setting and restart when finished.

These commands provide collection access only. The load runner captures profiles during synthetic traffic; an idle profile alone is not a performance measurement.

Local CLI verification on 2026-10-11 used the real gateway process, synthetic provider settings, and a task-owned temporary PostgreSQL database. `go tool pprof -top` fetched and parsed a 10-second CPU capture and a heap profile on the metrics listener; the API listener returned 404 for profiling, and restarting with `GATEWAY_PPROF=false` returned 404 on the metrics profile route while `/metrics` remained 200. No provider calls or load traffic were sent. This verifies collection and isolation only, not performance under load; the temporary database and artifacts were removed afterwards.

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

The fallback wrapper adds about 96 ns/op in the parallel, many-client fixture (159.7 versus 64.14 ns/op), with the same 40 bytes and two allocations. Its shared mutex limits how much spreading work across per-client locks helps. This difference includes the mode check and the fixed-clock fixture described above; it is not an isolated mutex latency or a measurement of healthy Redis mode. The hot-client wrapper result is more variable (±16%), since the local limiter already contends on one client's lock. Whole-request Redis/degraded-mode comparisons and profiles remain needed before deciding whether changing this wrapper is justified; the local-only HTTP runs above do not use it.

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
