# State lives in the bootstrap bucket under its own key. The bucket and the
# expected account come from terraform init -backend-config (see docs/aws.md);
# CI initializes with -backend=false and never reads state.
terraform {
  backend "s3" {
    key          = "platform/terraform.tfstate"
    region       = "eu-north-1"
    encrypt      = true
    use_lockfile = true
  }
}
