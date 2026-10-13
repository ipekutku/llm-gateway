# These plans exercise the module without credentials or AWS API calls.
mock_provider "aws" {
  mock_resource "aws_s3_bucket" {
    override_during = plan
    defaults = {
      id  = "llm-gateway-tfstate-123456789012-eu-north-1"
      arn = "arn:aws:s3:::llm-gateway-tfstate-123456789012-eu-north-1"
    }
  }

  mock_resource "aws_route53_zone" {
    override_during = plan
    defaults = {
      zone_id      = "Z1234567890"
      name_servers = ["ns-1.awsdns-01.com", "ns-2.awsdns-02.net", "ns-3.awsdns-03.org", "ns-4.awsdns-04.co.uk"]
    }
  }

  mock_resource "aws_acm_certificate" {
    override_during = plan
    defaults = {
      arn = "arn:aws:acm:eu-north-1:123456789012:certificate/12345678-1234-1234-1234-123456789012"
      domain_validation_options = [{
        domain_name           = "llm-gateway.example.com"
        resource_record_name  = "_token.llm-gateway.example.com."
        resource_record_type  = "CNAME"
        resource_record_value = "_token.acm-validations.aws."
      }]
    }
  }
}

variables {
  aws_account_id = "123456789012"
}

run "bootstrap" {
  command = plan

  # Do not inherit an operator's TF_VAR_domain_name during local checks.
  variables {
    domain_name = ""
  }

  assert {
    condition = (
      length(aws_route53_zone.gateway) == 0 &&
      length(aws_acm_certificate.gateway) == 0 &&
      length(aws_route53_record.certificate_validation) == 0 &&
      output.gateway_hosted_zone_id == null &&
      length(output.gateway_name_servers) == 0 &&
      output.gateway_certificate_arn == null
    )
    error_message = "An empty domain must preserve the state/ECR-only bootstrap without DNS or certificate resources."
  }

  assert {
    condition = (
      aws_s3_bucket.state.bucket == "llm-gateway-tfstate-123456789012-eu-north-1" &&
      !aws_s3_bucket.state.force_destroy &&
      aws_s3_bucket_versioning.state.versioning_configuration[0].status == "Enabled" &&
      one(aws_s3_bucket_server_side_encryption_configuration.state.rule).apply_server_side_encryption_by_default[0].sse_algorithm == "AES256" &&
      aws_s3_bucket_ownership_controls.state.rule[0].object_ownership == "BucketOwnerEnforced"
    )
    error_message = "State storage must be versioned, encrypted, owned by the bucket owner, and never force-emptied."
  }

  assert {
    condition = (
      jsondecode(aws_s3_bucket_policy.state.policy).Statement[0].Effect == "Deny" &&
      jsondecode(aws_s3_bucket_policy.state.policy).Statement[0].Principal == "*" &&
      jsondecode(aws_s3_bucket_policy.state.policy).Statement[0].Action == "s3:*" &&
      jsondecode(aws_s3_bucket_policy.state.policy).Statement[0].Condition.Bool["aws:SecureTransport"] == "false" &&
      jsondecode(aws_s3_bucket_policy.state.policy).Statement[0].Condition.Bool["aws:PrincipalIsAWSService"] == "false" &&
      toset(jsondecode(aws_s3_bucket_policy.state.policy).Statement[0].Resource) == toset([
        aws_s3_bucket.state.arn, "${aws_s3_bucket.state.arn}/*"
      ])
    )
    error_message = "The bucket policy must deny insecure transport on the bucket and its objects, excluding AWS service principals."
  }

  assert {
    condition = (
      aws_s3_bucket_public_access_block.state.block_public_acls &&
      aws_s3_bucket_public_access_block.state.block_public_policy &&
      aws_s3_bucket_public_access_block.state.ignore_public_acls &&
      aws_s3_bucket_public_access_block.state.restrict_public_buckets
    )
    error_message = "All four public-access protections must be enabled."
  }

  assert {
    condition = alltrue([for repository in aws_ecr_repository.images :
      repository.image_tag_mutability == "IMMUTABLE" &&
      repository.image_scanning_configuration[0].scan_on_push &&
      repository.encryption_configuration[0].encryption_type == "AES256" &&
      !repository.force_delete
    ]) && toset(keys(aws_ecr_repository.images)) == toset(["llm-gateway", "llm-gateway-fakeprovider"])
    error_message = "Both private repositories must have immutable tags, scanning, encryption, and no forced deletion."
  }

  assert {
    condition = alltrue([for policy in aws_ecr_lifecycle_policy.images :
      jsondecode(policy.policy).rules[0].selection.tagStatus == "any" &&
      jsondecode(policy.policy).rules[0].selection.countType == "imageCountMoreThan" &&
      jsondecode(policy.policy).rules[0].selection.countNumber == 10 &&
      jsondecode(policy.policy).rules[0].action.type == "expire"
    ]) && toset(keys(aws_ecr_lifecycle_policy.images)) == toset(keys(aws_ecr_repository.images))
    error_message = "Each repository must retain its newest 10 images regardless of tags."
  }
}

