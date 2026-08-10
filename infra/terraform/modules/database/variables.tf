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

variable "apply_immediately" {
  description = "Apply modifications at once rather than in the next maintenance window. Can cause a restart."
  type        = bool
  default     = false
}
