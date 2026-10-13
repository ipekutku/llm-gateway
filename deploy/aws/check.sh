#!/usr/bin/env bash
# Checks the gateway deployed by make aws-deploy (make aws-check):
# - one authenticated completion per provider through a port-forward to the
#   Service, answered by the fake provider sidecar;
# - the requests counted on that pod's metrics port, with rate limits shared
#   through Valkey (gateway_ratelimit_store_up 1);
# - shared limits: 30 requests for an unconfigured model, alternating between
#   two pods, from the limited client (bursts of 10, then one request every
#   10 seconds). Shared limits admit 10 or 11 in total; limits per pod would
#   admit about 20. Admitted requests are answered 404, limited ones 429,
#   neither reaching a provider;
# - both completions' usage records in RDS, read by a temporary psql pod as
#   the master user;
# - both completions' log lines in CloudWatch Logs, identical to the pod's
#   own lines and in that pod's stream, waiting up to AWS_CHECK_LOG_WAIT
#   seconds (default 120) for delivery.
# The client keys come from deploy/aws/session/, written by make aws-deploy;
# they reach curl through files, never arguments. No timing thresholds.
# Needs kubectl, curl, jq, terraform, and aws; run from the repository root.
set -euo pipefail
umask 077

context=${AWS_KUBE_CONTEXT:-llm-gateway-aws}
namespace=llm-gateway
release=llm-gateway
session=${AWS_SESSION_DIR:-deploy/aws/session}
usage_pod=llm-gateway-check-usage
api_port=28080
metrics_port=29464
pod_ports=(28081 28082)
log_wait=${AWS_CHECK_LOG_WAIT:-120}
# CloudWatch searches start here, in milliseconds, with a margin for clock skew.
log_start=$((($(date +%s) - 300) * 1000))
work=$(mktemp -d)
forwards=()
kn=(kubectl --context "$context" --namespace "$namespace")

cleanup() {
	local status=$?
	for pid in ${forwards[@]+"${forwards[@]}"}; do
		kill "$pid" 2>/dev/null || true
	done
	"${kn[@]}" delete pod "$usage_pod" --ignore-not-found --wait=false >/dev/null 2>&1 || true
	rm -rf "$work"
	exit "$status"
}
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM

fail() {
	echo "FAIL: $*" >&2
	exit 1
}

for client in demo limited; do
	[[ -s $session/$client-key ]] || fail "$session/$client-key is missing; it is written by make aws-deploy when it fills llm-gateway/clients"
	printf 'Authorization: Bearer %s\n' "$(<"$session/$client-key")" >"$work/$client.header"
done

# forward TARGET LOCAL:REMOTE... starts a port-forward and waits until the
# first port answers /readyz.
forward() {
	local target=$1 port=${2%%:*}
	shift
	"${kn[@]}" port-forward "$target" "$@" >/dev/null &
	forwards+=($!)
	for _ in $(seq 1 30); do
		curl -fsS "http://127.0.0.1:$port/readyz" >/dev/null 2>&1 && return
		sleep 1
	done
	fail "no answer through the port-forward to $target"
}

"${kn[@]}" rollout status "deployment/$release" --timeout=180s
forward "service/$release" "$api_port:8080" "$metrics_port:9464"

ids=()
for model in demo-openai demo-anthropic; do
	curl -fsS -D "$work/headers" -o "$work/body" "http://127.0.0.1:$api_port/v1/chat/completions" \
		-H @"$work/demo.header" -H 'Content-Type: application/json' \
		-d "{\"model\":\"$model\",\"messages\":[{\"role\":\"user\",\"content\":\"synthetic prompt\"}]}" ||
		fail "$model completion request"
	[[ $(jq -r '.choices[0].message.content' "$work/body") == "synthetic completion" ]] ||
		fail "unexpected $model response: $(<"$work/body")"
	id=$(tr -d '\r' <"$work/headers" | awk 'tolower($1) == "x-request-id:" { print $2 }')
	[[ $id =~ ^[A-Z2-7]{26}$ ]] || fail "$model response has no request ID"
	ids+=("$id")
	echo "ok: $model completion (request $id)"
