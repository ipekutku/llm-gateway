# Policy copied verbatim from the v3.6.0 controller release:
# https://github.com/kubernetes-sigs/aws-load-balancer-controller/blob/v3.6.0/docs/install/iam_policy.json
# Keep the policy, chart, and controller image versions aligned.
locals {
  load_balancer_controller_namespace       = "kube-system"
  load_balancer_controller_service_account = "aws-load-balancer-controller"
}

resource "aws_iam_role" "load_balancer_controller" {
  name        = "${local.name}-load-balancer-controller"
  description = "AWS Load Balancer Controller for the llm-gateway session"
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

resource "aws_iam_role_policy" "load_balancer_controller" {
  name   = "manage-session-load-balancers"
  role   = aws_iam_role.load_balancer_controller.id
  policy = file("${path.module}/load-balancer-controller-iam-policy.json")
}

resource "aws_eks_pod_identity_association" "load_balancer_controller" {
  cluster_name    = module.eks.cluster_name
  namespace       = local.load_balancer_controller_namespace
  service_account = local.load_balancer_controller_service_account
  role_arn        = aws_iam_role.load_balancer_controller.arn
}
