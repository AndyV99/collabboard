variable "name_prefix" {
  description = "Prefix for every resource name, e.g. `collabboard-staging`."
  type        = string
}

variable "vpc_id" {
  description = "VPC the security groups belong to."
  type        = string
}

variable "postgres_port" {
  description = "Port RDS Postgres listens on."
  type        = number
  default     = 5432
}

variable "redis_port" {
  description = "Port ElastiCache Redis listens on."
  type        = number
  default     = 6379
}
