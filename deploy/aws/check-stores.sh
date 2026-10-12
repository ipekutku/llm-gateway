#!/usr/bin/env bash
# Checks the platform's data stores from inside the cluster (make
# aws-check-stores, after make aws-up). A throwaway pod connects to PostgreSQL
# with sslmode=verify-full against the Region's RDS CA bundle, and to Valkey
# over TLS with the AUTH token, verified against Amazon's root CAs; it also
# checks that each store refuses connections without TLS or the token.
# Credentials go from Secrets Manager into a temporary Secret through stdin,
# never through command arguments, and the namespace is deleted afterwards.
# Needs aws, terraform, kubectl, jq, and curl; run from the repository root.
set -euo pipefail
umask 077

context=${AWS_KUBE_CONTEXT:-llm-gateway-aws}
platform=deploy/aws/platform
region=eu-north-1
work=$(mktemp -d)
namespace="llm-gateway-store-check-$(basename "$work" | tr '[:upper:].' '[:lower:]-')"
created_namespace=false
kubectl=(kubectl --context "$context" --namespace "$namespace" --request-timeout=15s)

cleanup() {
	local status=$?
	if [[ $created_namespace == true ]]; then
		if ! kubectl --context "$context" --request-timeout=15s delete namespace "$namespace" --timeout=120s >/dev/null; then
			echo "Cleanup failed: delete temporary namespace $namespace in context $context." >&2
			status=1
		fi
	fi
	rm -rf "$work"
	exit "$status"
}
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM

output() { terraform -chdir="$platform" output -raw "$1"; }
db_host=$(output database_address)
db_name=$(output database_name)
db_secret=$(output database_master_secret_arn)
redis_host=$(output redis_address)
redis_secret=$(output redis_auth_token_secret_name)

# The Region's RDS bundle, and Amazon's root CAs, which sign ElastiCache's
# certificates.
curl -fsS --proto '=https' --connect-timeout 10 --max-time 30 -o "$work/rds.pem" \
	"https://truststore.pki.rds.amazonaws.com/$region/$region-bundle.pem"
for root in AmazonRootCA1 AmazonRootCA2 AmazonRootCA3 AmazonRootCA4 SFSRootCAG2; do
	curl -fsS --proto '=https' --connect-timeout 10 --max-time 30 "https://www.amazontrust.com/repository/$root.pem"
	echo
done >"$work/amazon.pem"

cat >"$work/postgres.sh" <<'EOF'
set -eu
export PGCONNECT_TIMEOUT=10
conn="host=$DB_HOST dbname=$DB_NAME sslrootcert=/check/rds.pem"
timeout 20 psql -X "$conn sslmode=verify-full" -v ON_ERROR_STOP=1 -At \
	-c "select 'postgres: ' || version()" \
	-c "select 'tls: ' || version || ' ' || cipher from pg_stat_ssl where pid = pg_backend_pid()"
if out=$(timeout 20 psql -X "$conn sslmode=disable" -At -c 'select 1' 2>&1); then
	echo "FAIL: PostgreSQL accepted a connection without TLS"
	exit 1
fi
case $out in
*"no encryption"*) echo "refused without TLS: ok" ;;
*) echo "FAIL: unexpected error without TLS: $out"; exit 1 ;;
esac
EOF

cat >"$work/valkey.sh" <<'EOF'
set -eu
# Use OpenSSL for the actual command connection: redis-cli's --sni does not
# verify hostnames. No application bytes leave before certificate verification.
tls() {
	timeout 10 openssl s_client -quiet -ignore_unexpected_eof \
		-connect "$REDIS_HOST:6379" -servername "$REDIS_HOST" \
		-CAfile /check/amazon.pem -verify_hostname "$REDIS_HOST" -verify_return_error 2>/dev/null
}
expect() {
	if [ "$2" != "$3" ]; then
		echo "FAIL: $1"
		exit 1
	fi
	echo "$1: ok"
}
# The generated token is validated as 64 alphanumeric characters before the
# pod is created, so it is safe in an inline RESP command. printf is a shell
# builtin; the token goes only to stdin, never a process argument or diagnostic.
token=$REDISCLI_AUTH
unset REDISCLI_AUTH
if ! out=$({
	printf 'AUTH %s\r\n' "$token"
	printf 'PING\r\nEVAL "return '\''ok'\''" 0\r\nINFO server\r\nQUIT\r\n'
} | tls); then
	echo "FAIL: verified Valkey TLS connection"
	exit 1
fi
out=$(printf '%s\n' "$out" | tr -d '\r')
expect "AUTH over verified TLS" "$(printf '%s\n' "$out" | sed -n '1p')" +OK
expect "PING over verified TLS" "$(printf '%s\n' "$out" | sed -n '2p')" +PONG
# A Lua string is a RESP bulk string, not a +status reply.
expect "Lua reply type and length" "$(printf '%s\n' "$out" | sed -n '3p')" '$2'
expect "Lua script" "$(printf '%s\n' "$out" | sed -n '4p')" ok
printf '%s\n' "$out" | grep -E '^(valkey_version|redis_version):[0-9]+\.[0-9]+\.[0-9]+$'
expect "complete command response" "$(printf '%s\n' "$out" | tail -n 1)" +OK
if ! out=$(printf 'PING\r\nQUIT\r\n' | tls); then
	echo "FAIL: unauthenticated Valkey TLS connection"
	exit 1