done

metrics=$(curl -fsS "http://127.0.0.1:$metrics_port/metrics")
grep -Eq '^gateway_requests_total\{code="",model="demo-(openai|anthropic)",status="200"\} [1-9]' <<<"$metrics" ||
	fail "no successful request counted on the metrics port"
grep -qx 'gateway_ratelimit_store_up 1' <<<"$metrics" ||
	fail "rate limits are not shared through Valkey: $(grep '^gateway_ratelimit_store' <<<"$metrics" || echo 'no store metrics')"
echo "ok: metrics count the requests; rate limits are shared through Valkey"

# Shared limits across two pods.
pods=()
while read -r pod; do
	[[ -n $pod ]] && pods+=("$pod")
done < <("${kn[@]}" get pods --selector "app.kubernetes.io/name=llm-gateway,app.kubernetes.io/instance=$release" \
	--field-selector status.phase=Running -o jsonpath='{range .items[*]}{.metadata.name}{"\n"}{end}')
((${#pods[@]} >= 2)) || fail "need two running gateway pods, found ${#pods[@]}"
forward "pod/${pods[0]}" "${pod_ports[0]}:8080"
forward "pod/${pods[1]}" "${pod_ports[1]}:8080"
admitted=(0 0)
limited=(0 0)
for i in $(seq 0 29); do
	p=$((i % 2))
	code=$(curl -sS -o /dev/null -w '%{http_code}' "http://127.0.0.1:${pod_ports[$p]}/v1/chat/completions" \
		-H @"$work/limited.header" -H 'Content-Type: application/json' \
		-d '{"model":"not-configured","messages":[{"role":"user","content":"x"}]}') || code=000
	case $code in
	404) admitted[p]=$((admitted[p] + 1)) ;;
	429) limited[p]=$((limited[p] + 1)) ;;
	*) fail "unexpected status $code from pod ${pods[$p]}" ;;
	esac
done
total=$((admitted[0] + admitted[1]))
for p in 0 1; do
	echo "  pod ${pods[$p]}: ${admitted[$p]} admitted, ${limited[$p]} limited"
done
if ((total > 11)); then
	fail "$total of 30 admitted: limits are not shared across pods (per-pod limits admit about 20)"
elif ((total < 10)); then
	fail "$total of 30 admitted, fewer than the burst of 10: an earlier run may still be refilling the limited client's burst; wait two minutes and retry"
fi
echo "ok: shared limits, $total of 30 admitted across two pods"

# Usage records, read in the cluster: RDS is not reachable from outside.
"${kn[@]}" delete pod "$usage_pod" --ignore-not-found --wait=true >/dev/null
"${kn[@]}" apply -f - >/dev/null <<EOF
apiVersion: v1
kind: Pod
metadata:
  name: $usage_pod
