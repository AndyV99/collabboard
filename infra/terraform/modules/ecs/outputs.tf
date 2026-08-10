output "cluster_name" {
  description = "ECS cluster name. Needed by every `aws ecs` command in OPERATOR-INPUTS.md and by #103."
  value       = aws_ecs_cluster.this.name
}

output "cluster_arn" {
  description = "ECS cluster ARN."
  value       = aws_ecs_cluster.this.arn
}

output "api_service_name" {
  description = "API service name, for `update-service` in #103."
  value       = aws_ecs_service.api.name
}

output "web_service_name" {
  description = "Web service name."
  value       = aws_ecs_service.web.name
}

output "api_repository_url" {
  description = "ECR repository #103 pushes the API image to."
  value       = aws_ecr_repository.api.repository_url
}

output "web_repository_url" {
  description = "ECR repository #103 pushes the web image to."
  value       = aws_ecr_repository.web.repository_url
}

output "api_task_definition_arn" {
  description = "Current API task definition revision. #103 derives its own revision from this one by replacing the image and nothing else, which is what keeps Terraform the owner of the task's shape."
  value       = aws_ecs_task_definition.api.arn
}

output "migrate_task_definition_arn" {
  description = "Task definition that runs `api migrate up`. Run it once, to completion, before rolling the service -- see ADR 0013."
  value       = aws_ecs_task_definition.api_migrate.arn
}

output "provision_task_definition_arn" {
  description = "Task definition that runs `api provision`. Run it after `migrate up` and before the serving tasks pick up a rotated password."
  value       = aws_ecs_task_definition.api_provision.arn
}

output "admin_task_definition_arn" {
  description = "One-shot administrative task: a psql session inside the VPC. This is what #56's bootstrap-owner.sql runs from."
  value       = aws_ecs_task_definition.admin.arn
}

output "run_task_network_configuration" {
  description = <<-EOT
    The `--network-configuration` argument for `aws ecs run-task` with the
    migrate or provision task definition, rendered so that an operator does not
    have to assemble subnet and security group IDs by hand. The administrative
    task uses the admin security group instead -- see OPERATOR-INPUTS.md.
  EOT
  value = format(
    "awsvpcConfiguration={subnets=[%s],securityGroups=[%s],assignPublicIp=DISABLED}",
    join(",", var.private_subnet_ids),
    var.app_security_group_id,
  )
}

output "admin_run_task_network_configuration" {
  description = "The `--network-configuration` argument for running the administrative task. Same subnets, admin security group -- which reaches Postgres and nothing else."
  value = format(
    "awsvpcConfiguration={subnets=[%s],securityGroups=[%s],assignPublicIp=DISABLED}",
    join(",", var.private_subnet_ids),
    var.admin_security_group_id,
  )
}
