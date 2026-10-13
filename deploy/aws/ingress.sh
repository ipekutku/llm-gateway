#!/usr/bin/env bash
# Session ingress lifecycle. Called by deploy.sh and make aws-down; never
# destroys the persistent zone or certificate. Run from the repository root.
set -euo pipefail
umask 077

region=eu-north-1
context=${AWS_KUBE_CONTEXT:-llm-gateway-aws}
domain=${TF_VAR_domain_name:-llm-gateway.utkuipek.dev}
namespace=llm-gateway
name=llm-gateway
controller_namespace=kube-system
controller_service_account=aws-load-balancer-controller
controller_version=3.6.0
controller_chart_sha256=ee0403fcd713d6f1d28b1ff17d0d6d47e011cc4dd727a0aed0d95ae46897226a
work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT
kn=(kubectl --context "$context" --namespace "$namespace")
selector=app.kubernetes.io/name=llm-gateway,app.kubernetes.io/instance=llm-gateway

fail() { echo "$*" >&2; exit 1; }
aw() { aws "$@" --region "$region" --output json --cli-connect-timeout 10 --cli-read-timeout 30; }
[[ ${#domain} -le 253 && $domain == *.* ]] || fail "Invalid gateway domain_name"
IFS=. read -r -a labels <<<"$domain"
for label in "${labels[@]}"; do
	[[ ${#label} -le 63 && $label =~ ^[a-z0-9]([a-z0-9-]*[a-z0-9])?$ ]] || fail "Invalid gateway domain_name"
done
[[ $domain != *. ]] || fail "Invalid gateway domain_name"

zone() {
	aw route53 list-hosted-zones-by-name --dns-name "$domain" | jq -er --arg domain "$domain." '
		[.HostedZones[] | select(.Name == $domain and .Config.PrivateZone == false)] |
		if length == 0 then "" elif length == 1 then .[0].Id | split("/") | last
		else error("multiple public gateway zones") end'
}

certificate() {
	local arn
	arn=$(aw acm list-certificates | jq -er --arg domain "$domain" '
		[.CertificateSummaryList[] | select(.DomainName == $domain)] |
		if length == 1 then .[0].CertificateArn
		else error("expected one gateway certificate; check bootstrap and ACM") end')
	aw acm describe-certificate --certificate-arn "$arn"
}

check_account() {
	local identity
	[[ ${TF_VAR_aws_account_id:-} =~ ^[0-9]{12}$ ]] || fail "Set TF_VAR_aws_account_id"
	identity=$(aw sts get-caller-identity)
	[[ $(jq -r .Account <<<"$identity") == "$TF_VAR_aws_account_id" ]] || fail "AWS credentials belong to a different account"
}

preflight() {
	local cert
	check_account
	zone_id=$(zone)
	[[ -n $zone_id ]] || fail "Gateway hosted zone is missing; apply bootstrap and delegate it first"
	cert=$(certificate)
	jq -e --arg domain "$domain" '.Certificate.DomainName == $domain and .Certificate.Status == "ISSUED"' <<<"$cert" >/dev/null ||
		fail "Gateway certificate is not ISSUED; finish DNS delegation and validation first"
	certificate_arn=$(jq -er .Certificate.CertificateArn <<<"$cert")
}

load_balancer() {
	aw elbv2 describe-load-balancers | jq -c --arg name "$name" '
		[.LoadBalancers[] | select(.LoadBalancerName == $name)] |
		if length == 0 then null elif length == 1 then .[0] else error("multiple gateway load balancers") end' || {
		fail "Could not inspect gateway load balancer"
	}
}

# Resource ownership is checked before DNS mutation or teardown. The IAM policy
# is upstream's, so default/controller tags also define the cleanup boundary.
check_load_balancer() {
	local lb=$1 tags
	[[ $lb != null ]] || return 0
	tags=$(aw elbv2 describe-tags --resource-arns "$(jq -er .LoadBalancerArn <<<"$lb")")
	jq -e '.TagDescriptions[0].Tags | from_entries |
		.Project == "llm-gateway" and .Stack == "platform" and
		.["elbv2.k8s.aws/cluster"] == "llm-gateway" and
		.["ingress.k8s.aws/stack"] == "llm-gateway/llm-gateway"' <<<"$tags" >/dev/null ||
		fail "Load balancer llm-gateway has unexpected ownership tags; refusing to change DNS or tear it down"
}

gateway_records() {
	aw route53 list-resource-record-sets --hosted-zone-id "$zone_id" |
		jq -c --arg name "$domain." '[.ResourceRecordSets[] | select(.Name == $name and (.Type == "A" or .Type == "AAAA"))]'
}

check_alias() {
	# The script creates only an IPv4 alias to its fixed-name regional ALB.
	# This also recognizes a stale alias after its ALB was already removed.
	jq -e 'all(.[]; .Type == "A" and .AliasTarget != null and
		(.AliasTarget.DNSName | test("^dualstack\\.llm-gateway-[0-9]+\\.eu-north-1\\.elb\\.amazonaws\\.com\\.?$")))' <<<"$1" >/dev/null ||
		fail "Gateway DNS has an unexpected A/AAAA record; refusing to overwrite or delete it"
}

change_records() {
	local action=$1 records=$2 change
	jq -n --arg action "$action" --argjson records "$records" \
		'{Changes: [$records[] | {Action: $action, ResourceRecordSet: .}]}' >"$work/dns.json"
	change=$(aw route53 change-resource-record-sets --hosted-zone-id "$zone_id" --change-batch "file://$work/dns.json")
	aws route53 wait resource-record-sets-changed --id "$(jq -er .ChangeInfo.Id <<<"$change")" \
		--region "$region" --cli-connect-timeout 10 --cli-read-timeout 30
}

case ${1:-} in
preflight)
	preflight
	echo "ok: gateway zone and issued certificate in $region"
	;;
prepare)
	preflight
	vpc=$(terraform -chdir=deploy/aws/platform output -raw vpc_id)
	[[ $vpc =~ ^vpc-[0-9a-f]+$ ]] || fail "Invalid platform VPC output"
	curl -fsS --proto '=https' --connect-timeout 10 --max-time 60 \
		"https://aws.github.io/eks-charts/aws-load-balancer-controller-$controller_version.tgz" -o "$work/controller.tgz"
	[[ $(shasum -a 256 "$work/controller.tgz" | cut -d' ' -f1) == "$controller_chart_sha256" ]] || fail "Controller chart checksum mismatch"
	helm --kube-context "$context" upgrade --install aws-load-balancer-controller "$work/controller.tgz" \
		--namespace "$controller_namespace" -f deploy/aws/load-balancer-controller-values.yaml \
		--set clusterName=llm-gateway --set vpcId="$vpc" --set serviceAccount.name="$controller_service_account" \
		--wait --timeout 5m
	kubectl --context "$context" label namespace "$namespace" elbv2.k8s.aws/pod-readiness-gate-inject=enabled --overwrite
	;;
publish)
	preflight
	cidr=$(terraform -chdir=deploy/aws/platform output -raw admin_cidr)
	[[ $cidr =~ ^([0-9]{1,3}\.){3}[0-9]{1,3}/(2[4-9]|3[0-2])$ ]] || fail "Invalid platform admin_cidr output"
	ingress=$(<deploy/aws/ingress.yaml)
	ingress=${ingress//__DOMAIN_NAME__/"$domain"}
	ingress=${ingress//__CERTIFICATE_ARN__/"$certificate_arn"}
	ingress=${ingress//__ADMIN_CIDR__/"$cidr"}
	[[ ! $ingress =~ __[A-Z_]+__ ]] || fail "Unreplaced ingress placeholder"
	printf '%s\n' "$ingress" >"$work/ingress.yaml"
	"${kn[@]}" apply -f "$work/ingress.yaml"
	# Wait for a matching binding before recreating the initial pods. A namespace
	# label alone does not inject gates when the Service/binding does not exist.
	deadline=$((SECONDS + 300))
	while ! "${kn[@]}" get targetgroupbindings -o json | jq -e \
		'any(.items[]; .spec.serviceRef.name == "llm-gateway" and .spec.targetType == "ip")' >/dev/null; do
		((SECONDS < deadline)) || fail "No gateway target-group binding after 5m; inspect the controller logs"
		sleep 5
	done
	if ! "${kn[@]}" get pods -l "$selector" -o json | jq -e '
		[.items[] | select(.metadata.deletionTimestamp == null)] |
		length >= 2 and all(.[]; any(.spec.readinessGates[]?; .conditionType | startswith("target-health.elbv2.k8s.aws/")))' >/dev/null; then
		"${kn[@]}" rollout restart deployment/llm-gateway
	fi
	"${kn[@]}" rollout status deployment/llm-gateway --timeout=300s
	"${kn[@]}" get pods -l "$selector" -o json | jq -e '
		[.items[] | select(.metadata.deletionTimestamp == null)] |
		length >= 2 and all(.[]; any(.spec.readinessGates[]?; .conditionType | startswith("target-health.elbv2.k8s.aws/")))' >/dev/null ||
		fail "Gateway pods lack ALB readiness gates; DNS was not published"
	lb=$(load_balancer)
	[[ $lb != null ]] || fail "Gateway ALB is missing"
	check_load_balancer "$lb"
	address=$("${kn[@]}" get ingress "$name" -o jsonpath='{.status.loadBalancer.ingress[0].hostname}')
	[[ $address == "$(jq -er .DNSName <<<"$lb")" ]] || fail "Ingress address does not match the gateway ALB"
	records=$(gateway_records)
	check_alias "$records"
	record=$(jq -cn --arg name "$domain." --arg dns "dualstack.$address." --arg zone "$(jq -er .CanonicalHostedZoneId <<<"$lb")" \
		'[{Name: $name, Type: "A", AliasTarget: {DNSName: $dns, HostedZoneId: $zone, EvaluateTargetHealth: false}}]')
	change_records UPSERT "$record"
	echo "Gateway HTTPS endpoint: https://$domain (allowed CIDR: $cidr)"
	;;
down)
	# No certificate/issued-status dependency: teardown must work after expiry.
	check_account
	zone_id=$(zone)
	lb=$(load_balancer)
	check_load_balancer "$lb"
	if [[ -n $zone_id ]]; then
		records=$(gateway_records)
		check_alias "$records"
		if [[ $(jq length <<<"$records") != 0 ]]; then
			change_records DELETE "$records"
			echo "Removed the session DNS alias"
		fi
	fi
	clusters=$(aw eks list-clusters)
	if jq -e '.clusters | index("llm-gateway") != null' <<<"$clusters" >/dev/null; then
		ingress=$("${kn[@]}" get ingress "$name" --ignore-not-found -o json)
		if [[ -n $ingress ]]; then
			jq -e --arg domain "$domain" '.spec.rules[0].host == $domain and
				.metadata.annotations["alb.ingress.kubernetes.io/load-balancer-name"] == "llm-gateway"' <<<"$ingress" >/dev/null ||
				fail "Ingress llm-gateway has unexpected ownership; refusing teardown"
			"${kn[@]}" delete ingress "$name" --wait=true --timeout=300s
		fi
	elif [[ $lb != null ]]; then
		fail "Gateway ALB remains but its cluster is absent; restore controller cleanup before destroying the VPC"
	fi
	deadline=$((SECONDS + 300))
	while :; do
		lb=$(load_balancer)
		groups=$(aw ec2 describe-security-groups --filters \
			Name=tag:elbv2.k8s.aws/cluster,Values=llm-gateway Name=tag:Project,Values=llm-gateway Name=tag:Stack,Values=platform)
		[[ $lb != null || $(jq '.SecurityGroups | length' <<<"$groups") != 0 ]] || break
		((SECONDS < deadline)) || fail "Controller ALB/security groups remain after 5m; Terraform destroy was not started"
		sleep 5
	done
	echo "ok: session ingress removed; bootstrap DNS and certificate kept"
	;;
records)
	# Read-only leftover inventory, excluding exactly the apex NS/SOA and the
	# certificate's own DNS validation record. Extra CNAME/NS records stay visible.
	zone_id=$(zone)
	if [[ -z $zone_id ]]; then echo '[]'; exit; fi
	cert=$(certificate)
	validation=$(jq -c --arg domain "$domain" '[.Certificate.DomainValidationOptions[] |
		select(.DomainName == $domain) | .ResourceRecord]' <<<"$cert")
	aw route53 list-resource-record-sets --hosted-zone-id "$zone_id" | jq -c \
		--arg domain "$domain." --argjson validation "$validation" '
		[.ResourceRecordSets[] | select(
			(.Name == $domain and (.Type == "NS" or .Type == "SOA")) == false) |
		select(. as $record | any($validation[];
			(.Name | rtrimstr(".")) == ($record.Name | rtrimstr(".")) and .Type == $record.Type and
			($record.ResourceRecords | map(.Value | rtrimstr("."))) == [(.Value | rtrimstr("."))]) | not)]'
	;;
*) fail "Usage: bash deploy/aws/ingress.sh preflight|prepare|publish|down|records" ;;
esac
