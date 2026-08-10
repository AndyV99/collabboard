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
