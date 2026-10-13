#!/usr/bin/env bash
# Demonstrates an RDS reboot, a node drain, a rolling restart, and autoscaling
# on the EKS deployment (make aws-demo). Run it by hand after make aws-deploy
# aws-check; it is never part of CI. It takes about 20 minutes and leaves the
# release as make aws-deploy installs it, possibly still scaled out by the
# autoscaler.
#
#   deploy/aws/demo.sh [database] [drain] [rollout] [scale]
#   SCALE_RATE=300 deploy/aws/demo.sh scale
#
# With no arguments, all scenarios run in that order; scale goes last, so the
# autoscaler does not remove pods (and their logs) during the others. Load
# comes from Vegeta on this machine, through the ALB over HTTPS, as the load
# client of make aws-deploy; the ALB balances each request across the pods.
# rollout runs deploy/aws/rollout.sh (make aws-rollout). Results go to
# deploy/aws/session/demo-<UTC time>/ (gitignored); summary.md collects the
# numbers. No step asserts timing; the numbers are a record, not a gate, and
# include this machine's round trip to the Region.
#
# Needs aws, terraform, kubectl, jq, curl, and Go; run from the repository root.
set -euo pipefail
umask 077

region=eu-north-1
domain=${TF_VAR_domain_name:-llm-gateway.utkuipek.dev}
context=${AWS_KUBE_CONTEXT:-llm-gateway-aws}
session=${AWS_SESSION_DIR:-deploy/aws/session}
namespace=llm-gateway
release=llm-gateway
selector=app.kubernetes.io/name=llm-gateway,app.kubernetes.io/instance=$release
usage_pod=llm-gateway-demo-usage
vegeta_module=github.com/tsenart/vegeta/v12@v12.13.0
results=$session/demo-$(date -u +%Y%m%dT%H%M%SZ)
summary=$results/summary.md
work=$(mktemp -d)
load=
cordoned=
usage_created=
k=(kubectl --context "$context")
kn=(kubectl --context "$context" --namespace "$namespace")

