#!/usr/bin/env bash
# Lists resources in the project Region that bill while they exist, whoever
# created them: Terraform, a Kubernetes controller, or the console. Read-only.
# Exits 1 if any session resource or extra DNS record is found.
# Persistent bootstrap resources are not listed; extra records
# in the gateway zone are, including an alias left pointing at a deleted ALB.
set -euo pipefail

region=eu-north-1
found=0

# check LABEL AWS-ARGS...: prints what the query returns, or "none".
check() {
	local label=$1
	shift
	local result
	result=$(aws --region "$region" --output text "$@")
	result=$(tr -s '\t\n' '  ' <<<"$result")
	result=${result% }
	if [[ -n "$result" && "$result" != "None" ]]; then
		printf '%-24s %s\n' "$label" "$result"
		found=1
	else
		printf '%-24s none\n' "$label"
	fi
}

check "EKS clusters" eks list-clusters --query 'clusters[]'
check "EC2 instances" ec2 describe-instances \
	--filters Name=instance-state-name,Values=pending,running,stopping,stopped \
	--query 'Reservations[].Instances[].InstanceId'
check "EBS volumes" ec2 describe-volumes --query 'Volumes[].VolumeId'
check "NAT gateways" ec2 describe-nat-gateways \
	--filter Name=state,Values=pending,available,deleting \
	--query 'NatGateways[].NatGatewayId'
check "Elastic IPs" ec2 describe-addresses --query 'Addresses[].PublicIp'
check "Load balancers" elbv2 describe-load-balancers --query 'LoadBalancers[].LoadBalancerName'
check "Classic load balancers" elb describe-load-balancers --query 'LoadBalancerDescriptions[].LoadBalancerName'
check "RDS instances" rds describe-db-instances --query 'DBInstances[].DBInstanceIdentifier'
check "RDS manual snapshots" rds describe-db-snapshots --snapshot-type manual \
	--query 'DBSnapshots[].DBSnapshotIdentifier'
check "RDS automated backups" rds describe-db-instance-automated-backups \
	--query 'DBInstanceAutomatedBackups[].DBInstanceIdentifier'
check "ElastiCache clusters" elasticache describe-cache-clusters --query 'CacheClusters[].CacheClusterId'
check "ElastiCache snapshots" elasticache describe-snapshots --query 'Snapshots[].SnapshotName'
# Billed per secret per month, prorated. Secrets pending deletion are not listed.
check "Secrets" secretsmanager list-secrets --query 'SecretList[].Name'
# Stored log events bill per GB-month. The platform creates its group only
# in this Region, so us-east-1 is not searched.
check "Log groups" logs describe-log-groups --query 'logGroups[].logGroupName'
# A VPC costs nothing, but one left over means a destroy did not finish.
check "Project VPCs" ec2 describe-vpcs \
	--filters Name=tag:Project,Values=llm-gateway --query 'Vpcs[].VpcId'

records=$(bash deploy/aws/ingress.sh records)
if [[ $(jq length <<<"$records") != 0 ]]; then
	printf '%-24s %s\n' "Gateway DNS records" "$records"
	found=1
else
	printf '%-24s none\n' "Gateway DNS records"
fi

if ((found)); then
	echo "Session resources or DNS records remain; run make aws-down, or inspect what Terraform does not manage."
	exit 1
fi
echo "No session resources or extra gateway DNS records remain in $region; bootstrap is kept."
