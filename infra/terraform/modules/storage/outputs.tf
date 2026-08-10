output "bucket_name" {
  description = "Name of the attachments bucket."
  value       = aws_s3_bucket.attachments.id
}

output "bucket_arn" {
  description = "ARN of the attachments bucket. The IAM module scopes the task role's object permissions to it."
  value       = aws_s3_bucket.attachments.arn
}
