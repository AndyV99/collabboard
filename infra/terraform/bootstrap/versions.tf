terraform {
  # 1.10 is the floor because this project uses S3-native state locking
  # (`use_lockfile`) instead of a DynamoDB lock table. See ADR 0011.
  required_version = ">= 1.10.0"

  required_providers {
    aws = {
      source  = "hashicorp/aws"
      version = "~> 6.58"
    }
  }

  # Deliberately no `backend` block. This stack CREATES the remote backend, so
  # it cannot store its state in it. Its state is local and gitignored; see the
  # README for what that does and does not mean.
}