spec:
  restartPolicy: Never
  activeDeadlineSeconds: 90
  automountServiceAccountToken: false
  securityContext:
    runAsNonRoot: true
    runAsUser: 999
    runAsGroup: 999
    seccompProfile:
      type: RuntimeDefault
  containers:
    - name: psql
      image: postgres:18
      # The request IDs were checked against the gateway's alphabet above.
      command:
        - sh
        - -c
        - |
          query="SELECT request_id, client_id, requested_model, provider, status, input_tokens, output_tokens, cost_usd
                 FROM usage_records WHERE request_id IN ('${ids[0]}', '${ids[1]}') ORDER BY received_at"
          for i in \$(seq 1 15); do
            rows=\$(psql -XAt -F ' ' -c "\$query") || exit 1
            if [ "\$(printf '%s\n' "\$rows" | grep -c .)" -eq 2 ]; then
              printf '%s\n' "\$rows"
              exit 0
            fi
            sleep 2
          done
          echo "usage records not found"
          exit 1
      env:
        - name: PGHOST
          valueFrom:
            secretKeyRef:
              name: llm-gateway-database-owner
              key: host
        - name: PGDATABASE
          valueFrom:
            secretKeyRef:
              name: llm-gateway-database-owner
              key: database
        - name: PGUSER
          valueFrom:
            secretKeyRef:
              name: llm-gateway-database-owner
              key: username
        - name: PGPASSWORD
          valueFrom:
            secretKeyRef:
              name: llm-gateway-database-owner
              key: password
        - name: PGSSLMODE
          value: verify-full
        - name: PGSSLROOTCERT
          value: /etc/llm-gateway/database-ca/ca.pem
        - name: PGCONNECT_TIMEOUT
          value: "10"
      volumeMounts:
        - name: database-ca
          mountPath: /etc/llm-gateway/database-ca
          readOnly: true
      securityContext:
        allowPrivilegeEscalation: false
        readOnlyRootFilesystem: true
        capabilities:
          drop: [ALL]
  volumes:
    - name: database-ca
      configMap:
        name: llm-gateway-database-ca
EOF
phase=
for _ in $(seq 1 60); do
	phase=$("${kn[@]}" get pod "$usage_pod" -o jsonpath='{.status.phase}')
	case $phase in Succeeded | Failed) break ;; esac
	sleep 2
done
"${kn[@]}" logs "$usage_pod" || true
[[ $phase == Succeeded ]] || fail "usage records check (pod phase: ${phase:-unknown})"
echo "ok: both completions have usage records in RDS"

# Log lines in CloudWatch: Fluent Bit sends each line unchanged, so the
# events for a request equal the pod's lines for it, nothing added.
log_group=$(terraform -chdir=deploy/aws/platform output -raw application_log_group)
[[ $log_group =~ ^/[A-Za-z0-9/_.-]+$ ]] || fail "unexpected application_log_group output; run make aws-up first"
"${kn[@]}" logs --selector "app.kubernetes.io/name=llm-gateway,app.kubernetes.io/instance=$release" \
	--container gateway --prefix --tail=-1 --max-log-requests 10 >"$work/pod-logs"
for id in "${ids[@]}"; do
	# --prefix writes [pod/<pod>/gateway] before each line.
	grep -F "\"request_id\":\"$id\"" "$work/pod-logs" >"$work/request-lines" ||
		fail "request $id has no line in the gateway pods' logs"
	pod=$(head -n 1 "$work/request-lines" | sed -E 's|^\[pod/([^/]+)/gateway\] .*|\1|')
	[[ $pod =~ ^[a-z0-9.-]+$ ]] || fail "unexpected kubectl logs prefix for request $id"
	expected=$(sed -E 's|^\[pod/[^]]+\] ||' "$work/request-lines" | jq -Rsc 'split("\n") | map(select(. != "")) | sort')
	deadline=$((SECONDS + log_wait))
	while :; do
		events=$(aws logs filter-log-events --region eu-north-1 --log-group-name "$log_group" \
			--start-time "$log_start" --filter-pattern "{ \$.request_id = \"$id\" }" --output json)
		jq -e --argjson expected "$expected" --arg stream "pod/$pod.gateway" \
			'([.events[].message] | sort) == $expected and all(.events[]; .logStreamName == $stream)' <<<"$events" >/dev/null && break
		if ((SECONDS >= deadline)); then
			[[ $(jq '.events | length' <<<"$events") != 0 ]] ||
				fail "request $id has no log events in $log_group after ${log_wait}s; see kubectl -n fluent-bit logs daemonset/fluent-bit"
			fail "the CloudWatch events for request $id differ from pod $pod's lines"
		fi
		sleep 5
	done
	echo "ok: request $id logged unchanged in $log_group (stream pod/$pod.gateway)"
done
echo "AWS checks passed."
