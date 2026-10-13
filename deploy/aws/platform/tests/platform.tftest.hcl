# These runs exercise the module without credentials or AWS API calls. The
# mocked data sources return values shaped like the real ones, which the
# modules validate (ARNs and IAM policy JSON). The first run applies against
# the mocks, so generated IDs are known and assertions can compare them across
# resources, such as a security group rule's source.
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

  mock_data "aws_eks_addon_version" {
    defaults = {
      version = "v1.0.0-eksbuild.1"
    }
  }

  mock_resource "aws_eks_cluster" {
    defaults = {
      arn                   = "arn:aws:eks:eu-north-1:123456789012:cluster/llm-gateway"
      endpoint              = "https://mock.eks.eu-north-1.amazonaws.com"
      certificate_authority = [{ data = "bW9jaw==" }]
    }
  }

  mock_resource "aws_launch_template" {
    defaults = {
      id = "lt-0123456789abcdef0"
    }
  }

  mock_resource "aws_iam_role" {
    defaults = {
      arn = "arn:aws:iam::123456789012:role/mock"
    }
  }

  mock_resource "aws_cloudwatch_log_group" {
    defaults = {
      arn = "arn:aws:logs:eu-north-1:123456789012:log-group:/llm-gateway/application"
    }
  }

  mock_resource "aws_db_instance" {
    defaults = {
      master_user_secret = [{
        kms_key_id    = "arn:aws:kms:eu-north-1:123456789012:key/mock"
        secret_arn    = "arn:aws:secretsmanager:eu-north-1:123456789012:secret:rds!db-mock"
        secret_status = "active"
      }]
    }
  }
}

variables {
  aws_account_id = "123456789012"
  admin_cidr     = "203.0.113.10/32"
}

