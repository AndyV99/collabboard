output "address" {
  description = "DNS name of the instance. A hostname, not a credential -- it resolves only inside the VPC."
  value       = aws_db_instance.this.address
}

output "port" {
  description = "Port the instance listens on."
  value       = aws_db_instance.this.port
}

output "database_name" {
  description = "Name of the initial database."
  value       = aws_db_instance.this.db_name
}

output "instance_arn" {
  description = "ARN of the RDS instance."
  value       = aws_db_instance.this.arn
}

output "master_user_secret_arn" {
  description = <<-EOT
    ARN of the Secrets Manager secret RDS created and manages for the master
    user. Exported so the IAM module can Deny it by ARN, and so an operator can
    find it for the one-shot bootstrap in #56. The ARN is not sensitive; the
    secret's value is, and it is not available to Terraform at all.
  EOT
  value       = aws_db_instance.this.master_user_secret[0].secret_arn
}
