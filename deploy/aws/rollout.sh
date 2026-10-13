#!/usr/bin/env bash
# Opt-in ALB demonstration: 5 synthetic completions/s for 90s with one restart.
# The demo client allows 10/s and 10 concurrent requests; this stays within it.
# Results are private/gitignored; keys are only in a temporary target file.
set -euo pipefail
umask 077
domain=${TF_VAR_domain_name:-llm-gateway.utkuipek.dev}
context=${AWS_KUBE_CONTEXT:-llm-gateway-aws}
session=${AWS_SESSION_DIR:-deploy/aws/session}
[[ $domain =~ ^[a-z0-9.-]+$ && $domain != -* ]] || { echo "Invalid gateway domain_name" >&2; exit 1; }
[[ -s $session/demo-key ]] || { echo "Missing session demo-key" >&2; exit 1; }
work=$(mktemp -d)
results=$(mktemp -d "$session/rollout-XXXXXXXX")
pid=
cleanup() {
	local status=$?
	if [[ -n $pid ]]; then kill "$pid" 2>/dev/null || true; wait "$pid" 2>/dev/null || true; fi
	rm -rf "$work"
	exit "$status"
}
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM
kn=(kubectl --context "$context" --namespace llm-gateway)
"${kn[@]}" rollout status deployment/llm-gateway --timeout=300s
"${kn[@]}" get pods -l app.kubernetes.io/name=llm-gateway -o json | jq -e '
	[.items[] | select(.metadata.deletionTimestamp == null)] | length >= 2 and
	all(.[]; any(.spec.readinessGates[]?; .conditionType | startswith("target-health.elbv2.k8s.aws/")))' >/dev/null ||
	{ echo "Gateway pods must have ALB readiness gates before the demonstration" >&2; exit 1; }
curl -fsS --connect-timeout 10 --max-time 30 "https://$domain/readyz" >/dev/null
GOBIN="$work/bin" go install github.com/tsenart/vegeta/v12@v12.13.0
printf '%s\n' '{"model":"demo-openai","messages":[{"role":"user","content":"synthetic rollout check"}]}' >"$work/body.json"
printf 'POST https://%s/v1/chat/completions\nAuthorization: Bearer %s\nContent-Type: application/json\n@%s\n' \
	"$domain" "$(<"$session/demo-key")" "$work/body.json" >"$work/targets"
{
	printf 'UTC start: %s\nHostname: %s\nTraffic: 5 requests/s, 90s, one rollout restart\n' "$(date -u +%FT%TZ)" "$domain"
	"${kn[@]}" get deployment llm-gateway -o jsonpath='{.spec.template.spec.containers[0].image}{"\n"}'
	"${kn[@]}" get pods -l app.kubernetes.io/name=llm-gateway -o wide
} >"$results/environment.txt"
"$work/bin/vegeta" attack -targets="$work/targets" -rate=5 -duration=90s -timeout=30s -output="$results/requests.bin" &
pid=$!
sleep 15
kill -0 "$pid" || { echo "Load generator ended before the restart" >&2; exit 1; }
"${kn[@]}" rollout restart deployment/llm-gateway
"${kn[@]}" rollout status deployment/llm-gateway --timeout=300s
kill -0 "$pid" || { echo "Traffic ended before the rollout completed; this run cannot verify the full rollout" >&2; exit 1; }
wait "$pid"
pid=
"$work/bin/vegeta" report -type=json "$results/requests.bin" >"$results/report.json"
"$work/bin/vegeta" report "$results/requests.bin" | tee "$results/report.txt"
jq -e '.requests > 0 and .success == 1 and ((.errors // []) | length == 0) and
	(. as $r | .status_codes == {"200": $r.requests})' "$results/report.json" >/dev/null ||
	{ echo "Rollout had failed requests; inspect $results/report.json" >&2; exit 1; }
echo "ok: no failed requests during this ALB rollout run; results: $results"