run "platform" {
  command = apply

  assert {
    condition = jsondecode(aws_iam_role.load_balancer_controller.assume_role_policy).Statement == [{
      Effect    = "Allow"
      Principal = { Service = "pods.eks.amazonaws.com" }
      Action    = ["sts:AssumeRole", "sts:TagSession"]
      Condition = {
        StringEquals = { "aws:SourceAccount" = "123456789012" }
        ArnEquals    = { "aws:SourceArn" = module.eks.cluster_arn }
      }
    }]
    error_message = "Only Pod Identity in this cluster and account may assume the controller role."
  }

  assert {
    condition = (
      aws_eks_pod_identity_association.load_balancer_controller.namespace == "kube-system" &&
      aws_eks_pod_identity_association.load_balancer_controller.service_account == "aws-load-balancer-controller" &&
      aws_eks_pod_identity_association.load_balancer_controller.cluster_name == module.eks.cluster_name &&
      aws_eks_pod_identity_association.load_balancer_controller.role_arn == aws_iam_role.load_balancer_controller.arn &&
      aws_iam_role_policy.load_balancer_controller.role == aws_iam_role.load_balancer_controller.id &&
      jsondecode(aws_iam_role_policy.load_balancer_controller.policy) == jsondecode(file("./load-balancer-controller-iam-policy.json"))
    )
    error_message = "The pinned controller policy must be attached only to its Pod Identity role."
  }

  assert {
    condition = (
      alltrue([for subnet in module.vpc.public_subnet_objects : subnet.tags["kubernetes.io/role/elb"] == "1"]) &&
      alltrue([for subnet in module.vpc.private_subnet_objects : subnet.tags["kubernetes.io/role/internal-elb"] == "1"]) &&
      output.vpc_id == module.vpc.vpc_id && output.admin_cidr == "203.0.113.10/32"
    )
    error_message = "Subnet discovery tags and explicit VPC/CIDR outputs must support controller setup."
  }

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

  assert {
    condition = (
      aws_db_instance.postgres.engine == "postgres" &&
      startswith(aws_db_instance.postgres.engine_version, "18.") &&
      aws_db_instance.postgres.instance_class == "db.t4g.micro" &&
      !aws_db_instance.postgres.multi_az &&
      !aws_db_instance.postgres.publicly_accessible &&
      aws_db_instance.postgres.storage_encrypted &&
      aws_db_instance.postgres.parameter_group_name == aws_db_parameter_group.postgres.name &&
      aws_db_instance.postgres.db_subnet_group_name == aws_db_subnet_group.postgres.name &&
      toset(aws_db_instance.postgres.vpc_security_group_ids) == toset([aws_security_group.postgres.id])
    )
    error_message = "PostgreSQL must be 18 on the Free plan's db.t4g.micro, Single-AZ, private, and encrypted."
  }

  assert {
    condition = (
      aws_db_instance.postgres.manage_master_user_password &&
      aws_db_instance.postgres.password == null &&
      aws_db_instance.postgres.password_wo == null
    )
    error_message = "RDS must manage the master password, so it never reaches configuration or state."
  }

  assert {
    condition = anytrue([
      for parameter in aws_db_parameter_group.postgres.parameter :
      parameter.name == "rds.force_ssl" && parameter.value == "1" && parameter.apply_method == "pending-reboot"
    ])
    error_message = "PostgreSQL must require TLS with the static parameter's pending-reboot apply method."
  }

  assert {
    condition = (
      aws_db_instance.postgres.backup_retention_period == 0 &&
      aws_db_instance.postgres.skip_final_snapshot &&
      aws_db_instance.postgres.delete_automated_backups &&
      !aws_db_instance.postgres.deletion_protection &&
      !aws_db_instance.postgres.performance_insights_enabled
    )
    error_message = "The session database must leave no backup or snapshot behind and bill nothing extra."
  }

  assert {
    condition = (
      aws_elasticache_replication_group.redis.engine == "valkey" &&
      aws_elasticache_replication_group.redis.node_type == "cache.t4g.micro" &&
      aws_elasticache_replication_group.redis.num_cache_clusters == 1 &&
      aws_elasticache_replication_group.redis.cluster_mode == "disabled" &&
      !aws_elasticache_replication_group.redis.automatic_failover_enabled &&
      !aws_elasticache_replication_group.redis.multi_az_enabled &&
      aws_elasticache_replication_group.redis.snapshot_retention_limit == 0
    )
    error_message = "Valkey must be a single cache.t4g.micro node without snapshots."
  }

  assert {
    condition = (
      aws_elasticache_replication_group.redis.transit_encryption_enabled &&
      aws_elasticache_replication_group.redis.transit_encryption_mode == "required" &&
      aws_elasticache_replication_group.redis.at_rest_encryption_enabled &&
      aws_elasticache_replication_group.redis.auth_token == random_password.redis_auth_token.result &&
      aws_elasticache_replication_group.redis.auth_token_update_strategy == "SET" &&
      aws_secretsmanager_secret_version.redis_auth_token.secret_string == random_password.redis_auth_token.result &&
      aws_secretsmanager_secret_version.redis_auth_token.secret_id == aws_secretsmanager_secret.redis_auth_token.id &&
      aws_elasticache_replication_group.redis.subnet_group_name == aws_elasticache_subnet_group.redis.name &&
      toset(aws_elasticache_replication_group.redis.security_group_ids) == toset([aws_security_group.redis.id])
    )
    error_message = "Valkey must require TLS and encrypt its data."
  }

  assert {
    condition = (
      random_password.redis_auth_token.length == 64 &&
      !random_password.redis_auth_token.special
    )
    error_message = "The AUTH token must be long and alphanumeric, so it fits a rediss:// URL unescaped."
  }

  assert {
    condition = (
      aws_vpc_security_group_ingress_rule.postgres_from_nodes.referenced_security_group_id == module.eks.node_security_group_id &&
      aws_vpc_security_group_ingress_rule.postgres_from_nodes.security_group_id == aws_security_group.postgres.id &&
      aws_vpc_security_group_ingress_rule.postgres_from_nodes.ip_protocol == "tcp" &&
      aws_vpc_security_group_ingress_rule.postgres_from_nodes.from_port == 5432 &&
      aws_vpc_security_group_ingress_rule.postgres_from_nodes.to_port == 5432 &&
      aws_vpc_security_group_ingress_rule.redis_from_nodes.referenced_security_group_id == module.eks.node_security_group_id &&
      aws_vpc_security_group_ingress_rule.redis_from_nodes.security_group_id == aws_security_group.redis.id &&
      aws_vpc_security_group_ingress_rule.redis_from_nodes.ip_protocol == "tcp" &&
      aws_vpc_security_group_ingress_rule.redis_from_nodes.from_port == 6379 &&
      aws_vpc_security_group_ingress_rule.redis_from_nodes.to_port == 6379
    )
    error_message = "The data stores must accept only their own port, and only from the nodes."
  }

  assert {
    condition = (
      aws_db_subnet_group.postgres.subnet_ids == toset(module.vpc.private_subnets) &&
      aws_elasticache_subnet_group.redis.subnet_ids == toset(module.vpc.private_subnets)
    )
    error_message = "The data stores must live in the private subnets."
  }

  assert {
    condition = alltrue(concat(
      [for secret in aws_secretsmanager_secret.operator : secret.recovery_window_in_days == 0],
      [aws_secretsmanager_secret.redis_auth_token.recovery_window_in_days == 0]
    ))
    error_message = "Session secrets must be deleted without a recovery window, so the next session can recreate them."
  }

  assert {
    condition = jsondecode(aws_iam_role.external_secrets.assume_role_policy).Statement == [{
      Effect    = "Allow"
      Principal = { Service = "pods.eks.amazonaws.com" }
      Action    = ["sts:AssumeRole", "sts:TagSession"]
      Condition = {
        StringEquals = { "aws:SourceAccount" = "123456789012" }
        ArnEquals    = { "aws:SourceArn" = module.eks.cluster_arn }
      }
    }]
    error_message = "Only EKS Pod Identity may assume the External Secrets role, and only for this cluster and account."
  }

  assert {
    condition = (
      length(jsondecode(aws_iam_role_policy.external_secrets.policy).Statement) == 1 &&
      toset(jsondecode(aws_iam_role_policy.external_secrets.policy).Statement[0].Action) == toset([
        "secretsmanager:GetSecretValue", "secretsmanager:DescribeSecret"
      ]) &&
      toset(jsondecode(aws_iam_role_policy.external_secrets.policy).Statement[0].Resource) == toset(concat(
        [for secret in aws_secretsmanager_secret.operator : secret.arn],
        [aws_secretsmanager_secret.redis_auth_token.arn, "arn:aws:secretsmanager:eu-north-1:123456789012:secret:rds!db-mock"]
      )) &&
      aws_iam_role_policy.external_secrets.role == aws_iam_role.external_secrets.id
    )
    error_message = "External Secrets may only read the platform's own secrets, listed by ARN."
  }

  assert {
    condition = (
      aws_eks_pod_identity_association.external_secrets.cluster_name == module.eks.cluster_name &&
      aws_eks_pod_identity_association.external_secrets.namespace == "external-secrets" &&
      aws_eks_pod_identity_association.external_secrets.service_account == "external-secrets" &&
      aws_eks_pod_identity_association.external_secrets.role_arn == aws_iam_role.external_secrets.arn
    )
    error_message = "The External Secrets role must belong to the operator's service account only."
  }

  assert {
    condition = toset(keys(aws_secretsmanager_secret.operator)) == toset([
      "openai-api-key", "anthropic-api-key", "clients", "database-app-password"
    ])
    error_message = "Terraform must create only containers for the values written outside it."
  }

  assert {
    condition = (
      aws_cloudwatch_log_group.application.name == "/llm-gateway/application" &&
      aws_cloudwatch_log_group.application.retention_in_days == 1 &&
      aws_cloudwatch_log_group.application.log_group_class == "STANDARD" &&
      output.application_log_group == aws_cloudwatch_log_group.application.name
    )
    error_message = "The application log group must keep events for one day."
  }

  assert {
    condition = jsondecode(aws_iam_role.fluent_bit.assume_role_policy).Statement == [{
      Effect    = "Allow"
      Principal = { Service = "pods.eks.amazonaws.com" }
      Action    = ["sts:AssumeRole", "sts:TagSession"]
      Condition = {
        StringEquals = { "aws:SourceAccount" = "123456789012" }
        ArnEquals    = { "aws:SourceArn" = module.eks.cluster_arn }
      }
    }]
    error_message = "Only EKS Pod Identity may assume the Fluent Bit role, and only for this cluster and account."
  }

  assert {
    condition = (
      jsondecode(aws_iam_role_policy.fluent_bit.policy).Statement == [{
        Effect = "Allow"
        Action = ["logs:CreateLogStream", "logs:PutLogEvents"]
        Resource = [
          "arn:aws:logs:eu-north-1:123456789012:log-group:/llm-gateway/application",
          "arn:aws:logs:eu-north-1:123456789012:log-group:/llm-gateway/application:*",
        ]
      }] &&
      aws_iam_role_policy.fluent_bit.role == aws_iam_role.fluent_bit.id
    )
    error_message = "Fluent Bit may only create streams and write events in the application log group, never create groups."
  }

  assert {
    condition = (
      aws_eks_pod_identity_association.fluent_bit.cluster_name == module.eks.cluster_name &&
      aws_eks_pod_identity_association.fluent_bit.namespace == "fluent-bit" &&
      aws_eks_pod_identity_association.fluent_bit.service_account == "fluent-bit" &&
      aws_eks_pod_identity_association.fluent_bit.role_arn == aws_iam_role.fluent_bit.arn
    )
    error_message = "The Fluent Bit role must belong to the Fluent Bit service account only."
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