run "gateway_dns" {
  command = plan
  variables {
    domain_name = "llm-gateway.example.com"
  }

  assert {
    condition = (
      length(aws_route53_zone.gateway) == 1 &&
      aws_route53_zone.gateway[0].name == "llm-gateway.example.com" &&
      length(aws_route53_zone.gateway[0].vpc) == 0 &&
      !aws_route53_zone.gateway[0].force_destroy
    )
    error_message = "The gateway must have one public child zone without forced record deletion."
  }

  assert {
    condition = (
      length(aws_acm_certificate.gateway) == 1 &&
      aws_acm_certificate.gateway[0].domain_name == "llm-gateway.example.com" &&
      aws_acm_certificate.gateway[0].validation_method == "DNS" &&
      aws_acm_certificate.gateway[0].key_algorithm == "RSA_2048" &&
      one(aws_acm_certificate.gateway[0].options).certificate_transparency_logging_preference == "ENABLED" &&
      one(aws_acm_certificate.gateway[0].options).export == "DISABLED"
    )
    error_message = "Request one DNS-validated, non-exportable public certificate for the gateway hostname."
  }

  assert {
    condition = (
      length(aws_route53_record.certificate_validation) == 1 &&
      aws_route53_record.certificate_validation[0].zone_id == "Z1234567890" &&
      aws_route53_record.certificate_validation[0].name == "_token.llm-gateway.example.com." &&
      aws_route53_record.certificate_validation[0].type == "CNAME" &&
      aws_route53_record.certificate_validation[0].records == toset(["_token.acm-validations.aws."]) &&
      aws_route53_record.certificate_validation[0].ttl == 300
    )
    error_message = "Publish ACM's exact validation CNAME in the child zone."
  }

  assert {
    condition = (
      output.gateway_hosted_zone_id == "Z1234567890" &&
      toset(output.gateway_name_servers) == toset(["ns-1.awsdns-01.com", "ns-2.awsdns-02.net", "ns-3.awsdns-03.org", "ns-4.awsdns-04.co.uk"]) &&
      output.gateway_certificate_arn == "arn:aws:acm:eu-north-1:123456789012:certificate/12345678-1234-1234-1234-123456789012"
    )
    error_message = "Outputs must identify the zone, all four delegation servers, and the regional certificate request."
  }
}

run "invalid_domain_wildcard" {
  command = plan
  variables {
    domain_name = "*.example.com"
  }
  expect_failures = [var.domain_name]
}

run "invalid_domain_trailing_dot" {
  command = plan
  variables {
    domain_name = "llm-gateway.example.com."
  }
  expect_failures = [var.domain_name]
}

run "invalid_domain_uppercase" {
  command = plan
  variables {
    domain_name = "LLM-gateway.example.com"
  }
  expect_failures = [var.domain_name]
}

run "invalid_domain_label" {
  command = plan
  variables {
    domain_name = "-gateway.example.com"
  }
  expect_failures = [var.domain_name]
}

run "invalid_domain_empty_label" {
  command = plan
  variables {
    domain_name = "llm-gateway..example.com"
  }
  expect_failures = [var.domain_name]
}

run "invalid_domain_long_label" {
  command = plan
  variables {
    domain_name = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa.example.com"
  }
  expect_failures = [var.domain_name]
}

run "invalid_domain_total_length" {
  command = plan
  variables {
    domain_name = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa.aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa.aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa.aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa.com"
  }
  expect_failures = [var.domain_name]
}

run "valid_domain_maximum_length" {
  command = plan
  variables {
    domain_name = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa.aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa.aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa.aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
  }
}

run "invalid_account" {
  command = plan
  variables {
    aws_account_id = "not-an-account"
  }
  expect_failures = [var.aws_account_id]
}
