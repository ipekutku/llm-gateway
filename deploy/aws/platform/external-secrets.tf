# External Secrets Operator copies the gateway's secrets from Secrets Manager
# into the Kubernetes Secrets the chart expects. make aws-deploy installs it
# in this namespace under this
# service account, and EKS Pod Identity gives that service account the role
# below. The gateway pods get no AWS permissions.
locals {
  external_secrets_namespace       = "external-secrets"
  external_secrets_service_account = "external-secrets"
}

# Only the Pod Identity service may assume the role, and only on behalf of
# this cluster in this account, which guards against the confused deputy.
resource "aws_iam_role" "external_secrets" {
  name        = "${local.name}-external-secrets"
  description = "External Secrets Operator in the llm-gateway cluster: reads the gateway's secrets"
  assume_role_policy = jsonencode({
    Version = "2012-10-17"
    Statement = [{
      Effect    = "Allow"
      Principal = { Service = "pods.eks.amazonaws.com" }
      Action    = ["sts:AssumeRole", "sts:TagSession"]
      Condition = {
        StringEquals = { "aws:SourceAccount" = var.aws_account_id }
        ArnEquals    = { "aws:SourceArn" = module.eks.cluster_arn }
      }
    }]
  })
}

# Read-only access to exactly the platform's secrets, listed by ARN rather
# than by a name pattern. The RDS-managed secret is encrypted with the AWS
# managed aws/secretsmanager key, whose key policy already allows decryption
# through Secrets Manager, so no KMS permission is needed.
resource "aws_iam_role_policy" "external_secrets" {
  name = "read-gateway-secrets"
  role = aws_iam_role.external_secrets.id
  policy = jsonencode({
    Version = "2012-10-17"
    Statement = [{
      Effect = "Allow"
      Action = ["secretsmanager:GetSecretValue", "secretsmanager:DescribeSecret"]
      Resource = concat(
        [for secret in aws_secretsmanager_secret.operator : secret.arn],
        [
          aws_secretsmanager_secret.redis_auth_token.arn,
          aws_db_instance.postgres.master_user_secret[0].secret_arn,
        ],
      )
    }]
  })
}

# Pods created under this service account receive the role's credentials
# from the Pod Identity agent. The service account need not exist yet.
resource "aws_eks_pod_identity_association" "external_secrets" {
  cluster_name    = module.eks.cluster_name
  namespace       = local.external_secrets_namespace
  service_account = local.external_secrets_service_account
  role_arn        = aws_iam_role.external_secrets.arn
}
