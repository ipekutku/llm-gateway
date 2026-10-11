# AWS runbook

Milestone 10 begins with the persistent Terraform bootstrap in
[`deploy/aws/bootstrap`](../deploy/aws/bootstrap). It creates only a state bucket
and two private image repositories. The gateway does not run on AWS yet.
The roadmap defines the remaining [milestone scope](ROADMAP.md#milestone-10--aws-infrastructure).

## Setup and costs

Use AWS CLI v2, Terraform **1.16.5**, Docker, and `jq`. The AWS provider is pinned
to **6.68.0**. Kubernetes work later needs `kubectl` and Helm 4.3.0.
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
instances. GitHub OIDC publication/deployment needs a separate decision in its
own PR; this bootstrap adds no identity provider or CI AWS role. Its CI job needs
no AWS credentials.

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

CI still builds images without publishing. After applying bootstrap, build and
push both local images once. This example is for an arm64 Mac: confirm the built
architecture, and change the tag suffix on an amd64 machine. Use a unique tag;
immutable repositories reject overwrites. These single-platform images do not
establish multi-platform publication.

```bash
make image
docker image inspect llm-gateway:dev llm-gateway-fakeprovider:dev \
  --format '{{.RepoTags}} {{.Os}}/{{.Architecture}}'
GATEWAY_REPO="$(terraform -chdir=deploy/aws/bootstrap output -json ecr_repository_urls | jq -r '.["llm-gateway"]')"
FAKEPROVIDER_REPO="$(terraform -chdir=deploy/aws/bootstrap output -json ecr_repository_urls | jq -r '.["llm-gateway-fakeprovider"]')"
ECR_REGISTRY="${GATEWAY_REPO%/*}"
IMAGE_TAG="bootstrap-$(git rev-parse HEAD)-arm64"
aws ecr get-login-password | docker login --username AWS --password-stdin "$ECR_REGISTRY"
docker tag llm-gateway:dev "$GATEWAY_REPO:$IMAGE_TAG"
docker tag llm-gateway-fakeprovider:dev "$FAKEPROVIDER_REPO:$IMAGE_TAG"
docker push "$GATEWAY_REPO:$IMAGE_TAG"
docker push "$FAKEPROVIDER_REPO:$IMAGE_TAG"

aws ecr describe-images --repository-name llm-gateway --image-ids "imageTag=$IMAGE_TAG" \
  --query 'imageDetails[].{Digest:imageDigest,Tags:imageTags,Bytes:imageSizeInBytes}'
aws ecr describe-images --repository-name llm-gateway-fakeprovider --image-ids "imageTag=$IMAGE_TAG" \
  --query 'imageDetails[].{Digest:imageDigest,Tags:imageTags,Bytes:imageSizeInBytes}'
aws ecr wait image-scan-complete --repository-name llm-gateway --image-id "imageTag=$IMAGE_TAG"
aws ecr describe-image-scan-findings --repository-name llm-gateway --image-id "imageTag=$IMAGE_TAG" \
  --query '{Status:imageScanStatus,Counts:imageScanFindings.findingSeverityCounts}'
aws ecr wait image-scan-complete --repository-name llm-gateway-fakeprovider --image-id "imageTag=$IMAGE_TAG"
aws ecr describe-image-scan-findings --repository-name llm-gateway-fakeprovider --image-id "imageTag=$IMAGE_TAG" \
  --query '{Status:imageScanStatus,Counts:imageScanFindings.findingSeverityCounts}'
docker logout "$ECR_REGISTRY"
```

Record the digests and scan status, and investigate failures or findings. Repository
scan-on-push is scoped to these repositories; bootstrap does not overwrite the
account-wide registry scanning configuration. Basic scanning checks OS packages,
not Go dependency vulnerabilities, and this manual check is not a CI security gate.
See [AWS basic scanning](https://docs.aws.amazon.com/AmazonECR/latest/userguide/image-scanning-basic.html).
Authentication expires; use password-stdin and a Docker credential helper to avoid
keeping registry credentials in plaintext. Reauthenticate when needed.

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
verification steps until results are recorded.
