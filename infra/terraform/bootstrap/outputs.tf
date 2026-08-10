output "state_bucket_name" {
  description = "Name of the S3 bucket holding Terraform state for every environment."
  value       = aws_s3_bucket.state.id
}

output "state_kms_key_alias" {
  description = "Alias of the KMS key that encrypts state. Stable across key rotation, so environments reference the alias rather than the key ID."
  value       = aws_kms_alias.state.name
}

# The one piece of account-specific configuration an environment needs before it
# can init. Printed in ready-to-paste form so nobody has to assemble it by hand.
output "backend_config" {
  description = "Contents for infra/terraform/environments/<env>/backend.hcl."
  value       = <<-EOT
    bucket     = "${aws_s3_bucket.state.id}"
    kms_key_id = "${aws_kms_alias.state.name}"
  EOT
}
