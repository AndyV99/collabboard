# Consumed by #102 (ECS + ALB) and #56 (role provisioning), either by reading
# them with `terraform output` or via a `terraform_remote_state` data source.
#
# None of these is a credential. Endpoints are private DNS names that resolve
# only inside the VPC, and ARNs identify secrets without revealing them.

output "vpc_id" {
  description = "VPC ID."
  value       = module.network.vpc_id
}

output "public_subnet_ids" {
  description = "Public subnets, for the ALB."
  value       = module.network.public_subnet_ids
}

output "private_subnet_ids" {
  description = "Private subnets, for ECS tasks."
  value       = module.network.private_subnet_ids
}

output "data_subnet_ids" {
  description = "Data subnets. No egress; RDS and ElastiCache only."
  value       = module.network.data_subnet_ids
}

output "app_security_group_id" {
  description = "Attach this to the ECS service. It is the source of the only ingress rule on the database and cache."
  value       = module.security_groups.app_security_group_id
}

output "nat_gateway_public_ips" {
  description = "Addresses this environment egresses from. Empty when nat_gateway_count is 0."
  value       = module.network.nat_gateway_public_ips
}

output "database_address" {
  description = "RDS endpoint hostname."
  value       = module.database.address
}

output "database_port" {
  description = "RDS port."
  value       = module.database.port
}

output "database_name" {
  description = "Initial database name."
  value       = module.database.database_name
}

output "database_master_user_secret_arn" {
  description = <<-EOT
    Secrets Manager ARN of the RDS-managed master credential. Needed by a human
    operator exactly once, to run `bootstrap-owner.sql` per #56. Both ECS roles
    are explicitly Denied it.
  EOT
  value       = module.database.master_user_secret_arn
}

output "cache_primary_endpoint" {
  description = "ElastiCache primary endpoint. TLS is required -- see cache_transit_encryption_enabled."
  value       = module.cache.primary_endpoint_address
}

output "cache_port" {
  description = "ElastiCache port."
  value       = module.cache.port
}

output "cache_transit_encryption_enabled" {
  description = "True. A plaintext Redis client will not connect; #102 must use rediss:// or an equivalent TLS config."
  value       = module.cache.transit_encryption_enabled
}

output "attachments_bucket_name" {
  description = "S3 bucket for card attachments."
  value       = module.storage.bucket_name
}

output "ecs_task_execution_role_arn" {
  description = "executionRoleArn for the task definition in #102."
  value       = module.iam.execution_role_arn
}

output "ecs_task_role_arn" {
  description = "taskRoleArn for the task definition in #102."
  value       = module.iam.task_role_arn
}

output "api_log_group_name" {
  description = "CloudWatch Logs group for the API container."
  value       = module.iam.api_log_group_name
}

output "secret_name_prefix" {
  description = "Secrets Manager namespace the execution role can read. #56 creates its secrets under this prefix."
  value       = local.secret_name_prefix
}

output "kms_key_arn" {
  description = "CMK encrypting this environment's data at rest."
  value       = aws_kms_key.data.arn
}
