#!/usr/bin/env bash
# Checks the gateway deployed by make kind-deploy: every pod ready, one
# authenticated chat completion per provider answered by the fake sidecar,
# and the requests counted on the metrics port. No timing thresholds.
# Needs kubectl and curl; run from the repository root (make kind-check).
set -euo pipefail

context=${KIND_CONTEXT:-kind-llm-gateway}
namespace=${KIND_NAMESPACE:-llm-gateway}
release=${KIND_RELEASE:-llm-gateway}
api_port=18080
metrics_port=19464
kubectl=(kubectl --context "$context" --namespace "$namespace")

"${kubectl[@]}" rollout status "deployment/$release" --timeout=180s

"${kubectl[@]}" port-forward "service/$release" "$api_port:8080" "$metrics_port:9464" >/dev/null &
forward=$!
trap 'kill $forward 2>/dev/null || true' EXIT
for _ in $(seq 1 30); do
    if curl -fsS "http://127.0.0.1:$api_port/readyz" >/dev/null 2>&1; then
        break
    fi
    sleep 1
done
curl -fsS "http://127.0.0.1:$api_port/readyz" >/dev/null

for model in local-openai local-anthropic; do
    response=$(curl -fsS "http://127.0.0.1:$api_port/v1/chat/completions" \
        -H 'Authorization: Bearer synthetic-kind-demo-key' \
        -H 'Content-Type: application/json' \
        -d "{\"model\":\"$model\",\"messages\":[{\"role\":\"user\",\"content\":\"synthetic prompt\"}]}")
    if [[ $response != *'"content":"synthetic completion"'* ]]; then
        echo "unexpected $model response: $response" >&2
        exit 1
    fi
    echo "ok: $model completion"
done

# Each pod counts its own requests, and port-forward picks one pod, so the
# requests above may be split; at least one success must be counted.
metrics=$(curl -fsS "http://127.0.0.1:$metrics_port/metrics")
if ! grep -Eq '^gateway_requests_total\{code="",model="local-(openai|anthropic)",status="200"\} [1-9]' <<<"$metrics"; then
    echo "no successful request counted on the metrics port" >&2
    grep '^gateway_requests_total' <<<"$metrics" >&2 || true
    exit 1
fi
echo "ok: metrics count the requests"
