output "app_security_group_id" {
  description = "Security group for the ECS API tasks. #102 attaches this to the service."
  value       = aws_security_group.app.id
}

output "database_security_group_id" {
  description = "Security group attached to the RDS instance."
  value       = aws_security_group.database.id
}

output "cache_security_group_id" {
  description = "Security group attached to the ElastiCache replication group."
  value       = aws_security_group.cache.id
}
