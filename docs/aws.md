# AWS runbook

Milestone 10 has two Terraform root modules. The persistent bootstrap in
[`deploy/aws/bootstrap`](../deploy/aws/bootstrap) creates a state bucket and two
private image repositories. The [platform](#platform-sessions) in
[`deploy/aws/platform`](../deploy/aws/platform) creates the network and an EKS
cluster for a working session, then is destroyed. The gateway does not run on AWS yet.
The roadmap defines the remaining [milestone scope](ROADMAP.md#milestone-10--aws-infrastructure).

## Setup and costs

Use AWS CLI v2, Terraform **1.16.5**, Docker with Buildx, and `jq`. The AWS provider
is pinned to **6.68.0**. Kubernetes work later needs `kubectl` and Helm 4.3.0.
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

The platform holds the hourly-cost resources: today EKS, two nodes, and a NAT
gateway; later RDS, ElastiCache, and possibly an ALB. Create it only for working
sessions, then remove it and check for leftovers (see [Platform sessions](#platform-sessions)).
Session teardown keeps bootstrap.

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

While it exists, the platform costs about **$0.30 per hour**: the EKS control plane
($0.10), two nodes (about $0.08 each), and the NAT gateway with its public address
(about $0.05), plus NAT data processing per GB. Left running, that is about $7 a day.

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
make aws-down        # shows the destroy plan and asks for approval
make aws-leftovers   # fails while anything billable remains
```

`aws-leftovers` searches the Region by resource type rather than by tag, so it also
finds what Terraform does not manage, such as load balancers created by Kubernetes:
EKS clusters, EC2 instances, EBS volumes, NAT gateways, Elastic IPs, load balancers,
RDS instances and manual snapshots, ElastiCache clusters, and leftover project VPCs.
A NAT gateway can show as deleting for a few minutes; run it again.

Access to the cluster:

- The public API endpoint accepts only `admin_cidr`; nodes use the private endpoint.
  If your address changes, run `make aws-up` again with the new value.
- Authentication uses EKS access entries only (no `aws-auth` ConfigMap). Whoever
  applies gets cluster admin.
- IRSA is off because the project cannot create IAM identity providers; add-ons
  that need AWS permissions will use EKS Pod Identity, whose agent runs on every node.
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

## Validation and provider updates

`make terraform-check` checks formatting, then for each module initializes with
`-backend=false` and `-lockfile=readonly`, validates, and runs mocked Terraform
plans. Downloads need network access, but no AWS API is contacted. It runs in a
separate CI job and is not part of the Go-only `make check`. Locally, a module
whose working directory was already initialized with the S3 backend (bootstrap
after migration, the platform after `aws-up`) asks for AWS credentials even
then; log in first. The checks do not prove an AWS apply, state migration, or
push works in this account.

The platform's mocked plans check the Kubernetes version, the absence of an IAM
OIDC provider, KMS key, and control-plane log group, the exact add-on set, two AZs
with one NAT gateway, and the S3 gateway endpoint; they also require that an open
or wider than /24 `admin_cidr` and a malformed account ID are rejected.

Commit each module's `.terraform.lock.hcl`; it records the pinned providers'
checksums for `linux_amd64` (CI), `darwin_arm64`, and `darwin_amd64` (Macs). The
platform pins `aws` and the providers the EKS module needs (`cloudinit`, `null`,
`time`, `tls`). After intentionally changing a provider or module version,
regenerate and review the lock file with:

```bash
terraform -chdir=deploy/aws/<module> providers lock \
  -platform=linux_amd64 -platform=darwin_arm64 -platform=darwin_amd64
make terraform-check
```

Keep the Terraform CLI version aligned in `versions.tf`, CI, and this runbook.
Terraform and its provider are development tools; they add no Go dependency or
gateway runtime permission. Live creation, migration, and image push remain manual
verification steps; the bootstrap results above are separate from mocked CI checks.
