#!/usr/bin/env bash
# Deploys the gateway to the platform's EKS cluster (make aws-deploy, after
# make aws-up):
# 1. resolves the GHCR images of a commit on main to their digests;
# 2. fills empty Secrets Manager containers with this session's demo values:
#    synthetic provider keys, a random database password for gateway_app, and
#    three random client keys, whose plaintext stays only in deploy/aws/session/
#    for make aws-check and aws-demo; a secret that already has a value is
#    never replaced;
# 3. installs Fluent Bit (fluent-bit.yaml), which ships the gateway's log
#    lines to CloudWatch Logs, and checks it received Pod Identity credentials;
# 4. installs External Secrets Operator, the RDS CA ConfigMap, and the
#    ExternalSecrets, and waits until every Secret is synced;
# 5. creates or updates the gateway's database role (database-role.yaml);
# 6. installs or upgrades the chart, whose hook runs the migrations.
# 7. creates HTTPS ingress, ensures ALB readiness gates, and publishes DNS.
# AWS_IMAGE_SHA selects the commit; by default it is the newest commit of
# origin/main this branch contains. Needs aws, terraform, kubectl, helm,
# docker (buildx), jq, curl, openssl, and git; run from the repository root.
set -euo pipefail
umask 077

context=${AWS_KUBE_CONTEXT:-llm-gateway-aws}
namespace=llm-gateway
release=llm-gateway
region=eu-north-1
platform=deploy/aws/platform
session=${AWS_SESSION_DIR:-deploy/aws/session}
registry=ghcr.io/ipekutku
# Pinned by version and digest. The namespace and service account must match
# the Pod Identity association in deploy/aws/platform/external-secrets.tf.
eso_chart=oci://ghcr.io/external-secrets/charts/external-secrets@sha256:4a421e51d49160291b4765a42c6326355ee830cb1082c83ff2f62ebe0818265b
eso_version=2.12.0
eso_namespace=external-secrets
eso_service_account=external-secrets
work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT
k=(kubectl --context "$context")
kn=(kubectl --context "$context" --namespace "$namespace")

fail() {
	echo "$*" >&2
	exit 1
}

# Stop before any cluster/secret changes if DNS or issuance is incomplete.
bash deploy/aws/ingress.sh preflight

# 1. Images: a commit SHA tag on GHCR, pinned to the digest it names now.
sha=${AWS_IMAGE_SHA:-$(git merge-base HEAD origin/main)}
[[ $sha =~ ^[0-9a-f]{40}$ ]] || fail "AWS_IMAGE_SHA must be a full 40-character commit SHA"
# Only commits on main are deployed: CI publishes them after every job passed.
git merge-base --is-ancestor "$sha" origin/main 2>/dev/null ||
	fail "$sha is not on origin/main; fetch origin, or choose a commit merged to main."
digest() {
	local ref=$registry/$1:$sha digest
	if ! digest=$(docker buildx imagetools inspect --format '{{json .Manifest}}' "$ref" | jq -er .digest) ||
		[[ ! $digest =~ ^sha256:[0-9a-f]{64}$ ]]; then
		fail "Cannot resolve $ref. CI publishes images only for commits merged to main; fetch origin or set AWS_IMAGE_SHA."
	fi
	echo "$digest"
}
gateway_image="$registry/llm-gateway:$sha@$(digest llm-gateway)"
fakeprovider_image="$registry/llm-gateway-fakeprovider:$sha@$(digest llm-gateway-fakeprovider)"
echo "gateway image: $gateway_image"
echo "fake provider image: $fakeprovider_image"

output() { terraform -chdir="$platform" output -raw "$1"; }
db_host=$(output database_address)
db_name=$(output database_name)
db_secret=$(output database_master_secret_arn)
redis_host=$(output redis_address)
log_group=$(output application_log_group)
[[ $db_host =~ ^[a-z0-9.-]+$ && $redis_host =~ ^[a-z0-9.-]+$ && $db_name =~ ^[a-z_][a-z0-9_]*$ &&
	$db_secret =~ ^arn:aws:secretsmanager:$region:[0-9]{12}:secret:[A-Za-z0-9/_+=.@!-]+$ &&
	$log_group =~ ^/[A-Za-z0-9/_.-]+$ ]] ||
	fail "Unexpected Terraform outputs; run make aws-up first."

# 2. Session secrets. Values go through private files, never arguments.
has_value() {
	local versions
	versions=$(aws secretsmanager describe-secret --region "$region" --secret-id "$1" \
		--query 'length(VersionIdsToStages || `{}`)' --output text)
	[[ $versions != 0 ]]
}
put() {
	aws secretsmanager put-secret-value --region "$region" --secret-id "$1" \
		--secret-string "file://$2" --query Name --output text >/dev/null
	echo "filled $1"
}
for provider in openai anthropic; do
	if ! has_value "llm-gateway/$provider-api-key"; then
		# The fake provider ignores the key; it is not a secret.
		printf 'synthetic-aws-%s-key' "$provider" >"$work/$provider"
		put "llm-gateway/$provider-api-key" "$work/$provider"
	fi
done
if ! has_value llm-gateway/database-app-password; then
	openssl rand -hex 32 | tr -d '\n' >"$work/app-password"
	put llm-gateway/database-app-password "$work/app-password"
