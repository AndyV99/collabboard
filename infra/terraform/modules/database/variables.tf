variable "name_prefix" {
  description = "Prefix for every resource name, e.g. `collabboard-staging`."
  type        = string
}

variable "subnet_ids" {
  description = "Data-tier subnet IDs. Must span at least two AZs; RDS rejects a subnet group that does not."
  type        = list(string)
}

variable "security_group_ids" {
  description = "Security groups for the instance. Expected to be the database SG only."
  type        = list(string)
}

variable "kms_key_arn" {
  description = "CMK encrypting storage, automated backups, snapshots and the master-user secret."
  type        = string
}

variable "instance_class" {
  description = "RDS instance class. db.t4g.micro is the cheapest Graviton class that runs Postgres 16."
  type        = string
  default     = "db.t4g.micro"
}

variable "allocated_storage" {
  description = "Initial storage in GB. 20 is the gp3 minimum for RDS."
  type        = number
  default     = 20
}

variable "max_allocated_storage" {
  description = "Storage autoscaling ceiling in GB. Set equal to allocated_storage to disable. Only bills for what is actually used."
  type        = number
  default     = 100
}

variable "multi_az" {
  description = "Run a synchronous standby in a second AZ. Doubles the instance cost."
  type        = bool
  default     = false
}

variable "backup_retention_days" {
  description = "Automated backup retention. Backup storage up to the allocated size is free; beyond it bills per GB-month."
  type        = number
  default     = 7
}

variable "deletion_protection" {
  description = "Block deletion of the instance. True is the safe default; staging sets it false so `terraform destroy` can actually stop the bill."
  type        = bool
  default     = true
}

variable "skip_final_snapshot" {
  description = "Skip the snapshot taken on destroy. True discards the data; only appropriate where the data is reproducible."
  type        = bool
  default     = false
}

variable "performance_insights_enabled" {
  description = <<-EOT
    Enable Performance Insights. Defaults off because AWS does not support it on
    the burstable micro/small classes (db.t2/t3/t4g.micro and .small), and
    enabling it there fails CreateDBInstance rather than being ignored. Free at
    the 7-day retention this module uses, on classes that support it at all.
  EOT
  type        = bool
  default     = false

  # Turns a 15-minute apply failure into a plan-time error. Without this, the
  # combination is accepted by Terraform and rejected by CreateDBInstance with
  # InvalidParameterCombination, after the create has already started.
  validation {
    condition = (
      !var.performance_insights_enabled ||
      !contains(
        ["db.t2.micro", "db.t2.small", "db.t3.micro", "db.t3.small", "db.t4g.micro", "db.t4g.small"],
        var.instance_class,
      )
    )
    error_message = "Performance Insights is not supported on burstable micro/small instance classes. Use db.t4g.medium or larger, or leave performance_insights_enabled false."
  }
}

variable "apply_immediately" {
  description = "Apply modifications at once rather than in the next maintenance window. Can cause a restart."
  type        = bool
  default     = false
}
