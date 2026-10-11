# AWS runbook

Milestone 10 begins with the persistent Terraform bootstrap in
[`deploy/aws/bootstrap`](../deploy/aws/bootstrap). It creates only a state bucket
and two private image repositories. The gateway does not run on AWS yet.
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

The later platform stack will contain the hourly-cost resources: EKS, nodes, NAT,
RDS, ElastiCache, and possibly an ALB. Create that stack only for working sessions,
then remove it and check for leftovers. Session teardown must keep bootstrap.
The platform and its up/down commands are not implemented in PR 1.

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

## Validation and provider updates

`make terraform-check` checks formatting, initializes with `-backend=false` and
`-lockfile=readonly`, validates, and runs mocked Terraform plans. Downloads need
network access, but no backend or AWS API is contacted and no credentials are
required. It runs in a separate CI job and is not part of the Go-only `make check`.
It does not prove an AWS apply, state migration, or push works in this account.

Commit `.terraform.lock.hcl`; it records the pinned provider's checksums for
`linux_amd64` (CI), `darwin_arm64`, and `darwin_amd64` (Macs). After intentionally
changing the provider version, regenerate and review it with:

```bash
terraform -chdir=deploy/aws/bootstrap providers lock \
  -platform=linux_amd64 -platform=darwin_arm64 -platform=darwin_amd64
make terraform-check
```

Keep the Terraform CLI version aligned in `versions.tf`, CI, and this runbook.
Terraform and its provider are development tools; they add no Go dependency or
gateway runtime permission. Live creation, migration, and image push remain manual
verification steps; the bootstrap results above are separate from mocked CI checks.
