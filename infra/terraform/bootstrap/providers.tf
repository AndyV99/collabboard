provider "aws" {
  region = var.aws_region

  # Applied to every taggable resource this stack creates. `Environment = shared`
  # because the state backend is not per-environment: one bucket holds the state
  # of dev, staging and prod under different keys.
  default_tags {
    tags = {
      Project     = var.project
      Environment = "shared"
      Component   = "terraform-state"
      ManagedBy   = "terraform"
      Repository  = "AndyV99/collabboard"
      Terraform   = "infra/terraform/bootstrap"
    }
  }
}
