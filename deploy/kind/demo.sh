#!/usr/bin/env bash
# Demonstrates shared limits, shutdown, rolling restarts, a database outage,
# and autoscaling on the local kind cluster (make kind-demo). Run it by hand
# after make kind-up kind-deploy; it is never part of CI. It takes about ten
# minutes and leaves the release as make kind-deploy installs it, possibly
# still scaled out by the autoscaler.
#
#   deploy/kind/demo.sh [scale] [limits] [sigterm] [rollout] [database]
#   ROLLOUT_RATE=200 ROLLOUT_RESTARTS=5 deploy/kind/demo.sh rollout
#
# With no arguments, all scenarios run in that order. scale first waits for
# the autoscaler to return to its minimum of two pods, which takes up to five
# minutes after earlier load: the HPA controller remembers its recent
# recommendations, even for a reinstalled HPA of the same name. Load comes from Vegeta
# inside the kind node, sent to the Service's cluster IP, so kube-proxy
# spreads connections across pods like any in-cluster client; a port-forward
# would pin them to one pod. Results, raw Vegeta output, and gateway logs go
# to deploy/kind/results/<UTC time>/ (gitignored); summary.md collects the
# numbers. No step asserts timing; the numbers are a record, not a gate.
#
# Needs Docker, Go, kubectl, helm, and make, from the repository root.
set -euo pipefail

cluster=${KIND_CLUSTER:-llm-gateway}
context=kind-$cluster
namespace=${KIND_NAMESPACE:-llm-gateway}
release=${KIND_RELEASE:-llm-gateway}
node=$cluster-control-plane
vegeta_module=github.com/tsenart/vegeta/v12@v12.13.0
results=deploy/kind/results/$(date -u +%Y%m%dT%H%M%SZ)
summary=$results/summary.md

k() { kubectl --context "$context" --namespace "$namespace" "$@"; }
on_node() { docker exec -i "$node" "$@"; }
# on_node_quiet runs a command on the node without passing it stdin, so it
# can run inside a while read loop.
on_node_quiet() { docker exec "$node" "$@"; }
note() { echo "$*" | tee -a "$summary"; }
now() { on_node date +%s.%N; }

service_ip() { k get service "$release" -o jsonpath='{.spec.clusterIP}'; }
# gateway_pods prints the name and IP of each running gateway pod that is not
# terminating.
gateway_pods() {
    k get pods -l "app.kubernetes.io/name=llm-gateway" -o json | python3 -c '
import json, sys
for p in json.load(sys.stdin)["items"]:
    if p["status"].get("phase") == "Running" and "deletionTimestamp" not in p["metadata"]:
        print(p["metadata"]["name"], p["status"]["podIP"])'
}

# pod_count <metrics URL> <code> prints a pod's gateway_requests_total for
# requests with an unknown model and the given error code, or 0.
pod_count() {
    on_node_quiet curl -fsS "$1" | awk -v code="$2" '
        $1 ~ "^gateway_requests_total\\{code=\"" code "\",model=\"unknown\"" { n += $2 }
        END { print n + 0 }'
}

# attack <name> <rate> <duration> sends chat completions as the load client
# through the Service and writes the Vegeta report to the results.
attack() {
    local name=$1 rate=$2 duration=$3
    on_node vegeta attack -rate="$rate" -duration="$duration" -timeout=30s \
        -targets=/tmp/demo/chat.targets -output="/tmp/demo/$name.bin"
    on_node vegeta report "/tmp/demo/$name.bin" > "$results/$name.txt"
    on_node vegeta report -type=json "/tmp/demo/$name.bin" > "$results/$name.json"
}

# report <name> adds a Vegeta report's totals, status codes, and errors.
report() {
    python3 - "$results/$1.json" <<'EOF' | tee -a "$summary"
import json, sys
r = json.load(open(sys.argv[1]))
codes = ", ".join(f"{c}: {n}" for c, n in sorted(r["status_codes"].items()))
print(f"- requests: {r['requests']} at {r['rate']:.1f}/s, success {r['success'] * 100:.2f}%, status codes {codes}")
print(f"- latency p50 {r['latencies']['50th'] / 1e6:.1f} ms, p99 {r['latencies']['99th'] / 1e6:.1f} ms, max {r['latencies']['max'] / 1e6:.1f} ms")
for e in r.get("errors") or []:
    print(f"- error: {e}")
EOF
}