cleanup() {
	local status=$?
	if [[ -n $load ]]; then kill "$load" 2>/dev/null || true; wait "$load" 2>/dev/null || true; fi
	[[ -z $cordoned ]] || "${k[@]}" uncordon "$cordoned" >/dev/null 2>&1 || echo "uncordon $cordoned by hand" >&2
	[[ -z $usage_created ]] || "${kn[@]}" delete pod "$usage_pod" --ignore-not-found --wait=false >/dev/null 2>&1 || true
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
note() { echo "$*" | tee -a "$summary"; }
ready_pods() { "${kn[@]}" get deployment "$release" -o jsonpath='{.status.readyReplicas}'; }
# placement prints each running gateway pod that is not terminating with its node.
placement() {
	"${kn[@]}" get pods --selector "$selector" -o json | jq -r '.items[] |
		select(.status.phase == "Running" and .metadata.deletionTimestamp == null) |
		"\(.metadata.name) \(.spec.nodeName)"'
}

# attack <name> <rate> <duration> sends completions through the ALB as the
# load client, in the background; wait_attack collects the reports.
attack() {
	"$work/bin/vegeta" attack -targets="$work/targets" -rate="$2" -duration="$3" -timeout=30s \
		-output="$results/$1.bin" &
	load=$!
}
wait_attack() {
	wait "$load"
	load=
	"$work/bin/vegeta" report "$results/$1.bin" >"$results/$1.txt"
	"$work/bin/vegeta" report -type=json "$results/$1.bin" >"$results/$1.json"
	jq -r '
		def ms: . / 1e5 | round / 10;
		"- requests: \(.requests) at \(.rate * 10 | round / 10)/s, success \(.success * 10000 | round / 100)%, status codes \(.status_codes | to_entries | map("\(.key): \(.value)") | join(", "))",
		"- latency p50 \(.latencies."50th" | ms) ms, p99 \(.latencies."99th" | ms) ms, max \(.latencies.max | ms) ms",
		((.errors // [])[] | "- error: \(.)")' "$results/$1.json" | tee -a "$summary"
}

setup() {
	[[ $domain =~ ^[a-z0-9.-]+$ && $domain != -* ]] || fail "invalid gateway domain_name"
	[[ -s $session/load-key ]] || fail "$session/load-key is missing; make aws-deploy writes it when it fills llm-gateway/clients"
	mkdir -p "$results"
	"${kn[@]}" rollout status "deployment/$release" --timeout=180s >/dev/null
	curl -fsS --connect-timeout 10 --max-time 30 "https://$domain/readyz" >/dev/null ||
		fail "no HTTPS readiness at $domain; check DNS, ALB targets, and your allowed IP"
	GOBIN="$work/bin" go install "$vegeta_module"
	printf '%s\n' '{"model":"demo-openai","messages":[{"role":"user","content":"synthetic prompt"}],"max_tokens":32}' >"$work/body.json"
	# The key stays in the temporary directory, never in the results.
	printf 'POST https://%s/v1/chat/completions\nAuthorization: Bearer %s\nContent-Type: application/json\n@%s\n' \
		"$domain" "$(<"$session/load-key")" "$work/body.json" >"$work/targets"
	{
		echo "# AWS demonstrations, $(date -u +%Y-%m-%dT%H:%M:%SZ)"
		echo
		echo "- commit $(git rev-parse --short HEAD)$(git diff --quiet || echo ' (with local changes)'), chart $(awk '/^version:/ { print $2 }' deploy/helm/llm-gateway/Chart.yaml)"
		echo "- image $("${kn[@]}" get deployment "$release" -o jsonpath='{.spec.template.spec.containers[0].image}')"
		"${k[@]}" get nodes -o json | jq -r '.items[] | "- node \(.metadata.name): \(.metadata.labels."node.kubernetes.io/instance-type"), \(.metadata.labels."topology.kubernetes.io/zone"), \(.status.nodeInfo.kubeletVersion)"'
		echo "- load from $(uname -sm) through https://$domain"
		echo
	} >"$summary"
}

database() {
	local id since start status seen= ready=() t
	id=$(terraform -chdir=deploy/aws/platform output -raw database_identifier)
	[[ $id =~ ^[a-z][a-z0-9-]*$ ]] || fail "unexpected database_identifier output; run make aws-up first"
	note "## RDS reboot"
	note
	note "20 requests/s for 240 s through the ALB; after 15 s, \`aws rds reboot-db-instance\` on the single-AZ instance."
	note
	# Rows are counted from here on; earlier scenarios' requests have ended.
	since=$(date -u +%Y-%m-%dT%H:%M:%SZ)
	attack database 20 240s
	sleep 15
	start=$(date +%s)
	aws rds reboot-db-instance --region "$region" --db-instance-identifier "$id" --query DBInstance.DBInstanceStatus --output text >/dev/null
	while :; do
		t=$(($(date +%s) - start))
		status=$(aws rds describe-db-instances --region "$region" --db-instance-identifier "$id" \
			--query 'DBInstances[0].DBInstanceStatus' --output text)
		[[ $status == available ]] || seen=1
		((t % 30 < 5)) && ready+=("$(ready_pods)/$("${kn[@]}" get deployment "$release" -o jsonpath='{.status.replicas}')")
		[[ -n $seen && $status == available ]] && break
		((t < 600)) || fail "the instance was not available again after 600 s (status $status)"
		sleep 5
	done
	note "- instance status: available again $t s after the reboot request (polled every 5 s)"
	note "- ready gateway pods during the reboot, about every 30 s: ${ready[*]}"
	wait_attack database
	# Give the recorder time to flush its last batch.
	sleep 10
	"${kn[@]}" logs --selector "$selector" --container gateway --since-time="$since" --tail=-1 \
		--max-log-requests 10 >"$results/database-gateway.log"
	local warnings lost stored requests
	warnings=$(grep -c '"msg":"usage records not persisted"' "$results/database-gateway.log" || true)
	lost=$({ grep '"msg":"usage records not persisted"' "$results/database-gateway.log" || true; } |
		jq -s 'map(.count) | add // 0')
	usage_created=1
	stored=$(usage_rows "$since")
	requests=$(jq .requests "$results/database.json")
	note "- gateway logs: $warnings \`usage records not persisted\` warnings for $lost records"
	note "- usage rows stored for this scenario's client and time: $stored"
	note "- accounted for: $((stored + lost)) of $requests requests"
	note
}

# usage_rows <UTC time> counts the load client's usage records since then,
# read by a temporary psql pod as the master user: RDS is private.
usage_rows() {
	[[ $1 =~ ^[0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9:]{8}Z$ ]] || fail "invalid time $1"
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
      command: [psql, -XAt, -c, "SELECT count(*) FROM usage_records WHERE client_id = 'load' AND received_at >= '$1'"]
      env:
        - name: PGHOST
          valueFrom:
            secretKeyRef: {name: llm-gateway-database-owner, key: host}
        - name: PGDATABASE
          valueFrom:
            secretKeyRef: {name: llm-gateway-database-owner, key: database}
        - name: PGUSER
          valueFrom:
            secretKeyRef: {name: llm-gateway-database-owner, key: username}
        - name: PGPASSWORD
          valueFrom:
            secretKeyRef: {name: llm-gateway-database-owner, key: password}
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
	local phase=
	for _ in $(seq 1 60); do
		phase=$("${kn[@]}" get pod "$usage_pod" -o jsonpath='{.status.phase}')
		case $phase in Succeeded | Failed) break ;; esac
		sleep 2
	done
	[[ $phase == Succeeded ]] || fail "usage row count (pod phase: ${phase:-unknown}): $("${kn[@]}" logs "$usage_pod" 2>&1 || true)"
	"${kn[@]}" logs "$usage_pod"
}

drain() {
	local node start before after refused
	note "## Node drain"
	note
	note "20 requests/s for 150 s through the ALB; after 15 s, \`kubectl drain\` on the node with the most gateway pods, which evicts them within the PodDisruptionBudget (one unavailable at a time). The node is uncordoned afterwards."
	note
	placement >"$results/drain-before.txt"
	node=$(awk '{ print $2 }' "$results/drain-before.txt" | sort | uniq -c | sort -rn | awk 'NR == 1 { print $2 }')
	[[ -n $node ]] || fail "no running gateway pods"
	before=$(awk -v n="$node" '$2 == n' "$results/drain-before.txt" | wc -l | tr -d ' ')
	attack drain 20 150s
	sleep 15
	start=$(date +%s)
	cordoned=$node
	"${k[@]}" drain "$node" --ignore-daemonsets --delete-emptydir-data --timeout=300s >"$results/drain.log" 2>&1 ||
		fail "drain of $node failed; see $results/drain.log"
	note "- drained $node, which ran $before of $(wc -l <"$results/drain-before.txt" | tr -d ' ') gateway pods, in $(($(date +%s) - start)) s"
	refused=$(grep -c 'disruption budget' "$results/drain.log" || true)
	note "- evictions refused by a disruption budget, then retried: $refused (see drain.log for which pods)"
	"${kn[@]}" rollout status "deployment/$release" --timeout=300s >/dev/null
	placement >"$results/drain-after.txt"
	after=$(awk '{ print $2 }' "$results/drain-after.txt" | sort | uniq -c | awk '{ printf "%s%s on %s", sep, $1, $2; sep = ", " }')
	note "- gateway pods ready again $(($(date +%s) - start)) s after the drain started: $after"
	wait_attack drain
	"${k[@]}" uncordon "$node" >/dev/null
	cordoned=
	note "- $node uncordoned; pods are not moved back until they are replaced"
	note
}

rollout() {
	note "## Rolling restart"
	note
	note "\`make aws-rollout\`: 5 requests/s for 90 s as the demo client, with one \`kubectl rollout restart\`; it fails on any unsuccessful request."
	note
	if bash deploy/aws/rollout.sh 2>&1 | tee "$results/rollout.txt"; then
		note "- $(tail -n 1 "$results/rollout.txt")"
	else
		note "- failed: $(tail -n 1 "$results/rollout.txt")"
	fi
	note
}

scale() {
	local rate=${SCALE_RATE:-200} min waited=0 start i
	note "## Autoscaling under load"
	note
	note "$rate requests/s for 180 s through the ALB; the HPA targets 70% of the gateway container's 100m CPU request, between 2 and 4 pods."
	note
	min=$("${kn[@]}" get hpa "$release" -o jsonpath='{.spec.minReplicas}')
	until [[ $(ready_pods) == "$min" && $("${kn[@]}" get deployment "$release" -o jsonpath='{.spec.replicas}') == "$min" ]]; do
		((waited < 420)) || fail "the autoscaler did not return to $min pods"
		sleep 15
		waited=$((waited + 15))
	done
	note "- started with $min ready pods, after waiting $waited s for the autoscaler to scale down"
	note
	attack scale "$rate" 180s
	start=$(date +%s)
	note "| s | gateway CPU (% of request) | desired pods | ready pods |"
	note "|---|---|---|---|"
	for i in $(seq 0 13); do
		note "| $(($(date +%s) - start)) | $("${kn[@]}" get hpa "$release" -o jsonpath='{.status.currentMetrics[0].containerResource.current.averageUtilization}') | $("${kn[@]}" get hpa "$release" -o jsonpath='{.status.desiredReplicas}') | $(ready_pods) |"
		sleep 15
	done
	wait_attack scale
	note "- pods per node at the end: $(placement | awk '{ print $2 }' | sort | uniq -c | awk '{ printf "%s%s on %s", sep, $1, $2; sep = ", " }')"
	note
}

setup
if (($# == 0)); then
	set -- database drain rollout scale
fi
for scenario in "$@"; do
	case $scenario in
	database | drain | rollout | scale) "$scenario" ;;
	*) fail "unknown scenario: $scenario" ;;
	esac
done
echo "results: $results"
