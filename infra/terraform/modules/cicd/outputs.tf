output "deploy_role_arn" {
  description = "ARN of the role GitHub Actions assumes. Set this as the `AWS_DEPLOY_ROLE_ARN` repository variable -- a variable and not a secret, because a role ARN is not a credential and masking it in logs makes a failed AssumeRole impossible to debug."
  value       = aws_iam_role.deploy.arn
}

output "deploy_role_name" {
  description = "Name of the deploy role, for `aws iam simulate-principal-policy` in the runbook's verification steps."
  value       = aws_iam_role.deploy.name
}
