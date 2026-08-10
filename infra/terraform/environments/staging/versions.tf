terraform {
  required_version = ">= 1.10.0"

  required_providers {
    aws = {
      source  = "hashicorp/aws"
      version = "~> 6.58"
    }
  }

  # Partial configuration. `bucket` and `kms_key_id` are account-specific and
  # cannot be interpolated -- backend blocks accept no variables, functions or
  # references -- so they come from backend.hcl at init time:
  #
  #   terraform init -backend-config=backend.hcl
  #
  # `region` is duplicated from var.aws_region for the same reason. If one
  # changes, change both.
  backend "s3" {
    key     = "staging/base/terraform.tfstate"
    region  = "us-east-1"
    encrypt = true

    # S3-native locking, which replaces the DynamoDB lock table entirely: the
    # lock is a conditional PUT of `<key>.tflock` in the same bucket. One fewer
    # resource, one fewer thing to pay for, and no way for the lock table and
    # the state bucket to be granted to different people. Requires Terraform
    # >= 1.10. See ADR 0011.
    use_lockfile = true
  }
}
