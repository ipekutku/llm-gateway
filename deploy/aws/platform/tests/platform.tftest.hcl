# These plans exercise the module without credentials or AWS API calls. The
# mocked data sources return values shaped like the real ones, which the
# modules validate (ARNs and IAM policy JSON).
mock_provider "aws" {
  mock_data "aws_caller_identity" {
    defaults = {
      account_id = "123456789012"
      arn        = "arn:aws:sts::123456789012:assumed-role/operator/session"
    }
  }

  mock_data "aws_iam_session_context" {
    defaults = {
      issuer_arn = "arn:aws:iam::123456789012:role/operator"
    }
  }

  mock_data "aws_partition" {
    defaults = {
      partition  = "aws"
      dns_suffix = "amazonaws.com"
    }
  }

  mock_data "aws_iam_policy_document" {
    defaults = {
      json = "{\"Version\":\"2012-10-17\",\"Statement\":[]}"
    }
  }
}

variables {
  aws_account_id = "123456789012"
  admin_cidr     = "203.0.113.10/32"
}

run "platform" {
  command = plan

  assert {
    condition     = module.eks.cluster_version == "1.37"
    error_message = "The cluster must use the pinned standard-support Kubernetes version."
  }

  assert {
    condition = (
      module.eks.oidc_provider_arn == null &&
      module.eks.kms_key_arn == null &&
      module.eks.cloudwatch_log_group_name == null
    )
    error_message = "The project cannot create IAM OIDC providers, and a KMS key or control-plane log group would bill per session."
  }

  assert {
    condition = toset(keys(module.eks.cluster_addons)) == toset([
      "vpc-cni", "eks-pod-identity-agent", "kube-proxy", "coredns", "metrics-server"
    ])
    error_message = "The cluster must run exactly the expected add-ons."
  }

  assert {
    condition     = length(module.vpc.natgw_ids) == 1 && length(module.vpc.azs) == 2
    error_message = "The VPC must span two AZs with a single NAT gateway."
  }

  assert {
    condition = (
      aws_vpc_endpoint.s3.vpc_endpoint_type == "Gateway" &&
      aws_vpc_endpoint.s3.service_name == "com.amazonaws.eu-north-1.s3" &&
      length(aws_vpc_endpoint.s3.route_table_ids) == length(module.vpc.private_route_table_ids)
    )
    error_message = "The private subnets must reach S3 through the free gateway endpoint."
  }
}

run "rejects_open_api_access" {
  command = plan

  variables {
    admin_cidr = "0.0.0.0/0"
  }

  expect_failures = [var.admin_cidr]
}

run "rejects_wide_api_access" {
  command = plan

  variables {
    admin_cidr = "203.0.0.0/16"
  }

  expect_failures = [var.admin_cidr]
}

run "rejects_invalid_account" {
  command = plan

  variables {
    aws_account_id = "12345"
  }

  expect_failures = [var.aws_account_id]
}