fi
case $out in
-NOAUTH\ *) echo "refused without AUTH: ok" ;;
*) echo "FAIL: Valkey did not reject missing AUTH"; exit 1 ;;
esac
EOF

cat >"$work/valkey-plaintext.sh" <<'EOF'
set -eu
status=0
# Never send the token on the intentionally unencrypted connection.
out=$(env -u REDISCLI_AUTH timeout 10 redis-cli -h "$REDIS_HOST" -p 6379 PING 2>&1) || status=$?
case $status in
124) echo "no plaintext response within 10 seconds: ok" ;;
1) case $out in
    *"Connection reset by peer"*|*"Server closed the connection"*) echo "refused without TLS: ok" ;;
    *) echo "FAIL: unexpected plaintext connection error"; exit 1 ;;
   esac ;;
*) echo "FAIL: Valkey did not refuse the plaintext connection"; exit 1 ;;
esac
EOF

kubectl --context "$context" --request-timeout=15s create namespace "$namespace" >/dev/null
created_namespace=true
"${kubectl[@]}" create configmap store-check \
	--from-file="$work/rds.pem" --from-file="$work/amazon.pem" \
	--from-file="$work/postgres.sh" --from-file="$work/valkey.sh" \
	--from-file="$work/valkey-plaintext.sh" >/dev/null

aws secretsmanager get-secret-value --region "$region" --secret-id "$db_secret" \
	--query SecretString --output text >"$work/database.json"
aws secretsmanager get-secret-value --region "$region" --secret-id "$redis_secret" \
	--query SecretString --output text >"$work/redis-token"
jq -e --rawfile token "$work/redis-token" '
	if (.username | type) != "string" or (.password | type) != "string" or
	   (.username | length) == 0 or (.password | length) == 0 or
	   ($token | sub("\\n$"; "") | test("^[A-Za-z0-9]{64}$") | not)
	then error("invalid store credentials") else {
	apiVersion: "v1", kind: "Secret", type: "Opaque",
	metadata: {name: "store-check"},
	stringData: {PGUSER: .username, PGPASSWORD: .password, REDISCLI_AUTH: ($token | sub("\\n$"; ""))}
} end' "$work/database.json" | "${kubectl[@]}" create -f - >/dev/null

# The same images as kind and CI, pulled through the NAT gateway.
"${kubectl[@]}" apply -f - >/dev/null <<EOF
apiVersion: v1
kind: Pod
metadata:
  name: store-check
spec:
  restartPolicy: Never
  activeDeadlineSeconds: 120
  automountServiceAccountToken: false
  securityContext:
    runAsNonRoot: true
    runAsUser: 999
    runAsGroup: 999
    seccompProfile:
      type: RuntimeDefault
  containers:
    - name: postgres
      image: postgres:18
      command: [sh, /check/postgres.sh]
      env:
        - name: DB_HOST
          value: "$db_host"
        - name: DB_NAME
          value: "$db_name"
        - name: PGUSER
          valueFrom:
            secretKeyRef:
              name: store-check
              key: PGUSER
        - name: PGPASSWORD
          valueFrom:
            secretKeyRef:
              name: store-check
              key: PGPASSWORD
      volumeMounts:
        - name: check
          mountPath: /check
          readOnly: true
      securityContext: &restricted
        allowPrivilegeEscalation: false
        readOnlyRootFilesystem: true
        capabilities:
          drop: [ALL]
    - name: valkey
      image: postgres:18
      command: [sh, /check/valkey.sh]
      env:
        - name: REDIS_HOST
          value: "$redis_host"
        - name: REDISCLI_AUTH
          valueFrom:
            secretKeyRef:
              name: store-check
              key: REDISCLI_AUTH
      volumeMounts:
        - name: check
          mountPath: /check
          readOnly: true
      securityContext: *restricted
    - name: valkey-plaintext
      image: redis:8
      command: [sh, /check/valkey-plaintext.sh]
      env:
        - name: REDIS_HOST
          value: "$redis_host"
      volumeMounts:
        - name: check
          mountPath: /check
          readOnly: true
      securityContext: *restricted
  volumes:
    - name: check
      configMap:
        name: store-check
EOF

phase=
for _ in $(seq 1 90); do
	phase=$("${kubectl[@]}" get pod store-check -o jsonpath='{.status.phase}')
	case $phase in Succeeded | Failed) break ;; esac
	sleep 2
done
for container in postgres valkey valkey-plaintext; do
	echo "--- $container"
	"${kubectl[@]}" logs store-check -c "$container" || true
done
if [[ $phase != Succeeded ]]; then
	echo "store check failed (pod phase: ${phase:-unknown})" >&2
	exit 1
fi
echo "Store checks passed: verified TLS, authenticated connections, and rejection checks."
