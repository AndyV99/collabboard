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

output "alb_security_group_id" {
  description = "Security group for the load balancer. Port 443 is open to the internet; the API listener port is not."
  value       = aws_security_group.alb.id
}

output "web_security_group_id" {
  description = "Security group for the Next.js ECS tasks. Deliberately not admitted to Postgres or Redis."
  value       = aws_security_group.web.id
}

output "admin_security_group_id" {
  description = "Security group for the one-shot administrative task. Reaches Postgres and nothing else. See ADR 0013."
  value       = aws_security_group.admin.id
}
