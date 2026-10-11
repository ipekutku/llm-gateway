provider "aws" {
  region              = "eu-north-1"
  allowed_account_ids = [var.aws_account_id]

  default_tags {
    tags = {
      Project = "llm-gateway"
      Stack   = "bootstrap"
    }
  }
}
