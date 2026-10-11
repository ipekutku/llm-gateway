#!/usr/bin/env bash
# Lints the chart and renders it with each values file in its ci/ directory,
# then checks that invalid values are rejected with the expected message.
# Needs helm on PATH; run from the repository root (make helm-lint).
set -euo pipefail

chart=deploy/helm/llm-gateway
minimal=$chart/ci/minimal-values.yaml

for values in "$chart"/ci/*-values.yaml; do
    echo "== $values"
    helm lint --strict "$chart" -f "$values"
    helm template gateway "$chart" -f "$values" >/dev/null
done

failures=0

# reject <expected message> <helm template arguments...> renders the chart
# with the minimal values and the given arguments, and requires it to fail
# with a message containing the expected text.
reject() {
    local want=$1 output
    shift
    if output=$(helm template gateway "$chart" -f "$minimal" "$@" 2>&1); then
        echo "FAIL: rendering with $* succeeded, want an error containing: $want"
        failures=$((failures + 1))
    elif [[ $output != *"$want"* ]]; then
        echo "FAIL: rendering with $* failed with:"
        echo "$output"
        echo "want an error containing: $want"
        failures=$((failures + 1))
    else
        echo "ok: rejected $*"
    fi
}

echo "== invalid values"
reject "database.urlSecret.name is required" --set database.urlSecret.name=
reject "clients.secretName is required" --set clients.secretName=
reject "no provider configured" --set providers.openai.model=
reject "providers.openai.apiKeySecret.name is required" --set providers.openai.apiKeySecret.name=
reject "providers.openai.fallback needs both providers" --set providers.openai.fallback=anthropic
reject "'/providers/openai/fallback'" --set providers.openai.fallback=openai
reject "terminationGracePeriodSeconds must be at least" --set shutdown.terminationGracePeriodSeconds=39
reject "env must not set OPENAI_API_KEY" --set-json 'env=[{"name":"OPENAI_API_KEY","value":"inline"}]'
reject "env must not set GATEWAY_METRICS_ADDR" --set-json 'env=[{"name":"GATEWAY_METRICS_ADDR","value":"0.0.0.0:1"}]'
reject "'/prices/0/input': got number, want string" \
    --set-json 'prices=[{"provider":"openai","model":"gpt-4o","input":2.5,"cache_read":"0","cache_write":"0","output":"10"}]'
reject "'/logFormat'" --set logFormat=xml
reject "additional properties 'replicas' not allowed" --set replicas=2
reject "'/image/tag'" --set image.tag=
reject "'/migrations/activeDeadlineSeconds'" --set migrations.activeDeadlineSeconds=0

# render <values file> <expected: present|absent> <text> [helm template arguments...]
# requires the rendered manifests to contain, or not contain, the text.
render() {
    local values=$1 expect=$2 text=$3 output
    shift 3
    output=$(helm template gateway "$chart" -f "$values" "$@")
    if [[ ($expect == present && $output != *"$text"*) || ($expect == absent && $output == *"$text"*) ]]; then
        echo "FAIL: rendering $values $* should have $text $expect"
        failures=$((failures + 1))
    else
        echo "ok: $text $expect with $(basename "$values") $*"
    fi
}

echo "== rendered resources"
render "$minimal" present "helm.sh/hook: pre-install,pre-upgrade"
render "$minimal" absent "kind: Job" --set migrations.enabled=false
render "$chart/ci/full-values.yaml" present "name: gateway-database-owner"
# The migration pod must not match the selector of the Deployment and Service.
if helm template gateway "$chart" -f "$minimal" --show-only templates/migration-job.yaml |
    grep -Eq '^ *app\.kubernetes\.io/name: llm-gateway$'; then
    echo "FAIL: the migration Job carries the gateway's name label"
    failures=$((failures + 1))
else
    echo "ok: the migration Job has its own name label"
fi

if ((failures > 0)); then
    echo "$failures check(s) failed"
    exit 1
fi