setup() {
    mkdir -p "$results"
    k rollout status "deployment/$release" --timeout=180s >/dev/null
    {
        echo "# Kubernetes demonstrations, $(date -u +%Y-%m-%dT%H:%M:%SZ)"
        echo
        echo "- commit $(git rev-parse --short HEAD)$(git diff --quiet || echo ' (with local changes)'), chart $(awk '/^version:/ { print $2 }' deploy/helm/llm-gateway/Chart.yaml)"
        echo "- node $(on_node sh -c 'echo "$(nproc) CPUs, $(awk "/MemTotal/ { printf \"%.1f GiB\", \$2 / 1048576 }" /proc/meminfo)"'), Kubernetes $(k version -o json | python3 -c 'import json,sys; print(json.load(sys.stdin)["serverVersion"]["gitVersion"])')"
        echo "- Docker $(docker version --format '{{.Server.Version}} ({{.Server.Os}}/{{.Server.Arch}})')"
        echo
    } > "$summary"

    # Vegeta for the node's platform, built pinned like the load tests.
    local arch modcache
    case $(on_node uname -m) in
        x86_64) arch=amd64 ;;
        aarch64) arch=arm64 ;;
        *) echo "unsupported node architecture" >&2; exit 1 ;;
    esac
    # Cross-compiled installs refuse a GOBIN, so the binary lands in a
    # temporary GOPATH instead; the module cache stays shared.
    modcache=$(go env GOMODCACHE)
    env -u GOBIN GOOS=linux GOARCH=$arch CGO_ENABLED=0 GOPATH="$PWD/$results/go" GOMODCACHE="$modcache" \
        go install "$vegeta_module"
    local bin
    bin=$(find "$results/go/bin" -name vegeta -type f | head -1)
    on_node sh -c 'cat > /usr/local/bin/vegeta && chmod 755 /usr/local/bin/vegeta' < "$bin"
    chmod -R u+w "$results/go"
    rm -rf "$results/go"

    on_node mkdir -p /tmp/demo
    on_node sh -c 'cat > /tmp/demo/chat.json' <<'EOF'
{"model":"local-openai","messages":[{"role":"user","content":"synthetic prompt"}],"max_tokens":32}
EOF
    on_node sh -c 'cat > /tmp/demo/chat.targets' <<EOF
POST http://$(service_ip):8080/v1/chat/completions
Authorization: Bearer synthetic-kind-load-key
Content-Type: application/json
@/tmp/demo/chat.json
EOF
}

# burst <label> sends 30 requests for an unconfigured model, one connection
# each, as the limited client (burst 10, then 1 per second). An admitted
# request is answered 404, a limited one 429, both without a provider call.
burst() {
    local label=$1 ip pod pod_ip admitted limited before=$results/limits.before
    ip=$(service_ip)
    gateway_pods | while read -r pod pod_ip; do
        echo "$pod $pod_ip $(pod_count "http://$pod_ip:9464/metrics" model_not_found) $(pod_count "http://$pod_ip:9464/metrics" rate_limit_exceeded)"
    done > "$before"
    on_node sh -c "for i in \$(seq 30); do curl -s -o /dev/null -w '%{http_code}\n' \
        -H 'Authorization: Bearer synthetic-kind-limited-key' -H 'Content-Type: application/json' \
        -d '{\"model\":\"not-configured\",\"messages\":[{\"role\":\"user\",\"content\":\"x\"}]}' \
        http://$ip:8080/v1/chat/completions; done" > "$results/limits-$label.codes"
    note "- $label: $(grep -c '^404$' "$results/limits-$label.codes") of 30 admitted, $(grep -c '^429$' "$results/limits-$label.codes") limited, $(grep -vcE '^(404|429)$' "$results/limits-$label.codes") other"
    while read -r pod pod_ip admitted limited; do
        admitted=$(($(pod_count "http://$pod_ip:9464/metrics" model_not_found) - admitted))
        limited=$(($(pod_count "http://$pod_ip:9464/metrics" rate_limit_exceeded) - limited))
        note "  - pod $pod: $admitted admitted, $limited limited"
    done < "$before"
    rm "$before"
}

