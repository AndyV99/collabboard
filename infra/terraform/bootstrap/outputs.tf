output "state_bucket_name" {
  description = "Name of the S3 bucket holding Terraform state for every environment."
  value       = aws_s3_bucket.state.id
}

output "state_kms_key_arn" {
  description = <<-EOT
    ARN of the KMS key that encrypts state. The ARN, not the alias: Terraform's
    S3 backend accepts a key ARN or a bare key ID for `kms_key_id` and rejects
    an `alias/...` string, and the bucket policy's DenyWrongKmsKey compares the
    request header against this exact ARN -- an alias in the header would not
    match it, and every state write and lock acquisition would 403.

    Key rotation is not a reason to prefer the alias here: enable_key_rotation
    rotates the backing key material, and the key ID and ARN are unchanged by it.
  EOT
  value       = aws_kms_key.state.arn
}

output "state_kms_key_alias" {
  description = "Human-friendly alias for the state key. For console use; the backend needs the ARN above."
  value       = aws_kms_alias.state.name
}

# The one piece of account-specific configuration an environment needs before it
# can init. Printed in ready-to-paste form so nobody has to assemble it by hand.
output "backend_config" {
  description = "Contents for infra/terraform/environments/<env>/backend.hcl."
  value       = <<-EOT
    bucket     = "${aws_s3_bucket.state.id}"
    kms_key_id = "${aws_kms_key.state.arn}"
  EOT
}

output "github_oidc_provider_arn" {
  description = <<-EOT
    ARN of the GitHub Actions OIDC provider (#103).

    Environments do not consume this output -- they cannot, because this stack
    keeps its state locally and no remote state data source can reach it. They
    look the provider up by URL with `data "aws_iam_openid_connect_provider"`
    instead. It is printed here so an operator can confirm the thing exists
    before wondering why a deploy role will not apply.
  EOT
  value       = aws_iam_openid_connect_provider.github.arn
}
