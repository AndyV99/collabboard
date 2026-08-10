variable "aws_region" {
  description = "Region the state bucket lives in. Must match the `region` in every environment's backend block."
  type        = string
  default     = "us-east-1"
}

variable "project" {
  description = "Project name, used as a resource name prefix and as the `Project` tag."
  type        = string
  default     = "collabboard"
}