limits() {
    note "## Shared rate limits"
    note
    note "30 requests in about half a second from a client allowed bursts of 10, over new connections spread across the pods."
    note
    burst "with Redis"
    helm --kube-context "$context" upgrade "$release" deploy/helm/llm-gateway --namespace "$namespace" \
        -f deploy/kind/values.yaml --set redis.urlSecret.name= --wait --timeout 5m >/dev/null
    k rollout status "deployment/$release" --timeout=180s >/dev/null
    burst "without Redis (redis.urlSecret unset, pods replaced)"
    make -s kind-deploy >/dev/null
    k rollout status "deployment/$release" --timeout=180s >/dev/null
    note
}

sigterm() {
    note "## SIGTERM to one pod"
    note
    local pod ip
    read -r pod ip < <(gateway_pods)
    on_node sh -c "cat > /tmp/demo/timeline.sh" <<EOF
end=\$((\$(date +%s) + 12))
while [ \$(date +%s) -lt \$end ]; do
    ready=\$(curl -s -o /dev/null -w '%{http_code}' -m 2 http://$ip:8080/readyz)
    chat=\$(curl -s -o /dev/null -w '%{http_code}' -m 5 -H 'Authorization: Bearer synthetic-kind-load-key' \
        -H 'Content-Type: application/json' -d @/tmp/demo/chat.json http://$ip:8080/v1/chat/completions)
    echo "\$(date +%s.%N) \$ready \$chat"
    sleep 0.1
done
EOF
    on_node sh -c 'sh /tmp/demo/timeline.sh > /tmp/demo/timeline.out 2>&1' &
    local timeline=$!
    sleep 2
    local deleted
    deleted=$(now)
    k delete pod "$pod" --wait=false >/dev/null
    wait "$timeline"
    on_node cat /tmp/demo/timeline.out > "$results/sigterm.timeline"
    awk -v t0="$deleted" -v pod="$pod" '
        { t = $1 - t0 }
        t < 0 { if ($2 == 200 && $3 == 200) before++; next }
        $2 == 503 && !first503 { first503 = t }
        $3 == 200 { last200 = t; served++ }
        $3 != 200 && $3 != "000" { failed++ }
        ($2 == "000" || $3 == "000") && !closed { closed = t }
        END {
            printf "Requests to pod %s, one readiness probe and one chat completion (200 ms fake delay) about every 0.3 s; times from the delete request.\n\n", pod
            printf "- before: %d rounds, readiness 200 and completion 200\n", before
            printf "- readiness 503 from %.1f s\n", first503
            printf "- completions 200 until %.1f s (%d after the delete), %d failed with an HTTP error\n", last200, served, failed
            printf "- connections refused from %.1f s\n", closed
        }' "$results/sigterm.timeline" | tee -a "$summary"
    k logs "$pod" -c gateway > "$results/sigterm-gateway.log" 2>/dev/null || true
    note
    k rollout status "deployment/$release" --timeout=180s >/dev/null
}

rollout() {
    # ROLLOUT_RATE and ROLLOUT_RESTARTS raise the load and repeat the restart,
    # to compare shutdown behavior between builds.
    local rate=${ROLLOUT_RATE:-50} restarts=${ROLLOUT_RESTARTS:-1} duration
    duration=$((60 + 30 * restarts))
    note "## Rolling restart under load"
    note
    note "$rate requests/s for $duration s through the Service; \`kubectl rollout restart\` after 15 s, $restarts time(s), each once the previous one has finished."
    note
    attack rollout "$rate" "${duration}s" &
    local load=$!
    sleep 15
    local i start old pod
    for i in $(seq "$restarts"); do
        old=$(gateway_pods | awk '{ print $1 }')
        start=$(date +%s)
        k rollout restart "deployment/$release" >/dev/null
        k rollout status "deployment/$release" --timeout=180s >/dev/null
        note "- restart $i: the Deployment reported $(k get deployment "$release" -o jsonpath='{.status.replicas}') new pods ready after $(($(date +%s) - start)) s"
        for pod in $old; do
            k wait --for=delete "pod/$pod" --timeout=120s >/dev/null 2>&1 || true
        done
        note "- restart $i: the old pods had drained and exited after $(($(date +%s) - start)) s"
    done
    wait "$load"
    report rollout
    note
}

database() {
    note "## PostgreSQL deleted"
    note
    note "20 requests/s for 120 s through the Service. After 15 s the PostgreSQL Deployment is scaled to zero, which deletes its data; after 60 s it is scaled back, and \`make kind-deploy\` reruns the migration hook on the empty database."
    note
    # The node's clock, which stamps the usage records, not this machine's.
    local since
    since=$(on_node date -u +%Y-%m-%dT%H:%M:%S.%NZ)
    attack database 20 120s &
    local load=$!
    sleep 15
    local deleted_rows
    deleted_rows=$(k exec deployment/postgres -- psql -U gateway -Atc "select count(*) from usage_records where client_id = 'load' and received_at >= '$since'")
    k scale deployment/postgres --replicas=0 >/dev/null
    k wait --for=delete pod -l app=postgres --timeout=60s >/dev/null 2>&1 || true
    local ready=() i
    for i in 1 2 3 4 5 6; do
        sleep 7
        ready+=("$(k get deployment "$release" -o jsonpath='{.status.readyReplicas}')/$(k get deployment "$release" -o jsonpath='{.status.replicas}')")
    done
    note "- ready gateway pods during the outage, every 7 s: ${ready[*]}"
    k scale deployment/postgres --replicas=1 >/dev/null
    k rollout status deployment/postgres --timeout=120s >/dev/null
    local recovered
    recovered=$(date +%s)
    make -s kind-deploy >/dev/null
    note "- database back and migrated $(($(date +%s) - recovered)) s after PostgreSQL was ready again"
    wait "$load"
    report database
    k logs -l app.kubernetes.io/name=llm-gateway -c gateway --since-time="$since" --tail=-1 > "$results/database-gateway.log"
    local warnings lost stored
    warnings=$(grep -c '"msg":"usage records not persisted"' "$results/database-gateway.log" || true)
    lost=$(grep '"msg":"usage records not persisted"' "$results/database-gateway.log" | grep -o '"count":[0-9]*' | awk -F: '{ n += $2 } END { print n + 0 }')
    stored=$(k exec deployment/postgres -- psql -U gateway -Atc "select count(*) from usage_records where client_id = 'load'")
    note "- rows of this scenario counted in the database just before it was deleted: $deleted_rows (lost with it)"
    note "- gateway logs: $warnings \`usage records not persisted\` warnings for $lost records"
    note "- rows stored by the new database: $stored, all sent after it was migrated"
    note "- accounted for: $((deleted_rows + lost + stored)) of $(python3 -c 'import json,sys; print(json.load(open(sys.argv[1]))["requests"])' "$results/database.json") requests; records written between the count and the deletion are in neither number"
    note
}

scale() {
    note "## Autoscaling under load"
    note
    note "200 requests/s for 180 s through the Service; the HPA targets 70% of the gateway container's 100m CPU request, between 2 and 4 pods."
    note
    local min waited=0
    min=$(k get hpa "$release" -o jsonpath='{.spec.minReplicas}')
    until [[ $(k get deployment "$release" -o jsonpath='{.status.readyReplicas}') == "$min" && $(k get deployment "$release" -o jsonpath='{.spec.replicas}') == "$min" ]]; do
        if ((waited >= 420)); then
            echo "the autoscaler did not return to $min pods" >&2
            exit 1
        fi
        sleep 15
        waited=$((waited + 15))
    done
    note "- started with $min ready pods, after waiting $waited s for the autoscaler to scale down"
    note
    attack scale 200 180s &
    local load=$!
    local start i
    start=$(date +%s)
    note "| s | gateway CPU (% of request) | desired pods | ready pods |"
    note "|---|---|---|---|"
    for i in $(seq 0 13); do
        note "| $(($(date +%s) - start)) | $(k get hpa "$release" -o jsonpath='{.status.currentMetrics[0].containerResource.current.averageUtilization}') | $(k get hpa "$release" -o jsonpath='{.status.desiredReplicas}') | $(k get deployment "$release" -o jsonpath='{.status.readyReplicas}') |"
        sleep 15
    done
    wait "$load"
    note
    report scale
    note
}

setup
if (($# == 0)); then
    set -- scale limits sigterm rollout database
fi
for scenario in "$@"; do
    case $scenario in
        scale | limits | sigterm | rollout | database) "$scenario" ;;
        *) echo "unknown scenario: $scenario" >&2; exit 1 ;;
    esac
done
echo "results: $results"
