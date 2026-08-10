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

# ---------------------------------------------------------------------------
# #102
# ---------------------------------------------------------------------------

output "web_url" {
  description = "The product. This is the only address a browser ever needs."
  value       = module.alb.web_url
}

output "api_url" {
  description = "Base URL of the Go API, including its non-default listener port. Resolves publicly and answers only callers in api_ingress_cidrs."
  value       = module.alb.api_url
}

output "alb_dns_name" {
  description = "The load balancer's own name. Both hostnames alias it; useful when the alias or the certificate is the suspect."
  value       = module.alb.dns_name
}

output "alb_idle_timeout_seconds" {
  description = "Seconds of silence before the load balancer closes a connection. If realtime works locally and not when deployed, check this against REALTIME_PING_INTERVAL first."
  value       = module.alb.idle_timeout_seconds
}

output "ecs_cluster_name" {
  description = "ECS cluster name. Every `aws ecs` command below needs it."
  value       = module.ecs.cluster_name
}

output "api_service_name" {
  description = "API service name, for #103's update-service."
  value       = module.ecs.api_service_name
}

output "web_service_name" {
  description = "Web service name."
  value       = module.ecs.web_service_name
}

output "api_repository_url" {
  description = "ECR repository #103 pushes the API image to."
  value       = module.ecs.api_repository_url
}

output "web_repository_url" {
  description = "ECR repository #103 pushes the web image to."
  value       = module.ecs.web_repository_url
}

output "migrate_task_definition_arn" {
  description = "Runs `api migrate up`. Once, to completion, before the service is rolled -- ADR 0013."
  value       = module.ecs.migrate_task_definition_arn
}

output "provision_task_definition_arn" {
  description = "Runs `api provision`, which sets the serving role's password to the value in Secrets Manager. After migrate, before the serving tasks are rolled."
  value       = module.ecs.provision_task_definition_arn
}

output "admin_task_definition_arn" {
  description = <<-EOT
    The one-shot psql session inside the VPC. This is what #56's
    `bootstrap-owner.sql` runs from, and the reason #56 is no longer blocked:
    after #101 there was no network path to the database from anywhere.

    It holds no credential. The operator reads the master password with their own
    IAM identity and gives it to `psql -W` at the prompt -- see ADR 0013 and
    OPERATOR-INPUTS.md.
  EOT
  value       = module.ecs.admin_task_definition_arn
}

output "run_task_network_configuration" {
  description = "Ready-made --network-configuration for `aws ecs run-task` with the migrate or provision task definition."
  value       = module.ecs.run_task_network_configuration
}

output "admin_run_task_network_configuration" {
  description = "Ready-made --network-configuration for the administrative task. Same subnets, a security group that reaches Postgres and nothing else."
  value       = module.ecs.admin_run_task_network_configuration
}

output "database_app_secret_arn" {
  description = "Secrets Manager container for the serving role's credential. Created empty by #102; #56 writes the value. A task started before that fails with ResourceInitializationError."
  value       = aws_secretsmanager_secret.database_app.arn
}

output "database_owner_secret_arn" {
  description = "Secrets Manager container for the schema owner's credential. Created empty; #56 writes the value."
  value       = aws_secretsmanager_secret.database_owner.arn
}

output "jwt_secret_arn" {
  description = "Secrets Manager container for AUTH_JWT_SECRET. Created empty; an operator writes it once. Not covered by #56."
  value       = aws_secretsmanager_secret.jwt.arn
}

output "alarm_topic_arn" {
  description = "SNS topic the load balancer alarms publish to. It has no subscription until an operator adds one -- Terraform cannot confirm an email subscription."
  value       = module.alb.alarm_topic_arn
}

output "private_subnet_cidrs" {
  description = <<-EOT
    Exported for #33. Gin currently calls SetTrustedProxies(nil), so behind a
    load balancer ClientIP() is the peer address rather than the caller's -- and
    the per-address login budget becomes one shared budget for the whole fleet.

    Note that fixing #33 with these CIDRs is necessary and not sufficient here:
    per ADR 0010 the browser never reaches the API directly, so even a correct
    trusted-proxy list yields the web task's address unless the web tier also
    forwards X-Forwarded-For.
  EOT
  value       = module.network.private_subnet_cidrs
}
