# AWS runbook

Milestone 10 has two Terraform root modules. The persistent bootstrap in
[`deploy/aws/bootstrap`](../deploy/aws/bootstrap) creates a state bucket and two
private image repositories. The [platform](#platform-sessions) in
[`deploy/aws/platform`](../deploy/aws/platform) creates the network, an EKS
cluster, RDS PostgreSQL, Valkey, and secrets for a working session, then is
destroyed. [`make aws-deploy`](#deploy-and-check-the-gateway) installs the
gateway on that cluster for the session.
Bootstrap also optionally keeps the gateway's [DNS zone and certificate
request](#gateway-dns-and-certificate), independently of the platform.
The roadmap defines the remaining [milestone scope](ROADMAP.md#milestone-10--aws-infrastructure).

## Setup and costs

Use AWS CLI v2, Terraform **1.16.5**, Docker with Buildx, and `jq`. The AWS provider
is pinned to **6.68.0**. Kubernetes work needs `kubectl`, Helm 4.3.0, `curl`, and OpenSSL.
The current AWS project permits only **eu-north-1**, which is fixed in the provider
and the backend template. Do not substitute another Region from a pricing example.

Before applying anything, secure the root account with MFA and no access keys,
and configure the account's cost safety net: a monthly budget with email alerts
at $5, $10, and $15 actual spend and $15 forecast, excluding credits and refunds
so usage remains visible while credits cover the bill. Verify the email subscription.
The working budget for this demonstration is $10–20 out of pocket; credits and
account restrictions must be checked in Billing rather than assumed.

Bootstrap resources remain between sessions. They are metered, not guaranteed
free: [S3 charges](https://aws.amazon.com/s3/pricing/) cover stored state versions
and requests, and [ECR charges](https://aws.amazon.com/ecr/pricing/) cover image
storage and applicable transfer. SSE-S3/AES256 avoids a customer-managed KMS key.
Basic ECR scanning has no separate charge; enhanced Inspector scanning does.
The repositories expire images beyond the newest 10 in each, counting tagged
and untagged images (manifests, not releases). Older deployed or rollback images
can therefore disappear;
the policy is for this demonstration, not a production retention guarantee.
State versions are retained without automatic expiration, to allow recovery.

The platform holds the hourly-cost resources: EKS, two nodes, a NAT gateway,
RDS, and ElastiCache, plus the ALB created by deployment. Create it only for working
sessions, then remove it and check for leftovers (see [Platform sessions](#platform-sessions)).
Session teardown keeps bootstrap.
With the gateway DNS zone enabled, bootstrap also incurs the
[Route 53 hosted-zone charge](https://aws.amazon.com/route53/pricing/): $0.50 per
month for one of the first 25 zones, plus applicable DNS query charges. The zone
charge starts at creation and is not prorated. The certificate has export disabled;
[ACM public certificates used with an ALB](https://aws.amazon.com/certificate-manager/pricing/)
have no separate certificate charge. Domain registration is paid to the outside
registrar separately.

Authenticate with short-lived credentials and confirm the target account:

```bash
aws login --profile llm-gateway
export AWS_PROFILE=llm-gateway
export AWS_REGION=eu-north-1
aws sts get-caller-identity
export TF_VAR_aws_account_id="$(aws sts get-caller-identity --query Account --output text)"
```

Compare that account ID with the intended project before continuing. Terraform
requires it and rejects credentials belonging to a different account. Credentials
come from the AWS credential chain; never put keys in Terraform files, backend
arguments, GitHub secrets, or Git. Renew the login if it expires.

The current project's restrictions prohibit IAM identity providers and Spot
instances. Bootstrap therefore adds no identity provider or CI AWS role, and no
CI job uses AWS credentials; see [CI images and AWS credentials](#ci-images-and-aws-credentials).
On the Free plan, EC2 also launches only free-tier-eligible instance types. Check
them before choosing any instance or node type:

```bash
aws ec2 describe-instance-types --region eu-north-1 \
  --filters Name=free-tier-eligible,Values=true \
  --query 'sort_by(InstanceTypes,&InstanceType)[].[InstanceType,MemoryInfo.SizeInMiB]' --output table
```

On 2026-10-12 these were `t3` and `t4g` `micro` and `small`, `c7i-flex.large`, and
`m7i-flex.large`. Another type fails only when its instances launch: an EKS node
group with `t4g.medium` retried for about 33 minutes before failing.

## Create bootstrap with local state

Run from the repository root. The first apply must use local state, since its
bucket does not exist yet. Leave `backend.tf` absent on this first run.

```bash
umask 077
make terraform-check
terraform -chdir=deploy/aws/bootstrap plan -out=bootstrap.tfplan
terraform -chdir=deploy/aws/bootstrap apply bootstrap.tfplan
```

Review the plan before applying: it should create 10 resources, all in the intended
account and Region: six S3 resources (bucket, versioning, encryption, public access
block, ownership controls, and policy), two ECR repositories, and two lifecycle
policies. Taggable resources inherit `Project=llm-gateway` and `Stack=bootstrap`.
There are no VPC, cluster, data store, IAM identity, or secret resources.
This count assumes `domain_name` is empty (the default). Enabling
[gateway DNS](#gateway-dns-and-certificate) adds three resources.

The bucket name is `llm-gateway-tfstate-<account-id>-eu-north-1`. S3 names are
globally unique; if this name is already taken or a repository already exists,
stop and inspect ownership before importing or changing the configuration.
Never delete an existing resource just to get past an apply error.

Local `terraform.tfstate`, its backups, the plan, and `.terraform/` are gitignored.
Treat them as sensitive and keep them until remote migration is verified.
`prevent_destroy` protects the bucket while its resource block remains in the
configuration; `force_destroy=false` also refuses to empty it. Neither substitutes
for protecting the state or its IAM permissions. Do not use bootstrap destroy as
session teardown. Nonempty ECR repositories cannot be force-deleted either.

## Migrate state into its own bucket

After the local apply succeeds:

```bash
STATE_BUCKET="$(terraform -chdir=deploy/aws/bootstrap output -raw state_bucket_name)"
cp deploy/aws/bootstrap/backend.tf.example deploy/aws/bootstrap/backend.tf
terraform -chdir=deploy/aws/bootstrap init -migrate-state \
  -backend-config="bucket=$STATE_BUCKET" \
  -backend-config="allowed_account_ids=[\"$TF_VAR_aws_account_id\"]"
```

Answer Terraform's migration prompt only after checking the source and destination.
The copied, gitignored file selects S3, encryption, and native locking with
`use_lockfile=true`; no DynamoDB table is needed. Bootstrap state lives at
`bootstrap/terraform.tfstate`. The future platform uses a separate
`platform/terraform.tfstate` key in the same bucket. Backend arguments cannot
refer to resource outputs or input variables, which is why this is a second step.
See [HashiCorp's S3 backend documentation](https://developer.hashicorp.com/terraform/language/backend/s3).

Verify migration with the remote object and a fresh plan:

```bash
aws s3api head-object --bucket "$STATE_BUCKET" --key bootstrap/terraform.tfstate \
  --query '{Encryption:ServerSideEncryption,Version:VersionId}'
terraform -chdir=deploy/aws/bootstrap plan
```

Require `AES256`, a version ID, and no planned changes. Keep local backups secure
until this is confirmed; do not commit them. The bucket blocks all public access,
disables ACLs, and denies non-TLS requests by non-service principals. It grants no
access by itself; operator IAM permissions still govern who can read or change state.

A fresh checkout must copy the template again and initialize the existing backend
before planning; **do not apply with empty local state**:

```bash
STATE_BUCKET="llm-gateway-tfstate-${TF_VAR_aws_account_id}-eu-north-1"
cp deploy/aws/bootstrap/backend.tf.example deploy/aws/bootstrap/backend.tf
terraform -chdir=deploy/aws/bootstrap init \
  -backend-config="bucket=$STATE_BUCKET" \
  -backend-config="allowed_account_ids=[\"$TF_VAR_aws_account_id\"]"
terraform -chdir=deploy/aws/bootstrap plan
```

Backend access requires `s3:ListBucket` on the bucket, `s3:GetObject` and
`s3:PutObject` on the selected state key, and `s3:GetObject`, `s3:PutObject`, and
`s3:DeleteObject` on its `.tflock` key. Scope future roles to their own keys. Never
disable locking to bypass a conflict; confirm that no operation is running before
considering `force-unlock`. State recovery uses previous S3 versions; do not
overwrite the current version while another Terraform operation is active.

## Gateway DNS and certificate

This is the persistent part of HTTPS ingress, independent of the platform.
The operator's verification on 2026-10-13 created exactly three resources,
confirmed all four delegation servers through 1.1.1.1 and 8.8.8.8, observed ACM
status `ISSUED`, and finished with a no-change bootstrap plan (exit code 0).
The [session ingress](#https-session-ingress) also has
[live deployment, rollout, and teardown verification](#ingress-verification--2026-10-13).

The hostname is `llm-gateway.utkuipek.dev`. Cloudflare Registrar keeps the parent
`utkuipek.dev` zone; Route 53 hosts only the child zone. The operator adds its
four NS records in Cloudflare once, by hand, following
[AWS's subdomain delegation procedure](https://docs.aws.amazon.com/Route53/latest/DeveloperGuide/MigratingSubdomain.html).
No Cloudflare provider or API token is used.

Before applying, complete the registrar's registrant email verification and
ensure you can add the NS records the same day. Bootstrap requests the certificate
and writes its validation CNAME, then finishes without waiting for issuance, so
the name-server outputs are available for delegation. ACM allows
[72 hours for DNS validation](https://docs.aws.amazon.com/acm/latest/userguide/dns-validation.html).

Use the existing remote bootstrap state, initialized as described above; never
apply against empty local state. Set the optional `domain_name` input to the
hostname itself, without a wildcard or trailing dot:

```bash
export TF_VAR_domain_name=llm-gateway.utkuipek.dev
make terraform-check
terraform -chdir=deploy/aws/bootstrap plan -out=ingress-bootstrap.tfplan
```

Review the saved plan before the next step. Against the existing bootstrap, it
should add exactly three resources: one public hosted zone, one DNS-validated
RSA certificate request in **eu-north-1** (the ALB's Region), and one validation
CNAME in that zone. It should leave S3 and ECR unchanged. There is no A/AAAA alias
or ALB yet. The certificate is non-exportable and uses certificate transparency.

When ready to delegate, apply the reviewed plan and read the outputs:

```bash
terraform -chdir=deploy/aws/bootstrap apply ingress-bootstrap.tfplan
terraform -chdir=deploy/aws/bootstrap output -json gateway_name_servers
terraform -chdir=deploy/aws/bootstrap output -raw gateway_hosted_zone_id
terraform -chdir=deploy/aws/bootstrap output -raw gateway_certificate_arn
```

In the Cloudflare dashboard, open `utkuipek.dev` → DNS → Records. Add four records,
each with Type **NS**, Name **llm-gateway**, TTL **Auto**, and one of the four
name servers from the output as its value. Keep the existing parent-zone name
servers. Add no A, AAAA, CNAME, or DS record for `llm-gateway` in Cloudflare.
The ACM CNAME belongs in the Route 53 child zone and Terraform already creates it.

The parent may remain DNSSEC-signed, while this child delegation is unsigned.
Route 53 signing is deliberately off: its signing key would need to be in
[us-east-1](https://docs.aws.amazon.com/Route53/latest/DeveloperGuide/dns-configuring-dnssec-cmk-requirements.html),
outside the project's permitted Region. Adding a child DS without signing the
child zone would break validating resolvers. If the parent has CAA restrictions,
allow ACM's `amazon.com` issuer.

Check delegation and issuance:

```bash
dig @1.1.1.1 NS llm-gateway.utkuipek.dev +short
dig @8.8.8.8 NS llm-gateway.utkuipek.dev +short
GATEWAY_CERTIFICATE_ARN="$(terraform -chdir=deploy/aws/bootstrap output -raw gateway_certificate_arn)"
aws acm describe-certificate --region eu-north-1 \
  --certificate-arn "$GATEWAY_CERTIFICATE_ARN" \
  --query 'Certificate.{Domain:DomainName,Status:Status,Validation:DomainValidationOptions}'
```

Both DNS replies must match the four output servers. Confirm the validation
CNAME resolves using the record name returned by ACM. Continue with ALB work
only once certificate status is `ISSUED`; an output ARN alone does not prove
issuance. Pending validation can take time after delegation propagates.
If validation times out, fix delegation first, then review a replacement plan:

```bash
terraform -chdir=deploy/aws/bootstrap plan \
  -replace='aws_acm_certificate.gateway[0]' -out=ingress-bootstrap.tfplan
# After review, apply this saved plan as above and check issuance again.
```

An ordinary re-apply does not necessarily replace a timed-out certificate.
Keep `domain_name` set to the same hostname on every later bootstrap plan/apply,
either through the environment or a gitignored `.tfvars` file. The zone has
`prevent_destroy` and `force_destroy=false`, so accidentally omitting or changing
the input cannot remove its delegation. The certificate permits replacement
before destruction. Session teardown keeps the zone, validation CNAME, and
certificate; eligible managed renewals require the certificate to be in use and
the CNAME to remain in place. Without a session alias, A/AAAA queries for the
gateway hostname return no address, even after the certificate is issued.

## HTTPS session ingress

The session uses AWS Load Balancer Controller **v3.6.0**, installed from Helm
chart **3.6.0**, pinned by package SHA-256 and image index digest. Terraform
adds the controller's IAM role, upstream release policy, and EKS Pod Identity
association in `kube-system/aws-load-balancer-controller`. It also tags public
subnets for ALB discovery and passes the VPC ID and Region explicitly, so the
controller never needs pod access to instance metadata. The gateway has no AWS
role. The upstream policy has broad discovery and ELB/EC2 permissions; it is
not limited to Terraform's resource ARNs. Trust is restricted to this account
and cluster, and the association identifies the controller service account.
WAF and Shield integration are disabled in its values.

The ingress in `deploy/aws/ingress.yaml` creates one internet-facing IPv4 ALB
named `llm-gateway`, with only an HTTPS listener on 443, the issued regional ACM
certificate, and TLS 1.2/1.3 (`ELBSecurityPolicy-TLS13-1-2-Res-2021-06`). Its frontend
security group admits only the platform's `admin_cidr`, the same CIDR used for
the Kubernetes API. The controller manages the backend security group and the
node rules for pod targets. TLS terminates at the ALB; traffic from it to pod
port 8080 is plain HTTP inside the VPC. The health check is `/readyz`; dependencies
still never determine readiness. Metrics and pprof are not routed to their port.

Before starting a session, keep the existing AWS login/profile and account
settings from [Setup](#setup-and-costs), and set:

```bash
export TF_VAR_domain_name=llm-gateway.utkuipek.dev
# Use your current public IPv4 address, as /32.
export TF_VAR_admin_cidr="$(curl -fsS https://checkip.amazonaws.com | tr -d '\n')/32"
make aws-up
make aws-deploy
make aws-check
make aws-rollout          # optional: 90s of synthetic traffic and one restart
make aws-down
make aws-leftovers
```

Review the `aws-up` plan before its apply. It includes three new controller
resources (role, inline policy, Pod Identity association) in addition to the
existing platform. Bootstrap remains separate. The ALB, its target group, and
controller security groups are created during `aws-deploy`, outside Terraform
state. They add hourly ALB, capacity-unit, and public IPv4 charges until removed;
consult [ELB pricing](https://aws.amazon.com/elasticloadbalancing/pricing/).
On an unmerged branch the deployment helpers and values come from the working
tree, while gateway and fake-provider images still come from a published main
commit; no gateway binary change is needed for ingress.

`aws-deploy` checks the current account, finds exactly one public child zone and
one certificate for the hostname, and requires `ISSUED` before changing secrets
or the cluster. It installs the controller and labels the gateway namespace for
readiness-gate injection before upgrading the gateway. On the first install,
it creates ingress and waits for the Service's target-group binding, then
restarts the initial pods if they lack gates. It requires gated pods and a
completed rollout before publishing the Route 53 A alias. Later upgrades use
the existing binding. The pod mutator fails closed only for gateway pods in the
labeled namespace; migration and database Jobs do not match its pod selector.
See the controller's [readiness-gate requirements](https://kubernetes-sigs.github.io/aws-load-balancer-controller/latest/deploy/pod_readiness_gate/).

The ALB deregistration delay is 5s. AWS values keep the gateway serving for 15s
after readiness fails, then drain HTTP for up to 25s, with a 55s pod grace period
(at least 15 + 25 + 10). Its native fake-provider sidecar remains available
through shutdown. The ALB idle timeout is 150s, above the default 120s upstream
budget. These settings support the short synthetic rollout scenario; they do
not guarantee that arbitrary long completions finish during a rollout.

`make aws-check` retains the port-forward/store checks, then adds requests over
verified HTTPS to the actual hostname: readiness, a 401 without a gateway key,
both synthetic completions, and 404 for `/metrics` and `/debug/pprof/` on the API
ingress. It never disables TLS verification. `make aws-rollout` builds pinned
Vegeta v12.13.0 into a temporary directory and sends 5 completions/s for 90s,
restarting after 15s. This stays below the demo client's 10/s rate. It requires
readiness gates, load lasting through the rollout, and every measured request
returning 200 without a transport error. Environment and reports remain under
the gitignored `deploy/aws/session/rollout-*`; the key lives only in a private
temporary target file that is deleted afterwards. This is an opt-in
demonstration, not a capacity guarantee or CI test.

Teardown removes the session alias first, then deletes the Ingress and waits
for the controller's ALB and tagged security groups to disappear, before
Terraform destroys the cluster and VPC. DNS mutation checks ALB ownership tags
and refuses to replace/delete unexpected A/AAAA records. AWS inventory failures
fail the script, never masquerade as an absent resource. A certificate that has
expired does not block teardown. If controller cleanup fails, `aws-down` stops:
keep the cluster running, inspect controller logs and Ingress finalizers, and
restore cleanup; do not force-remove finalizers or destroy its VPC first.
`aws-leftovers` lists extra child-zone records, retaining only the apex NS/SOA
and the certificate's exact validation CNAME as expected persistent records.
The zone and certificate stay between sessions; the hostname has no address
after teardown.

If HTTPS times out, confirm your current public IP still falls within the
platform's CIDR; changing networks requires updating the platform and rerunning
`aws-deploy`. For an ingress failure, inspect:

```bash
kubectl --context llm-gateway-aws -n llm-gateway describe ingress llm-gateway
kubectl --context llm-gateway-aws -n kube-system logs deployment/aws-load-balancer-controller
kubectl --context llm-gateway-aws -n llm-gateway get pods -o wide
```

Mocked tests cover preflight failures, artifact checksum failure, initial and
existing readiness gates, DNS publication ordering and payload, ALB ownership,
foreign DNS records, inventory failures, cleanup order, and the leftover filter.
The operator's [live session](#ingress-verification--2026-10-13) separately
verified deployment, HTTPS routing, one successful rollout run, and cleanup.

### Ingress verification — 2026-10-13

The operator ran the AWS commands; the results below combine their terminal
output with the local rollout report. This session used `eu-north-1`, two
`c7i-flex.large` nodes running Kubernetes `v1.37.0-eks-3b4a6ca` on Amazon Linux
2023 (amd64), the v3.6.0 controller/chart, and the working-tree ingress helpers
and AWS values. The gateway and fake provider were the published images of main
commit `ce0e41cb9899fe910b2667824dd925b73812df7d`; the gateway index digest was
`sha256:0e541e310db84c0f3647a614a22918f2b5ee33e01e38f01b1fff557b913ef623`.
All completions used the fake-provider sidecar, not real provider APIs.

- Platform creation added exactly 74 resources, with no changes or deletions.
  Both nodes became Ready, with private addresses and no public node address.
- `make aws-deploy` installed the controller and gateway. It created the
  Ingress, then restarted the initial pods. Both replacement pods ran on
  separate nodes with readiness gates `1/1`, before the HTTPS alias was published.
- `make aws-check` passed both provider completions through the Service,
  metrics, and the Valkey shared-limit check: each pod admitted 5 and limited
  10 requests, so the shared burst admitted 10 of 30 overall. Both completions
  appeared in RDS with status 200, 12 input and 3 output tokens, and estimated
  cost `0.000018000000` under the synthetic prices.
- The HTTPS checks passed readiness, both completions with normal certificate
  verification, authentication rejection without a key, and absence of
  `/metrics` and `/debug/pprof/` from the API ingress.
- `make aws-rollout`, run `rollout-abnql0y7` at `2026-10-13T00:16:29Z`, sent
  5 completions/s for 90s with one rolling restart after 15s. All **450**
  requests returned **200**, with no reported transport errors. Latency p50 was
  **296.104 ms**, p99 **305.186 ms**, and maximum **511.482 ms**. The restart
  completed while traffic was still running. Raw environment/report artifacts
  remain gitignored under `deploy/aws/session/rollout-abnql0y7`.
- Rerunning `make aws-up` reported `No changes` and `0 added, 0 changed,
  0 destroyed`; both nodes stayed Ready.
- `make aws-down` completed and destroyed all **74** Terraform resources,
  after the ingress cleanup stage. It removed the session kubeconfig entries.
  `make aws-leftovers` then found no EKS, EC2, EBS, NAT, EIP, load balancer,
  RDS/backup/snapshot, ElastiCache/snapshot, Secret, project VPC, or extra gateway
  DNS record. Bootstrap, including the child zone and validation CNAME, was kept.

This verifies the full session lifecycle and one low-rate ALB rollout over the
operator's network. It is not a capacity or general zero-downtime guarantee.
Not exercised: long real-provider generations, an ALB rollout at high load,
node drain or AZ failure, a second ingress deployment in the same session,
connections from a disallowed IP, or negative TLS-version negotiation. The
CIDR and TLS policy restrictions are configured and covered by local checks;
the live positive HTTPS checks do not prove those negative cases.

## Manually verify image publication

CI publishes to GHCR, not ECR (see [below](#ci-images-and-aws-credentials)).
After applying bootstrap, build and push both local images once. Confirm both built images use the same Linux
architecture; the tag below includes that architecture and a UTC timestamp.
Use a unique tag; immutable repositories reject overwrites. These single-platform
images do not establish multi-platform publication.

```bash
make image
docker image inspect llm-gateway:dev llm-gateway-fakeprovider:dev \
  --format '{{.RepoTags}} {{.Os}}/{{.Architecture}}'
IMAGE_ARCH="$(docker image inspect --format '{{.Architecture}}' llm-gateway:dev)"
GATEWAY_REPO="$(terraform -chdir=deploy/aws/bootstrap output -json ecr_repository_urls | jq -er '.["llm-gateway"]')"
FAKEPROVIDER_REPO="$(terraform -chdir=deploy/aws/bootstrap output -json ecr_repository_urls | jq -er '.["llm-gateway-fakeprovider"]')"
ECR_REGISTRY="${GATEWAY_REPO%/*}"
IMAGE_TAG="bootstrap-$(git rev-parse --short=12 HEAD)-$IMAGE_ARCH-$(date -u +%Y%m%dT%H%M%SZ)"
aws ecr get-login-password --region eu-north-1 | docker login --username AWS --password-stdin "$ECR_REGISTRY"
docker tag llm-gateway:dev "$GATEWAY_REPO:$IMAGE_TAG"
docker tag llm-gateway-fakeprovider:dev "$FAKEPROVIDER_REPO:$IMAGE_TAG"
docker push "$GATEWAY_REPO:$IMAGE_TAG"
docker push "$FAKEPROVIDER_REPO:$IMAGE_TAG"
```

Keep the same `IMAGE_TAG` and `IMAGE_ARCH` for verification. Docker can publish an
OCI index even for one runnable platform, with additional attestation manifests.
The tag then identifies the index, not the image ECR scans; querying it can return
`ScanNotFoundException` while the platform image's scan is complete. Resolve the
Linux image for the built architecture and query that digest instead, excluding
attestations. A plain image manifest uses the tag's digest directly.
See [Docker's manifest inspection reference](https://docs.docker.com/reference/cli/docker/buildx/imagetools/inspect/).

```bash
(
  set -eu
  set -o pipefail
  for repository in llm-gateway llm-gateway-fakeprovider; do
    printf '\nRepository: %s\n' "$repository"
    TAG_DIGEST="$(aws ecr describe-images \
      --repository-name "$repository" --image-ids "imageTag=$IMAGE_TAG" \
      --region eu-north-1 --output json \
      --query 'imageDetails[0].imageDigest' | jq -er '.')"
    printf 'Published digest: %s\n' "$TAG_DIGEST"
    SCAN_DIGEST="$(docker buildx imagetools inspect \
      --raw "$ECR_REGISTRY/$repository@$TAG_DIGEST" |
      jq -er --arg arch "$IMAGE_ARCH" --arg digest "$TAG_DIGEST" '
        if has("manifests") then
          [.manifests[] |
            select(.platform.os == "linux" and .platform.architecture == $arch)] |
          if length == 1 then .[0].digest
          else error("expected exactly one matching Linux image") end
        else $digest end')"
    printf 'Scan digest (linux/%s): %s\n' "$IMAGE_ARCH" "$SCAN_DIGEST"
    aws ecr wait image-scan-complete \
      --repository-name "$repository" --image-id "imageDigest=$SCAN_DIGEST" \
      --region eu-north-1
    aws ecr describe-image-scan-findings \
      --repository-name "$repository" --image-id "imageDigest=$SCAN_DIGEST" \
      --region eu-north-1 --output json \
      --query '{Image:imageId,Status:imageScanStatus,Counts:imageScanFindings.findingSeverityCounts}'
  done
)
```

The verification subshell stops on errors without exiting your interactive shell.
Do not mark a failed or missing scan as successful. If the runnable image has no
scan, inspect `get-registry-scanning-configuration` and the repositories' scanning
settings before changing configuration or starting a manual scan.

After recording the results, remove the local ECR login:

```bash
docker logout "$ECR_REGISTRY"
```

Record the digests and scan status, and investigate failures or findings. Repository
scan-on-push is scoped to these repositories; bootstrap does not overwrite the
account-wide registry scanning configuration. Basic scanning checks OS packages,
not Go dependency vulnerabilities, and this manual check is not a CI security gate.
See [AWS basic scanning](https://docs.aws.amazon.com/AmazonECR/latest/userguide/image-scanning-basic.html).
Authentication expires; use password-stdin and a Docker credential helper to avoid
keeping registry credentials in plaintext. Reauthenticate when needed.

## Bootstrap verification — 2026-10-11

The operator ran the live checks in `eu-north-1` and supplied their terminal
results. The verified bootstrap/image revision is `512365bf8eee`; this record
does not claim another live run for later documentation-only commits.

- The post-apply Terraform plan refreshed all 10 resources and reported no changes.
- After state migration troubleshooting, `head-object` confirmed
  `bootstrap/terraform.tfstate` uses `AES256`, with version ID
  `J6KnrNnX0h0U63nZ2B4ax654KzFH0Bzp`.
- Both repositories contain tag `bootstrap-512365bf8eee-arm64-20261011T200534Z`.
  Each tag identifies an OCI index; each `linux/arm64` child scan returned
  `COMPLETE` with an empty severity-count map (`{}`), meaning no findings reported.

| Repository | Published OCI index digest | Scanned linux/arm64 image digest |
|---|---|---|
| `llm-gateway` | `sha256:77eb74a930af5c07e64d4f8a7b6fc429ef5257a302719b91b6e7a97edde9c4a7` | `sha256:136d9db637873b0904fb94672c20e1c56ca73043a00235b70a79eeaf0e94ffd8` |
| `llm-gateway-fakeprovider` | `sha256:a40627b6d4a939cf945a9d18eb2b06d43a41c69440c8e6b3a0aa22f8db2ffb9e` | `sha256:6f24f0da2bf19a4e61f6cf1430eb5556ebb3ad14278bbe6350d598930eb44842` |

This verifies bootstrap and manual arm64 publication only, not EKS deployment,
multi-platform publication, lock contention, or Go dependency security. Basic
scan results are point-in-time OS-package findings, not proof of a vulnerability-free
application. No state contents, backups, or credentials are included in this record.

## CI images and AWS credentials

Keyless pushes from GitHub Actions to ECR need an IAM OIDC identity provider for
GitHub and a role that trusts it. The project's managed SCPs deny
`iam:*Provider*`, so neither can be created. The remaining AWS options were
long-lived IAM user access keys stored in GitHub, which the roadmap rules out in
favor of workload identity, and activating advanced AWS features, which lifts the
SCPs but changes the project's account model. Instead, CI publishes to the GitHub
Container Registry with the workflow's own `GITHUB_TOKEN`:

- The `publish` job runs only for pushes to `main` in this repository, after every
  other job has passed, and is the only job with `packages: write`. Its token
  expires with the job and is used only by `docker login`, never by an action;
  the checkout keeps no credentials.
- `make image-scan` scans both images for both platforms in the job's builder,
  then `make image-publish` pushes them from the same build cache as
  `ghcr.io/ipekutku/llm-gateway:<sha>` and `ghcr.io/ipekutku/llm-gateway-fakeprovider:<sha>`.
  Each multi-platform index carries the repository source annotation and minimal
  BuildKit provenance: the repository, the commit, and the pinned base image digests.
- GitHub holds nothing that can reach the AWS project. ECR remains the manual
  path from the operator's machine, above.

A new GHCR package can start private. After the first publication, check both
packages under the repository's Packages, and make them public so a cluster can
pull them without a pull secret. The images contain no configuration or
credentials, and the scan also looks for secrets. A cluster in private subnets
will pull through the NAT gateway, which bills per GB processed; each image is
about 10 MB compressed.

Limitations: GHCR tags are mutable, so deployments should pin the digest that a
SHA tag resolves to; SHA tags accumulate with no retention policy; and a CI
deployment job would need AWS credentials, which the same restriction blocks.

## Platform sessions

The platform module creates a VPC across `eu-north-1a` and `eu-north-1b` (private
/19 subnets for nodes, public /24 subnets for one NAT gateway, and a free S3 gateway
endpoint) and an EKS 1.37 cluster with two `c7i-flex.large` nodes. It uses the
pinned `terraform-aws-modules/vpc` 6.7.3 and `terraform-aws-modules/eks` 21.29.0
modules. State is `platform/terraform.tfstate` in the bootstrap bucket; its
committed `backend.tf` takes the bucket and account from `make aws-up` and `make aws-down`.

The network and cluster alone cost about **$0.30 per hour**: the EKS control plane
($0.10), two nodes (about $0.08 each), and the NAT gateway with its public address
(about $0.05), plus NAT data processing per GB. Left running, that is about $7 a day
before RDS, Valkey, storage, and Secrets Manager charges. The data stores now
belong to the same session stack and must also be destroyed at the end.

Authenticate as in [Setup and costs](#setup-and-costs), then set the address
allowed to reach the Kubernetes API. It must be a /24 or narrower; use your own:

```bash
export TF_VAR_admin_cidr="$(curl -s https://checkip.amazonaws.com)/32"
make aws-leftovers   # expect "none" everywhere before starting
make aws-up          # shows the plan and asks for approval; about 13 minutes
```

Review the plan before approving: `aws_eks_cluster` at `1.37`, one NAT gateway, a
node group of `c7i-flex.large` on `AL2023_x86_64_STANDARD`, and no
`aws_iam_openid_connect_provider`, `aws_kms_key`, or `aws_cloudwatch_log_group`.
`aws-up` then adds the context `llm-gateway-aws` to your kubeconfig. Healthy nodes
join a few minutes after the node group starts. If it is still creating after about
10 minutes, interrupt it and run `make aws-down`: EKS otherwise retries failed
launches for about 30 minutes, which bills the whole time.

End every session the same way, even after a failed apply:

```bash
make aws-down        # removes ingress/DNS, then asks to destroy the platform
make aws-leftovers   # fails while anything billable remains
```

`aws-leftovers` searches the Region by resource type rather than by tag, so it also
finds what Terraform does not manage, such as load balancers created by Kubernetes:
EKS clusters, EC2 instances, EBS volumes, NAT gateways, Elastic IPs, load balancers,
RDS instances, manual snapshots and automated backups, ElastiCache clusters and
snapshots, Secrets Manager secrets, and leftover project VPCs.
A NAT gateway can show as deleting for a few minutes; run it again.

Access to the cluster:

- The public API endpoint accepts only `admin_cidr`; nodes use the private endpoint.
  If your address changes, run `make aws-up` again with the new value.
- Authentication uses EKS access entries only (no `aws-auth` ConfigMap). Whoever
  applies gets cluster admin.
- IRSA is off because the project cannot create IAM identity providers; External
  Secrets and the Load Balancer Controller use EKS Pod Identity, whose agent runs on every node.
- There is no customer-managed KMS key (EKS encrypts Secrets with an AWS-owned key)
  and no control-plane logging, since both bill per session.

On some macOS 26 Macs, a second `terraform init` over the EKS module's installed
local submodules hangs, because a nil-pointer dereference there hangs instead of
panicking ([hashicorp/terraform#39047](https://github.com/hashicorp/terraform/issues/39047)).
The Make targets delete `.terraform/modules` before each init; do the same before
running `terraform init` in `deploy/aws/platform` by hand.

## Platform verification — 2026-10-12

The operator ran the session from a Mac with Terraform 1.16.5 and supplied the results.

- A first apply with `t4g.medium` nodes created the cluster, network, and add-ons,
  but its node group failed after about 33 minutes with `AsgInstanceLaunchFailures`:
  the Free plan allows only free-tier-eligible instance types. `make aws-down`
  removed everything. The node type was changed to `c7i-flex.large`.
- `make aws-up` then created 52 resources in 13 minutes. Both nodes were `Ready`
  on `v1.37.0-eks`, `amd64`, Amazon Linux 2023, one in each private subnet,
  with no external address.
- All add-on pods ran: `aws-node`, `kube-proxy`, and the Pod Identity agent on each
  node, and two replicas each of CoreDNS and metrics-server with their own VPC
  addresses. `kubectl top nodes` reported about 470 MiB (15%) of memory per node.
- `make aws-down` destroyed all 52 resources in 11 minutes and removed the
  kubeconfig entries; `make aws-leftovers` then reported none of the checked
  resource types in `eu-north-1`.

This verifies the network, cluster, node group, add-ons, and teardown, not a
gateway deployment, managed data stores, ingress, or any request traffic.

## Private data stores

The platform also defines PostgreSQL **18.6** on RDS (`db.t4g.micro`, Single-AZ,
20 GiB encrypted gp3) and **Valkey 9.1** on ElastiCache (`cache.t4g.micro`, one
node, cluster mode disabled). Both use private subnets; their security groups
accept only their service port from the EKS node security group, including pods
on those nodes. They are not reachable directly from the laptop. RDS forces TLS;
Valkey requires TLS and an AUTH token. There are no backups, replicas, final
snapshots, or deletion protection: `aws-down` permanently deletes the stores and
their data. This is a session environment, not a durable production deployment.

Before applying, confirm the pinned engine versions and class are available in
the project's Region:

```bash
aws rds describe-orderable-db-instance-options --region eu-north-1 \
  --engine postgres --engine-version 18.6 --db-instance-class db.t4g.micro \
  --query 'OrderableDBInstanceOptions[].{Version:EngineVersion,Class:DBInstanceClass,Storage:StorageType}'
aws elasticache describe-cache-engine-versions --region eu-north-1 \
  --engine valkey --engine-version 9.1 --query 'CacheEngineVersions[].EngineVersion'
```

On 2026-10-12, read-only API checks confirmed PostgreSQL 18.6 with `db.t4g.micro`
and gp3, and Valkey 9.1 in `eu-north-1`. The project was still on the Free plan,
and `make aws-leftovers` found none of the checked resources. This does not
establish a successful store creation or connection. Check current plan limits
before changing classes; engine/class availability alone does not prove that a
Free-plan apply is permitted.

### Secrets flow

RDS generates its master password and stores it in a service-managed secret.
Terraform exposes `database_master_secret_arn`, never the password. RDS deletes
the secret when its instance is deleted; see
[AWS password management](https://docs.aws.amazon.com/AmazonRDS/latest/UserGuide/rds-secrets-manager.html).

Terraform generates the Valkey token and writes it to
`llm-gateway/redis-auth-token`. This token **is in Terraform state**, in both the
random-password resource and the cache/secret resources, even though plans hide
it. S3 encryption and private access protect state; old versions retain old
tokens after destruction. Restrict state access accordingly, and never print
`terraform show -json` or retrieve secret values into shared terminal output.

The other four secrets are empty containers only. `make aws-deploy` fills each
one that is still empty with a value for this session:

| Name | Value supplied outside Terraform | Filled by `aws-deploy` with |
|---|---|---|
| `llm-gateway/openai-api-key` | OpenAI key | a synthetic key the fake provider ignores |
| `llm-gateway/anthropic-api-key` | Anthropic key | a synthetic key the fake provider ignores |
| `llm-gateway/clients` | Complete clients JSON, containing only SHA-256 key hashes | two random clients, `demo` and `limited` |
| `llm-gateway/database-app-password` | Password of the gateway's `gateway_app` PostgreSQL role | 64 random hex characters |

Never reuse the public kind/load-test keys here. To supply your own value, write
it before `aws-deploy`, which never replaces an existing value. Use the AWS CLI
with a private file rather than a literal secret in command arguments or shell
history. For example, replace the path below with a file readable only by you:

```bash
aws secretsmanager put-secret-value --region eu-north-1 \
  --secret-id llm-gateway/clients \
  --secret-string file:///absolute/path/to/private/clients.json \
  --query ARN --output text
```

Use the same command with each provider secret name and a private text file
containing that key. Values never enter Terraform. Store checks need only the
RDS-managed secret and generated Valkey token, so these containers can remain
empty until gateway deployment. All Terraform-created secrets have zero
recovery days, so every session starts empty. AWS secret deletion is asynchronous;
if immediate recreation reports an existing name, wait for deletion to finish.

### Check the private data stores

After reviewing the plan and starting a session:

```bash
make aws-up
make aws-check-stores
make aws-down
make aws-leftovers
```

Review the additions before approving `aws-up`: one private RDS instance, one
single-node Valkey replication group, their subnet/security groups, four
Terraform-managed secret containers, one generated token and its secret version.
The RDS-managed secret is created by RDS. There is no public database, new IAM
role, customer-managed KMS key, or gateway deployment in this step.

`aws-check-stores` needs `aws`, `terraform`, `kubectl`, `jq`, and `curl`, with the
same AWS profile and project as `aws-up`. It uses the `llm-gateway-aws` context,
creates a unique temporary namespace, and runs a restricted pod with the
`postgres:18` image for PostgreSQL and OpenSSL, plus a `redis:8` container for
plaintext rejection. The latter receives no token. Credentials travel through
private temporary files and stdin into a temporary Kubernetes Secret, without a
last-applied annotation. Cleanup deletes the namespace and local files; a
cleanup failure makes the command fail and names the namespace to remove if
the API was unreachable.

The checks cover a PostgreSQL connection with `sslmode=verify-full` and the
Region's RDS CA bundle, its negotiated TLS details, rejection without TLS,
Valkey certificate/hostname verification on every TLS command connection,
authenticated `PING` and a trivial Lua script over TLS, and rejection without
AUTH. OpenSSL verifies the certificate before sending AUTH on that same
connection; it reads the token from stdin. A plaintext Valkey connection
must close/reset or provide no response within 10 seconds; an application reply
fails the check. No token is sent on that plaintext connection. Connection
commands and the pod lifetime have time limits; a failed or unfinished pod
fails the command.

Record the client output and teardown result after a live run. The
[store verification below](#store-verification--2026-10-12) covers live TLS and
authentication; mocked Terraform tests alone do not establish that behavior.
This step does not run migrations, insert usage
rows, or test the gateway's shared limits. CI uses Redis 8, while AWS runs Valkey
9.1; the gateway's GCRA and lease scripts were checked later, by the
[gateway verification](#gateway-verification--2026-10-13). If any step fails,
still run `aws-down` and `aws-leftovers` before ending the session.

### Store verification — 2026-10-12

The operator supplied the live terminal results from `eu-north-1`:

- `make aws-up` created 67 resources in 11 minutes 14 seconds. Both amd64
  Amazon Linux 2023 nodes were Ready on Kubernetes `v1.37.0-eks`, with no
  external addresses.
- `make aws-check-stores` passed. PostgreSQL reported 18.6 on aarch64 and
  negotiated TLS 1.3 with `TLS_AES_256_GCM_SHA384`; plaintext was rejected.
  Valkey reported 9.1.0; certificate-chain/hostname verification, authenticated
  PING, and the trivial Lua check passed. No AUTH was rejected; plaintext
  produced no response within the 10-second bound. Temporary-pod cleanup
  completed successfully.
- The first follow-up plan found one parameter-group update: AWS returned
  `pending-reboot` for `rds.force_ssl`, while the provider defaulted to
  `immediate`. Configuration now sets `apply_method="pending-reboot"`
  explicitly, with a mocked regression assertion. The operator reran
  `terraform plan -detailed-exitcode`: no changes, exit code 0.
- `make aws-down` destroyed all 67 resources and removed the AWS kubeconfig
  entries. `make aws-leftovers` then reported none for every checked category,
  including RDS backups/snapshots, ElastiCache snapshots, and Secrets Manager
  secrets. Persistent bootstrap resources were retained. No teardown duration
  was supplied.

These results establish managed-store connections from an EKS pod, not gateway
deployment, schema migrations, usage persistence, or shared gateway limits.
The live run used the earlier helper, which checked the Valkey hostname on a
separate connection from `redis-cli` AUTH. Security review caught that gap;
the revised helper sends AUTH through the hostname-verifying OpenSSL connection.
Local tests with real TLS verify successful AUTH and reject both hostname
mismatches and untrusted CAs before application data is sent.

In a second session, the operator created all 67 resources again and confirmed
both nodes Ready. The revised helper passed AUTH and PING over verified TLS,
plus the PostgreSQL and plaintext-rejection checks, but failed the Lua reply
check: it expected `+ok` instead of the bulk-string reply `$2\r\nok\r\n` that
`return 'ok'` produces. The helper and TLS fixture now check the correct reply,
with regression cases rejecting a status reply, wrong length, or wrong content.
The Lua return type was also reproduced with local Redis 8. The operator reran
the corrected helper on the same platform: verified AUTH and PING, the Lua bulk
reply and content, server versions, the complete command response, and rejection
without AUTH all passed. PostgreSQL still reported 18.6 over TLS 1.3 and rejected
plaintext; plaintext Valkey again gave no response within 10 seconds.

The second session's follow-up plan reported no changes with exit code 0.
`make aws-down` then destroyed all 67 resources and removed the AWS kubeconfig
entries; `make aws-leftovers` reported none in every checked category, including
backups, snapshots, and secrets. This verifies the final helper against AWS.
No second-session creation or teardown durations were supplied.

## Deploy and check the gateway

After `make aws-up`, in the same shell (the AWS login, `AWS_PROFILE`, and the
`TF_VAR_` variables from [Platform sessions](#platform-sessions)):

```bash
git fetch origin           # aws-deploy deploys the newest main commit this branch contains
make aws-deploy            # a few minutes
make aws-check
make aws-down
make aws-leftovers
```

The cluster then runs two gateway pods, each with the fake provider as a sidecar,
plus the three External Secrets Operator pods. That adds no hourly AWS charge
beyond the session's nodes; the controller's two pods fit there too. HTTPS ingress
adds the ALB charges described above. The IAM roles and Pod
Identity association are free, and Secrets Manager bills each API call at a
fraction of a cent. Image pulls cross the NAT gateway, which bills per GB
processed; the largest is `postgres:18` for the Jobs, at roughly 150 MB per node,
so a session's pulls cost a few cents.

What `aws-deploy` does, in order (see [Gateway on EKS](architecture.md#gateway-on-eks)):

1. Resolves `ghcr.io/ipekutku/llm-gateway` and `-fakeprovider` for the chosen
   commit to their digests. It fails if CI has not published that commit, for
   example on an unmerged branch whose base is not in your local `origin/main`.
   To deploy another commit from `main`, set `AWS_IMAGE_SHA` to its full SHA.
2. Fills the empty session secrets (see [Secrets flow](#secrets-flow)) and writes
   the two client keys to `deploy/aws/session/demo-key` and `limited-key`.
   Treat them as credentials; they are gitignored and valid only until `aws-down`.
3. Installs External Secrets Operator 2.12.0 into `external-secrets`, creates the
   namespace `llm-gateway`, the RDS CA ConfigMap, and the ExternalSecrets from
   `deploy/aws/secrets.yaml`, and waits until all six Kubernetes Secrets are synced.
4. Runs the `llm-gateway-database-role` Job, which creates or updates
   `gateway_app` and prints its table privileges (`none yet` on the first deploy,
   `INSERT, SELECT` afterwards).
5. Installs the Load Balancer Controller and labels the namespace for readiness
   gates, then installs or upgrades the gateway chart; its hook migrates as the master user.
6. Applies HTTPS ingress, ensures ALB readiness gates, and publishes the session DNS alias.

Rerunning `aws-deploy` in the same session upgrades in place and keeps the
secrets. The gateway reads its Secrets only at startup: after writing a new
value, wait for the operator's hourly refresh, or force one with
`kubectl annotate externalsecret <name> force-sync=$(date +%s) --overwrite`, then run
`kubectl --context llm-gateway-aws -n llm-gateway rollout restart deployment/llm-gateway`.

`aws-check` prints each step: both completions with their request IDs, the
metrics sample, admitted and limited counts per pod, and the two usage rows (no
content, only IDs, models, status, tokens, and estimated cost). Its shared-limit
step uses the `limited` client's burst of 10, which refills one request every 10
seconds; run it again within about two minutes and it reports fewer than 10
admitted. To use the gateway by hand, keep a port-forward open and read the key
from the file, not the command line:

```bash
kubectl --context llm-gateway-aws -n llm-gateway port-forward service/llm-gateway 8080:8080 &
printf 'Authorization: Bearer %s\n' "$(cat deploy/aws/session/demo-key)" > /tmp/llm-gateway-demo.header
curl -s localhost:8080/v1/chat/completions -H @/tmp/llm-gateway-demo.header \
  -H 'Content-Type: application/json' \
  -d '{"model":"demo-openai","messages":[{"role":"user","content":"hello"}]}'
rm /tmp/llm-gateway-demo.header; kill %1
```

Review these additions in the `aws-up` plan: `aws_iam_role.external_secrets`
(trusting only `pods.eks.amazonaws.com` for this account and cluster), its inline
policy listing six secret ARNs with `GetSecretValue` and `DescribeSecret` only,
`aws_eks_pod_identity_association.external_secrets` for
`external-secrets/external-secrets`, and the `database-app-password` container.
The controller role/policy and Pod Identity association are session resources too.
`aws-down` first removes the alias and controller-created ingress resources,
then removes what Helm installed with the cluster. Bootstrap is kept.

Troubleshooting (`aws-up` makes `llm-gateway-aws` the current kubectl context):

- **ExternalSecrets not Ready:** `kubectl -n llm-gateway describe externalsecret`
  shows the operator's error. `AccessDenied` means the operator pod started
  before the Pod Identity association existed or lacks the role: restart it with
  `kubectl -n external-secrets rollout restart deployment/external-secrets`.
- **Role Job failed:** `kubectl -n llm-gateway logs job/llm-gateway-database-role`.
  A certificate error names the CA bundle; a password error means the owner
  Secret is stale (force a sync as above).
- **Pods not ready or `CrashLoopBackOff`:** `kubectl -n llm-gateway logs deploy/llm-gateway -c gateway`.
  The gateway's startup errors name the setting, never its value. `tls_certificate`
  means the database CA does not match; an authentication failure for
  `gateway_app` means the role Job and the Secret disagree, which a rerun of
  `make aws-deploy` resolves.
- **`aws-check` reports local limits:** the gateway could not reach Valkey; its
  log has one warning with the reason, and `gateway_ratelimit_store_errors_total`
  counts failures.

### Gateway verification — 2026-10-13

The operator ran the session from a Mac with the working tree of this change and
supplied the terminal output:

- `make aws-up` created 71 resources in 12 minutes 52 seconds: the 67 of the store
  session plus the External Secrets role, its policy, the Pod Identity
  association, and the `database-app-password` container. Both amd64 nodes were
  `Ready` on `v1.37.0-eks` with no external address.
- `make aws-deploy` finished in 2 minutes 57 seconds. It deployed commit
  `98f00586cfa8` as `llm-gateway@sha256:ef6c3f8c…` and
  `llm-gateway-fakeprovider@sha256:53de61f1…`, filled the four empty secrets, and
  installed External Secrets Operator. All six ExternalSecrets reached
  `SecretSynced` without an access error, so the Pod Identity role and its
  six-ARN policy were sufficient. The role Job reported `none yet (no tables)`,
  as expected before the first migration. The chart's migration hook succeeded,
  and two pods ran `2/2` on different nodes. The startup line reported
  `"rate_limits":"shared"` and two clients.
- `make aws-check` passed. Both models answered through the Service, and the
  pod's metrics counted the requests with `gateway_ratelimit_store_up 1`. Of 30
  requests from the limited client alternating between the two pods, 10 were
  admitted (5 on each) and 20 limited. Both completions' usage rows were in RDS:
  client `demo`, status 200, 12 input and 3 output tokens, and an estimated
  $0.000018 each, matching the synthetic prices.
- A second `make aws-deploy` finished in 57 seconds and upgraded both releases
  in place: no secret was refilled, every ExternalSecret was unchanged and still
  synced, the role Job reported `gateway_app table privileges: INSERT, SELECT` on
  the migrated tables, and the chart's second revision kept the same pods. A
  second `make aws-check` passed the same way, again admitting 10 of 30 (5 per
  pod), with new usage rows for both completions.
- `make aws-down` destroyed all 71 resources in 12 minutes 10 seconds, including
  the IAM role and Pod Identity association, and removed the AWS kubeconfig
  entries. `make aws-leftovers` then reported none in every checked category.
  Everything Helm installed went with the cluster; none of it had created AWS
  resources outside Terraform.

This establishes on AWS what mocked tests could not: Pod Identity credentials for
the operator, the templated database and `rediss://` URLs, `verify-full` TLS to
RDS with the mounted CA bundle, the gateway running as `gateway_app` (its schema
check reads and its usage writes insert), and the gateway's GCRA and lease
scripts on Valkey 9.1.0, sharing one client's burst across two pods, and a
repeatable deployment. Not covered: ingress, a rolling restart or node drain on
AWS, autoscaling, and real providers.

## Validation and provider updates

`make terraform-check` checks formatting, then for each module initializes with
`-backend=false` and `-lockfile=readonly`, validates, and runs mocked Terraform
tests. Downloads need network access, but no AWS API is contacted. It runs in a
separate CI job and is not part of the Go-only `make check`. Locally, a module
whose working directory was already initialized with the S3 backend (bootstrap
after migration, the platform after `aws-up`) asks for AWS credentials even
then; log in first. The checks do not prove an AWS apply, state migration, or
push works in this account.

The platform's mocked tests check the Kubernetes version, the absence of an IAM
OIDC provider, KMS key, and control-plane log group, the exact add-on set, two AZs
with one NAT gateway, and the S3 gateway endpoint; they also require that an open
or wider than /24 `admin_cidr` and a malformed account ID are rejected. A mocked
apply makes generated IDs available to check that the stores use the private
subnets, their own security groups, and node-only ingress, plus encryption,
password management, token/secret agreement, and session teardown settings.

Commit each module's `.terraform.lock.hcl`; it records the pinned providers'
checksums for `linux_amd64` (CI), `darwin_arm64`, and `darwin_amd64` (Macs). The
platform pins `aws` and the providers the EKS module needs (`cloudinit`, `null`,
`time`, `tls`), plus `random` for the Valkey token. After intentionally changing
a provider or module version, regenerate and review the lock file with:

```bash
terraform -chdir=deploy/aws/<module> providers lock \
  -platform=linux_amd64 -platform=darwin_arm64 -platform=darwin_amd64
make terraform-check
```

Keep the Terraform CLI version aligned in `versions.tf`, CI, and this runbook.
Terraform and its provider are development tools; they add no Go dependency or
gateway runtime permission. Live creation, migration, and image push remain manual
verification steps; the bootstrap results above are separate from mocked CI checks.
