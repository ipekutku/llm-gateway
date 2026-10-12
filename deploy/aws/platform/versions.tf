terraform {
  required_version = "= 1.16.5"

  # aws as in bootstrap. The others are required by the EKS module and its
  # submodules; pinning them here keeps the lock file reviewable instead of
  # taking whatever is newest.
  required_providers {
    aws = {
      source  = "hashicorp/aws"
      version = "= 6.68.0"
    }
    cloudinit = {
      source  = "hashicorp/cloudinit"
      version = "= 2.4.1"
    }
    null = {
      source  = "hashicorp/null"
      version = "= 3.3.2"
    }
    # The Valkey AUTH token (secrets.tf).
    random = {
      source  = "hashicorp/random"
      version = "= 3.9.1"
    }
    time = {
      source  = "hashicorp/time"
      version = "= 0.14.2"
    }
    tls = {
      source  = "hashicorp/tls"
      version = "= 4.4.1"
    }
  }
}
