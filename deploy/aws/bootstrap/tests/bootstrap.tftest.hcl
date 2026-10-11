# These plans exercise the module without credentials or AWS API calls.
mock_provider "aws" {
  mock_resource "aws_s3_bucket" {
    override_during = plan
    defaults = {
      id  = "llm-gateway-tfstate-123456789012-eu-north-1"
      arn = "arn:aws:s3:::llm-gateway-tfstate-123456789012-eu-north-1"
    }
  }
}

variables {
  aws_account_id = "123456789012"
}

run "bootstrap" {
  command = plan

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

run "invalid_account" {
  command = plan
  variables {
    aws_account_id = "not-an-account"
  }
  expect_failures = [var.aws_account_id]
}
