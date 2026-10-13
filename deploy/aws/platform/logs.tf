# Application logs: Fluent Bit (deploy/aws/fluent-bit.yaml, installed by
# make aws-deploy) ships the gateway's own JSON lines to this log group.
# Logs only: no Container Insights, metric filters, or alarms.
locals {
  fluent_bit_namespace       = "fluent-bit"
  fluent_bit_service_account = "fluent-bit"
}

# A session resource like the rest of the platform: destroyed, with its
# events, by make aws-down. One day of retention bounds storage if a session
# is left running.
resource "aws_cloudwatch_log_group" "application" {
  name              = "/llm-gateway/application"
  retention_in_days = 1
  log_group_class   = "STANDARD"
}

# Trusted only by Pod Identity for this cluster and account, like the other
# add-on roles.
resource "aws_iam_role" "fluent_bit" {
  name        = "${local.name}-fluent-bit"
  description = "Fluent Bit in the llm-gateway cluster: writes the gateway's logs"
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

# Streams and events in this group only. Without CreateLogGroup, Fluent Bit
# cannot create a group Terraform would not destroy.
resource "aws_iam_role_policy" "fluent_bit" {
  name = "write-application-logs"
  role = aws_iam_role.fluent_bit.id
  policy = jsonencode({
    Version = "2012-10-17"
    Statement = [{
      Effect   = "Allow"
      Action   = ["logs:CreateLogStream", "logs:PutLogEvents"]
      Resource = [aws_cloudwatch_log_group.application.arn, "${aws_cloudwatch_log_group.application.arn}:*"]
    }]
  })
}

resource "aws_eks_pod_identity_association" "fluent_bit" {
  cluster_name    = module.eks.cluster_name
  namespace       = local.fluent_bit_namespace
  service_account = local.fluent_bit_service_account
  role_arn        = aws_iam_role.fluent_bit.arn
}
