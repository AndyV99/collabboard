provider "aws" {
  region = var.aws_region

  # Applied to every taggable resource in this environment. Tagging is a
  # Security Practices requirement rather than housekeeping: `Environment` is
  # what makes a cost report attributable, and `Terraform` is what tells the
  # next person which directory to change instead of clicking in the console.
  default_tags {
    tags = {
      Project     = var.project
      Environment = var.environment
      ManagedBy   = "terraform"
      Repository  = "AndyV99/collabboard"
      Terraform   = "infra/terraform/environments/${var.environment}"
    }
  }
}
