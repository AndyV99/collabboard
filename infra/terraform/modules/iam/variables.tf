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
  description = <<-EOT
    ECR repository namespace the execution role may pull from. The resulting
    ARN pattern is `repository/<prefix>/*`, which matches a SLASH-separated
    name such as `collabboard/api` and does NOT match a flat `collabboard-api`.

    That naming convention is invented here and #103 has to honour it. If #103
    creates flat repository names instead, the pull is denied and the symptom is
    an opaque CannotPullContainerError -- the same symptom as a missing NAT
    gateway, with an entirely different cause.
  EOT
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
