variable "name_prefix" {
  description = "Prefix for every resource name, e.g. `collabboard-staging`."
  type        = string
}

variable "secret_name_prefix" {
  description = "Secrets Manager name prefix the execution role may read, e.g. `collabboard/staging/`. #56 creates the secrets underneath it."
  type        = string
}

variable "attachments_bucket_arn" {
  description = "ARN of the attachments bucket the task role may read and write."
  type        = string
}

variable "kms_key_arn" {
  description = "CMK the roles may use, constrained by kms:ViaService to the services that legitimately need it."
  type        = string
}

variable "ecr_repository_prefix" {
  description = "ECR repository namespace the execution role may pull from, e.g. `collabboard`. The repositories themselves arrive with #103."
  type        = string
}

variable "denied_secret_arns" {
  description = <<-EOT
    Secret ARNs both roles are explicitly denied, whatever else any policy says.
    The RDS master-user secret goes here: ADR 0001's tenant isolation depends on
    the application never connecting as a role that can bypass row-level
    security, and an explicit Deny is the only form of that rule which survives
    somebody later attaching a broader policy.
  EOT
  type        = list(string)
}

variable "log_retention_days" {
  description = "CloudWatch Logs retention for the API log group."
  type        = number
  default     = 30
}
