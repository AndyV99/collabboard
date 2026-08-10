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

    Must be non-empty. There is no supported way to instantiate this module
    without the Deny.
  EOT
  type        = list(string)

  # An earlier version gated the Deny resources on `length(...) > 0`, which
  # meant an empty list produced no Deny, no error and no signal -- a safety
  # control that disappeared precisely when it had been misconfigured, which is
  # when it matters most. Same failure shape as #84, where NewRouter silently
  # serves no routes when its dependencies are nil.
  #
  # There is no legitimate empty case to preserve. This module exists to serve
  # a stack whose defining constraint is that the application must not reach the
  # RDS master credential; an instantiation with nothing to deny is not a
  # smaller version of that, it is a different module. So the gate is gone and
  # the resources are unconditional -- an empty list is now a plan-time error
  # rather than a quietly weaker deployment.
  validation {
    condition     = length(var.denied_secret_arns) > 0
    error_message = "denied_secret_arns must name at least one secret -- normally the RDS master-user secret ARN. ADR 0001's tenant isolation depends on neither ECS role being able to read a credential that bypasses row-level security, and an empty list would silently produce no Deny at all."
  }

  validation {
    condition     = alltrue([for arn in var.denied_secret_arns : startswith(arn, "arn:")])
    error_message = "denied_secret_arns must contain full ARNs. A bare secret name in a Deny matches nothing and fails open."
  }
}

variable "log_retention_days" {
  description = "CloudWatch Logs retention for the API and web log groups."
  type        = number
  default     = 30
}

variable "exec_log_retention_days" {
  description = "CloudWatch Logs retention for the ECS Exec session transcripts and the administrative task's own output. Longer than the service logs: this is the audit trail for every break-glass session against the database, and it is a few kilobytes a year."
  type        = number
  default     = 365
}
