variable "name_prefix" {
  description = "Prefix for every resource name, e.g. `collabboard-staging`."
  type        = string
}

variable "subnet_ids" {
  description = "Data-tier subnet IDs."
  type        = list(string)
}

variable "security_group_ids" {
  description = "Security groups for the replication group. Expected to be the cache SG only."
  type        = list(string)
}

variable "kms_key_arn" {
  description = "CMK encrypting data at rest and snapshots."
  type        = string
}

variable "node_type" {
  description = "ElastiCache node type. cache.t4g.micro has 0.5 GiB, which is ample for refresh tokens, pub/sub and an Asynq queue at staging volume."
  type        = string
  default     = "cache.t4g.micro"
}

variable "node_count" {
  description = "Number of cache nodes (primary + replicas). 1 means no failover; every node above the first is a full node's cost."
  type        = number
  default     = 1

  validation {
    condition     = var.node_count >= 1
    error_message = "node_count must be at least 1."
  }
}

variable "snapshot_retention_days" {
  description = "Daily snapshot retention. 0 disables snapshots entirely, which for a single-node group means a node replacement starts empty."
  type        = number
  default     = 1
}

variable "apply_immediately" {
  description = "Apply modifications at once rather than in the next maintenance window."
  type        = bool
  default     = false
}
