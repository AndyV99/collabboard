output "execution_role_arn" {
  description = "ECS task execution role. #102 sets this as the task definition's executionRoleArn."
  value       = aws_iam_role.execution.arn
}

output "task_role_arn" {
  description = "ECS task role. #102 sets this as the task definition's taskRoleArn."
  value       = aws_iam_role.task.arn
}

output "api_log_group_name" {
  description = "CloudWatch Logs group for the API container's awslogs driver."
  value       = aws_cloudwatch_log_group.api.name
}

output "web_execution_role_arn" {
  description = "Execution role for the Next.js task definition. Cannot read this environment's secrets."
  value       = aws_iam_role.web_execution.arn
}

output "web_log_group_name" {
  description = "CloudWatch Logs group for the web container's awslogs driver."
  value       = aws_cloudwatch_log_group.web.name
}

output "admin_execution_role_arn" {
  description = "Execution role for the one-shot administrative task."
  value       = aws_iam_role.admin_execution.arn
}

output "admin_task_role_arn" {
  description = "Task role for the one-shot administrative task: ECS Exec plus session logging, and no secret access at all."
  value       = aws_iam_role.admin_task.arn
}

output "admin_log_group_name" {
  description = "CloudWatch Logs group for the administrative task's container output."
  value       = aws_cloudwatch_log_group.admin.name
}

output "exec_log_group_name" {
  description = "CloudWatch Logs group holding ECS Exec session transcripts. Configured on the cluster in the ecs module."
  value       = aws_cloudwatch_log_group.exec.name
}