fi
if ! has_value llm-gateway/clients; then
	# demo answers the checks; limited has bursts of 10 and refills one
	# request every 10 seconds, so the shared-limit check is exact; load
	# carries make aws-demo's traffic, up to 500 requests/s.
	for client in demo limited load; do
		printf 'aws-%s-%s' "$client" "$(openssl rand -hex 24)" >"$work/$client-key"
	done
	hash() { openssl dgst -sha256 -r <"$work/$1-key" | cut -d' ' -f1; }
	jq -n --arg demo "$(hash demo)" --arg limited "$(hash limited)" --arg load "$(hash load)" '{clients: [
		{id: "demo", key_sha256: $demo, requests_per_minute: 600, burst: 20, max_concurrent: 10},
		{id: "limited", key_sha256: $limited, requests_per_minute: 6, burst: 10, max_concurrent: 5},
		{id: "load", key_sha256: $load, requests_per_minute: 30000, burst: 1000, max_concurrent: 400}
	]}' >"$work/clients.json"
	put llm-gateway/clients "$work/clients.json"
	mkdir -p "$session"
	mv "$work/demo-key" "$work/limited-key" "$work/load-key" "$session/"
	echo "client keys for make aws-check and aws-demo: $session/{demo,limited,load}-key"
elif [[ ! -f $session/demo-key || ! -f $session/limited-key || ! -f $session/load-key ]]; then
	echo "note: llm-gateway/clients already has a value, but $session lacks client keys; make aws-check and aws-demo need them"
fi

# 3. Fluent Bit, before the gateway's pods and Jobs start. The hash covers
# the whole manifest, so a changed configuration restarts its pods.
logging=$(<deploy/aws/fluent-bit.yaml)
logging=${logging//__LOG_GROUP__/"$log_group"}
logging=${logging//__MANIFEST_SHA256__/"$(openssl dgst -sha256 -r <<<"$logging" | cut -c1-64)"}
[[ ! $logging =~ __[A-Z0-9_]+__ ]] || fail "unreplaced placeholder in deploy/aws/fluent-bit.yaml"
printf '%s\n' "$logging" >"$work/fluent-bit.yaml"
"${k[@]}" apply -f "$work/fluent-bit.yaml"
"${k[@]}" --namespace fluent-bit rollout status daemonset/fluent-bit --timeout=180s
# Pod Identity adds its credentials to pods created after the association.
"${k[@]}" --namespace fluent-bit get pods --selector app.kubernetes.io/name=fluent-bit -o json | jq -e '
	[.items[] | select(.metadata.deletionTimestamp == null)] | length > 0 and
	all(.[]; any(.spec.containers[0].env[]?; .name == "AWS_CONTAINER_CREDENTIALS_FULL_URI"))' >/dev/null ||
	fail "Fluent Bit pods have no Pod Identity credentials; restart them with kubectl -n fluent-bit rollout restart daemonset/fluent-bit"

# 4. External Secrets Operator and the Secrets it keeps in sync.
helm --kube-context "$context" upgrade --install external-secrets "$eso_chart" --version "$eso_version" \
	--namespace "$eso_namespace" --create-namespace --set serviceAccount.name="$eso_service_account" \
	--wait --timeout 5m
"${k[@]}" create namespace "$namespace" --dry-run=client -o yaml | "${k[@]}" apply -f -
curl -fsS --proto '=https' --connect-timeout 10 --max-time 30 -o "$work/ca.pem" \
	"https://truststore.pki.rds.amazonaws.com/$region/$region-bundle.pem"
"${kn[@]}" create configmap llm-gateway-database-ca --from-file=ca.pem="$work/ca.pem" \
	--dry-run=client -o yaml | "${kn[@]}" apply -f -

# The placeholders become Terraform outputs, checked above.
secrets=$(<deploy/aws/secrets.yaml)
secrets=${secrets//__DATABASE_HOST__/"$db_host"}
secrets=${secrets//__DATABASE_NAME__/"$db_name"}
secrets=${secrets//__DATABASE_SECRET_ARN__/"$db_secret"}
secrets=${secrets//__REDIS_HOST__/"$redis_host"}
[[ ! $secrets =~ __[A-Z_]+__ ]] || fail "unreplaced placeholder in deploy/aws/secrets.yaml"
printf '%s\n' "$secrets" >"$work/secrets.yaml"
# The operator's admission webhook can refuse requests for a few seconds after
# its pods are ready, until its certificate is in place.
for attempt in $(seq 1 12); do
	"${kn[@]}" apply -f "$work/secrets.yaml" && break
	((attempt < 12)) || fail "Could not apply the External Secrets."
	sleep 5
done
if ! "${kn[@]}" wait --for=condition=Ready externalsecret --all --timeout=180s; then
	"${kn[@]}" get externalsecret >&2
	fail "External Secrets did not sync; see kubectl describe externalsecret in namespace $namespace."
fi

# 5. The gateway's database role, before the chart starts pods that use it.
"${kn[@]}" delete job llm-gateway-database-role --ignore-not-found --wait=true >/dev/null
"${kn[@]}" apply -f deploy/aws/database-role.yaml
status=
for _ in $(seq 1 100); do
	status=$("${kn[@]}" get job llm-gateway-database-role \
		-o jsonpath='{range .status.conditions[?(@.status=="True")]}{.type}{" "}{end}')
	case $status in *Complete* | *Failed*) break ;; esac
	sleep 2
done
"${kn[@]}" logs job/llm-gateway-database-role || true
[[ $status == *Complete* ]] || fail "The database role Job did not complete (conditions: ${status:-none})."

# 6. The chart; its pre-install/pre-upgrade hook migrates as the master user.
# Install the controller and label the namespace before any gateway rollout.
bash deploy/aws/ingress.sh prepare
helm --kube-context "$context" upgrade --install "$release" deploy/helm/llm-gateway \
	--namespace "$namespace" -f deploy/aws/values.yaml \
	--set image.tag="$sha@${gateway_image##*@}" --set "sidecars[0].image=$fakeprovider_image" \
	--wait --timeout 5m
bash deploy/aws/ingress.sh publish
"${kn[@]}" get pods -o wide
echo "Deployed $sha. Check it with make aws-check."
