#!/usr/bin/env bash
# Local synthetic measurements only. Never source a user's provider settings.
set -euo pipefail
umask 077
cd "$(dirname "$0")/../.."

fail() { printf '%s\n' "$*" >&2; exit 1; }
positive() {
    [[ "$2" =~ ^[1-9][0-9]*$ ]] && [[ ${#2} -le 6 ]] && (( $2 <= $3 )) ||
        fail "$1 must be a positive integer at most $3"
}

[[ -n "${GATEWAY_DATABASE_URL:-}" ]] || fail 'Set GATEWAY_DATABASE_URL to a migrated disposable database (see docs/performance.md).'
provider=${LOADTEST_PROVIDER:-openai}
[[ "$provider" == openai || "$provider" == anthropic ]] || fail 'LOADTEST_PROVIDER must be openai or anthropic'
selected=${LOADTEST_SCENARIO:-all}
[[ "$selected" == all || "$selected" == A || "$selected" == B || "$selected" == C ]] || fail 'LOADTEST_SCENARIO must be all, A, B, or C'
open_rate=${LOADTEST_OPEN_RATE:-}
if [[ "$selected" == C ]]; then
    positive LOADTEST_OPEN_RATE "$open_rate" 1000000
elif [[ -n "$open_rate" ]]; then
    fail 'LOADTEST_OPEN_RATE is only allowed with LOADTEST_SCENARIO=C; all derives it from A'
fi
seconds=${LOADTEST_SECONDS:-30}
warmup=${LOADTEST_WARMUP_SECONDS:-3}
profile=${LOADTEST_PROFILE_SECONDS:-10}
workers=${LOADTEST_WORKERS:-500}
cpus=${LOADTEST_CPUS:-8}
cooldown=${LOADTEST_COOLDOWN_SECONDS:-35}
fake_port=${LOADTEST_FAKE_PORT:-19000}
gateway_port=${LOADTEST_GATEWAY_PORT:-18080}
metrics_port=${LOADTEST_METRICS_PORT:-19464}
positive LOADTEST_SECONDS "$seconds" 3600
positive LOADTEST_WARMUP_SECONDS "$warmup" 60
positive LOADTEST_PROFILE_SECONDS "$profile" 60
positive LOADTEST_WORKERS "$workers" 10000
positive LOADTEST_CPUS "$cpus" 1024
positive LOADTEST_COOLDOWN_SECONDS "$cooldown" 300
(( profile < seconds )) || fail 'LOADTEST_PROFILE_SECONDS must be less than LOADTEST_SECONDS'
for port in "$fake_port" "$gateway_port" "$metrics_port"; do
    positive 'Load-test port' "$port" 65535
    (( port > 1024 )) || fail 'Load-test ports must be above 1024'
done
[[ "$fake_port" != "$gateway_port" && "$fake_port" != "$metrics_port" && "$gateway_port" != "$metrics_port" ]] || fail 'Load-test ports must differ'
for tool in go curl awk sed; do command -v "$tool" >/dev/null || fail "Missing tool: $tool"; done
curl --help all | awk '/--rate/ {found=1} END {exit !found}' || fail 'curl 7.84+ is required for connection-reusing metric sampling'
# Avoid silently benchmarking socket-limit errors on macOS's small shell default.
fd_limit=$(ulimit -n)
if [[ "$fd_limit" != unlimited ]] && (( fd_limit < workers * 2 + 1024 )); then
    ulimit -n "$((workers * 2 + 1024))" || fail 'Raise the shell file-descriptor limit before running'
fi
for port in "$fake_port" "$gateway_port" "$metrics_port"; do
    if curl --noproxy '*' -s -o /dev/null --max-time 1 "http://127.0.0.1:$port/"; then
        fail 'A load-test port is already occupied; choose unused LOADTEST_*_PORT settings'
    fi
done

mkdir -p deploy/loadtest/results
run_dir=$(mktemp -d "$PWD/deploy/loadtest/results/$(date -u +%Y%m%dT%H%M%SZ)-$provider.XXXXXX")
mkdir "$run_dir/bin"
mkdir "$run_dir/fixtures"
cp deploy/loadtest/*.json deploy/loadtest/*.targets deploy/loadtest/run.sh deploy/loadtest/summary.awk "$run_dir/fixtures/"
vegeta_module=github.com/tsenart/vegeta/v12@v12.13.0
export GOMAXPROCS="$cpus"
printf 'Preparing pinned Vegeta and local binaries; results: %s\n' "$run_dir"
# Tool installation is scoped to this run, never a global install or go.mod change.
GOBIN="$run_dir/bin" go install "$vegeta_module"
go build -o "$run_dir/bin/gateway" ./cmd/gateway
go build -o "$run_dir/bin/fakeprovider" ./cmd/fakeprovider
vegeta="$run_dir/bin/vegeta"
{
    date -u
    git rev-parse HEAD
    git status --short --untracked-files=no
    go version
    uname -a
    printf 'Vegeta=%s GOMAXPROCS=%s provider=%s scenarios=%s workers=%s seconds=%s warmup=%s profile=%s cooldown=%s FD-limit=%s\n' \
        "$vegeta_module" "$cpus" "$provider" "$selected" "$workers" "$seconds" "$warmup" "$profile" "$cooldown" "$(ulimit -n)"
    printf 'local limits; tracing off; JSON outcome logs; default recorder; HTTP/1.1 loopback; database supplied by operator\n'
} > "$run_dir/environment.txt"
sed "s/FAKE_PORT/$fake_port/g" "deploy/loadtest/baseline-$provider.targets" > "$run_dir/baseline.targets"
sed "s/GATEWAY_PORT/$gateway_port/g" "deploy/loadtest/gateway-$provider.targets" > "$run_dir/gateway.targets"

fake_pid= gateway_pid= attack_pid= sample_pid= profile_pid=
cleanup() {
    # Only our recorded children, never containers or an existing gateway.
    for pid in "$attack_pid" "$sample_pid" "$profile_pid" "$gateway_pid" "$fake_pid"; do
        if [[ -n "$pid" ]]; then kill -TERM "$pid" 2>/dev/null || true; fi
    done
    for pid in "$attack_pid" "$sample_pid" "$profile_pid" "$gateway_pid" "$fake_pid"; do
        if [[ -n "$pid" ]]; then wait "$pid" 2>/dev/null || true; fi
    done
}
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM

ready() {
    local pid=$1 url=$2 require_ok=$3
    for ((i=0; i<100; i++)); do
        kill -0 "$pid" 2>/dev/null || fail "Local server stopped; inspect logs in $run_dir"
        if [[ "$require_ok" == true ]]; then
            if curl --noproxy '*' -fs -o /dev/null --max-time 1 "$url"; then return; fi
        elif curl --noproxy '*' -s -o /dev/null --max-time 1 "$url"; then return
        fi
        sleep 0.1
    done
    fail "Local server did not become ready; inspect logs in $run_dir"
}
attack() {
    local targets=$1 output=$2 duration=$3 rate=$4 max_workers=$5
    "$vegeta" -cpus="$cpus" attack -targets="$targets" -output="$output" -duration="${duration}s" \
        -rate="$rate" -workers="$workers" -max-workers="$max_workers" \
        -connections=10000 -keepalive=true -http2=false -timeout=10s -max-body=0
}
scrape() { curl --noproxy '*' -fsS --max-time 2 "http://127.0.0.1:$metrics_port/metrics"; }

scenario() {
    local name=$1 delay=$2 rate=$3 max_workers=$4 dir="$run_dir/$1"
    mkdir "$dir"
    # macOS's default TIME_WAIT lifetime is 30 seconds. Separate phases so an
    # earlier churn run does not consume the next phase's initial socket budget.
    printf 'Cooling down for %s seconds before %s (socket state from earlier phases).\n' "$cooldown" "$name"
    sleep "$cooldown"
    printf 'Running %s: delay=%s, rate=%s, initial workers=%s, maximum=%s\n' "$name" "$delay" "$rate" "$workers" "$max_workers"
    "$run_dir/bin/fakeprovider" -addr="127.0.0.1:$fake_port" -delay="$delay" > "$dir/fake.log" 2>&1 &
    fake_pid=$!
    ready "$fake_pid" "http://127.0.0.1:$fake_port/" false
    # env -i prevents inherited provider, retry/fallback, Redis, OTLP, or proxy
    # settings changing the scenario. No values from these settings are logged.
    env -i PATH="$PATH" GOMAXPROCS="$cpus" \
        GATEWAY_DATABASE_URL="$GATEWAY_DATABASE_URL" \
        GATEWAY_ADDR="127.0.0.1:$gateway_port" GATEWAY_METRICS_ADDR="127.0.0.1:$metrics_port" \
        GATEWAY_PPROF=true GATEWAY_LOG_FORMAT=json \
        GATEWAY_CLIENTS_FILE="$PWD/deploy/loadtest/clients.json" \
        GATEWAY_PRICING_FILE="$PWD/deploy/loadtest/prices.json" \
        OPENAI_MODEL=local-openai OPENAI_API_KEY=synthetic-openai-key \
        ANTHROPIC_MODEL=local-anthropic ANTHROPIC_API_KEY=synthetic-anthropic-key \
        OPENAI_BASE_URL="http://127.0.0.1:$fake_port" ANTHROPIC_BASE_URL="http://127.0.0.1:$fake_port" \
        "$run_dir/bin/gateway" > "$dir/gateway.log" 2>&1 &
    gateway_pid=$!
    ready "$gateway_pid" "http://127.0.0.1:$metrics_port/metrics" true
    ready "$gateway_pid" "http://127.0.0.1:$gateway_port/" false
    attack "$run_dir/baseline.targets" "$dir/warmup-baseline.bin" "$warmup" 100/s "$max_workers"
    attack "$run_dir/gateway.targets" "$dir/warmup-gateway.bin" "$warmup" 100/s "$max_workers"
    attack "$run_dir/baseline.targets" "$dir/baseline.bin" "$seconds" "$rate" "$max_workers"
    "$vegeta" -cpus="$cpus" report -type=json "$dir/baseline.bin" > "$dir/baseline.json"
    "$vegeta" -cpus="$cpus" report "$dir/baseline.bin" > "$dir/baseline.txt"
    scrape > "$dir/before.prom"
    cp "$dir/before.prom" "$dir/samples.prom"
    # One curl process reuses its connection throughout the measurement. An
    # HTTP/1.1 churn test can exhaust ephemeral ports: opening a new connection
    # per scrape would lose diagnostics exactly when that limit is reached.
    curl --noproxy '*' -fsS --max-time 2 --rate 1/s --write-out '# loadtest scrape-end\n' \
        "http://127.0.0.1:$metrics_port/metrics?sample=[1-$((seconds + 2))]" \
        --next --noproxy '*' -fsS --max-time 5 -o "$dir/heap.pprof" \
        "http://127.0.0.1:$metrics_port/debug/pprof/heap" >> "$dir/samples.prom" &
    sample_pid=$!
    curl --noproxy '*' -fsS --max-time "$((profile + 5))" \
        "http://127.0.0.1:$metrics_port/debug/pprof/profile?seconds=$profile" > "$dir/cpu.pprof" &
    profile_pid=$!
    # Launch the executable directly so cleanup owns the actual process.
    "$vegeta" -cpus="$cpus" attack -targets="$run_dir/gateway.targets" -output="$dir/gateway.bin" \
        -duration="${seconds}s" -rate="$rate" -workers="$workers" -max-workers="$max_workers" \
        -connections=10000 -keepalive=true -http2=false -timeout=10s -max-body=0 &
    attack_pid=$!
    wait "$attack_pid"; attack_pid=
    wait "$profile_pid"; profile_pid=
    wait "$sample_pid"; sample_pid=
    awk '/^# loadtest scrape-end/ {last=sample; sample=""; next} {sample=sample $0 ORS} END {printf "%s", last}' \
        "$dir/samples.prom" > "$dir/after.prom"
    "$vegeta" -cpus="$cpus" report -type=json "$dir/gateway.bin" > "$dir/gateway.json"
    "$vegeta" -cpus="$cpus" report "$dir/gateway.bin" > "$dir/gateway.txt"
    go tool pprof -top -nodecount=10 "$run_dir/bin/gateway" "$dir/cpu.pprof" > "$dir/cpu-top.txt"
    go tool pprof -top -nodecount=10 "$run_dir/bin/gateway" "$dir/heap.pprof" > "$dir/heap-top.txt"
    # Graceful shutdown flushes final queue-full drop summaries and drains usage.
    kill -TERM "$gateway_pid"; wait "$gateway_pid"; gateway_pid=
    kill -TERM "$fake_pid"; wait "$fake_pid"; fake_pid=
    awk -v dir="$dir" -v rate="${rate%/s}" -f deploy/loadtest/summary.awk > "$dir/summary.md"
    cat "$dir/summary.md"
}

if [[ "$selected" == all || "$selected" == A ]]; then
    scenario A-zero-delay 0 0 "$workers"
    open_rate=$(awk -v mode=rate -v file="$run_dir/A-zero-delay/gateway.json" -f deploy/loadtest/summary.awk)
    (( open_rate > 0 )) || fail 'Scenario A did not produce a usable open-model rate'
fi
if [[ "$selected" == all || "$selected" == B ]]; then scenario B-500ms 500ms 0 "$workers"; fi
# More workers may be created to preserve fixed arrivals; a safety cap is not
# a closed-model concurrency target. The report checks the issued arrival rate.
if [[ "$selected" == all || "$selected" == C ]]; then scenario C-open 0 "$open_rate/s" 10000; fi
printf 'Completed; raw reports and profiles: %s\n' "$run_dir"
