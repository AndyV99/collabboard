# Remote state backend for every CollabBoard environment.
#
# Run once, by hand, before any environment can `terraform init`. See ADR 0011
# for why the backend is its own stack and why there is no DynamoDB table.

data "aws_caller_identity" "current" {}

locals {
  # S3 bucket names are globally unique across all AWS accounts, so the account
  # ID is appended to make the name deterministic without being a guess. This is
  # also why the bucket name cannot be hardcoded in an environment's `backend`
  # block ahead of time -- see `terraform output backend_config`.
  state_bucket_name = "${var.project}-tfstate-${data.aws_caller_identity.current.account_id}"
}

# --------------------------------------------------------------------------
# KMS
# --------------------------------------------------------------------------

# Terraform state contains secrets by definition -- resource attributes that are
# sensitive in the plan are plaintext in the state file. A customer-managed key
# rather than the AWS-managed `aws/s3` key because the key policy is the
# enforceable answer to "who can read state": revoking access to this key
# revokes the ability to decrypt every state file, independently of the bucket
# policy and of S3 permissions.
resource "aws_kms_key" "state" {
  description             = "Encrypts CollabBoard Terraform state at rest"
  deletion_window_in_days = 30
  enable_key_rotation     = true

  # No explicit `policy`. AWS's default key policy grants the account root full
  # control and thereby delegates authorization to IAM, which is what we want
  # while the only principal in this account is the human operator. When #103
  # introduces a GitHub OIDC deploy role, narrow this to that role plus the
  # operator rather than leaving it delegating to IAM broadly.

  lifecycle {
    prevent_destroy = true
  }
}

resource "aws_kms_alias" "state" {
  name          = "alias/${var.project}-tfstate"
  target_key_id = aws_kms_key.state.key_id
}

# --------------------------------------------------------------------------
# State bucket
# --------------------------------------------------------------------------

resource "aws_s3_bucket" "state" {
  bucket = local.state_bucket_name

  # Not force_destroy: emptying this bucket destroys the record of everything
  # Terraform has built. Deleting it has to be a deliberate, manual act.
  force_destroy = false

  lifecycle {
    prevent_destroy = true
  }
}

# Versioning is the recovery path for the failure this backend actually has: a
# corrupt or truncated state write, or an `apply` that recorded the wrong thing.
# Without it there is no way back to the previous state.
resource "aws_s3_bucket_versioning" "state" {
  bucket = aws_s3_bucket.state.id

  versioning_configuration {
    status = "Enabled"
  }
}

resource "aws_s3_bucket_server_side_encryption_configuration" "state" {
  bucket = aws_s3_bucket.state.id

  rule {
    apply_server_side_encryption_by_default {
      sse_algorithm     = "aws:kms"
      kms_master_key_id = aws_kms_key.state.arn
    }
    # Cuts KMS API calls (and cost) by reusing one data key per bucket/prefix
    # for a short window instead of one per object.
    bucket_key_enabled = true
  }
}

resource "aws_s3_bucket_public_access_block" "state" {
  bucket = aws_s3_bucket.state.id

  block_public_acls       = true
  block_public_policy     = true
  ignore_public_acls      = true
  restrict_public_buckets = true
}

# Disables ACLs entirely, so the only thing that can grant access to an object
# is a policy. Removes a whole class of "made public by an ACL" mistakes.
resource "aws_s3_bucket_ownership_controls" "state" {
  bucket = aws_s3_bucket.state.id

  rule {
    object_ownership = "BucketOwnerEnforced"
  }
}

# Old state versions are still state: they hold the same secrets. 90 days is
# long enough to recover from a bad apply nobody noticed for a while, short
# enough that the bucket is not an unbounded archive of every credential the
# project has ever held.
resource "aws_s3_bucket_lifecycle_configuration" "state" {
  bucket = aws_s3_bucket.state.id

  rule {
    id     = "expire-noncurrent-state-versions"
    status = "Enabled"

    filter {}

    noncurrent_version_expiration {
      noncurrent_days = 90
    }

    abort_incomplete_multipart_upload {
      days_after_initiation = 7
    }
  }

  depends_on = [aws_s3_bucket_versioning.state]
}

data "aws_iam_policy_document" "state" {
  # S3 is TLS-capable everywhere; there is no client here that needs plaintext.
  statement {
    sid    = "DenyInsecureTransport"
    effect = "Deny"

    principals {
      type        = "AWS"
      identifiers = ["*"]
    }

    actions = ["s3:*"]
    resources = [
      aws_s3_bucket.state.arn,
      "${aws_s3_bucket.state.arn}/*",
    ]

    condition {
      test     = "Bool"
      variable = "aws:SecureTransport"
      values   = ["false"]
    }
  }

  # Denies an object written with a *different* KMS key, which bucket-default
  # encryption cannot prevent on its own -- that is the case where a state file
  # ends up readable by principals the key policy above does not cover.
  #
  # The `Null` condition is load-bearing, not defensive. An IAM condition on an
  # absent key evaluates as a match for StringNotEquals, so without it this
  # statement would also deny every write that omits the header and relies on
  # bucket default encryption -- which includes Terraform's own `.tflock` PUT.
  # The result would be that no apply could ever acquire a lock. Requiring the
  # key to be *present* before comparing it means a headerless write falls
  # through to the bucket default, which is this same key anyway.
  statement {
    sid    = "DenyWrongKmsKey"
    effect = "Deny"

    principals {
      type        = "AWS"
      identifiers = ["*"]
    }

    actions   = ["s3:PutObject"]
    resources = ["${aws_s3_bucket.state.arn}/*"]

    condition {
      test     = "Null"
      variable = "s3:x-amz-server-side-encryption-aws-kms-key-id"
      values   = ["false"]
    }

    condition {
      test     = "StringNotEquals"
      variable = "s3:x-amz-server-side-encryption-aws-kms-key-id"
      values   = [aws_kms_key.state.arn]
    }
  }
}

resource "aws_s3_bucket_policy" "state" {
  bucket = aws_s3_bucket.state.id
  policy = data.aws_iam_policy_document.state.json

  # Applying a policy before public access is blocked leaves a window where a
  # mistake in the policy is actually public.
  depends_on = [aws_s3_bucket_public_access_block.state]
}

# --------------------------------------------------------------------------
# GitHub Actions OIDC (#103)
# --------------------------------------------------------------------------

# The identity provider that lets a GitHub Actions job exchange a
# workflow-scoped token for temporary AWS credentials, so nothing in this
# project ever needs a long-lived access key in a repository secret. A stored
# key pair is the finding a reviewer raises first, and it is the credential that
# cannot be revoked by revoking anything else.
#
# It lives in `bootstrap` rather than in an environment because an
# OpenIDConnectProvider is an account-wide singleton keyed by its URL: a second
# environment creating one would fail with EntityAlreadyExists, and whichever
# environment happened to own it would take it away on destroy. Environments
# look it up with a data source instead, which also means an environment can be
# destroyed and rebuilt without touching account-level identity.
#
# No `thumbprint_list`. AWS has verified this provider's certificate chain
# against its own trust store since mid-2023 and ignores the field for the
# GitHub endpoint; pinning a thumbprint here would be a value that looks
# load-bearing, is not, and silently expires. Terraform still records whatever
# AWS returns, so a diff on this resource is worth reading rather than applying.
resource "aws_iam_openid_connect_provider" "github" {
  url = "https://token.actions.githubusercontent.com"

  # The audience a workflow requests. `sts.amazonaws.com` is what
  # aws-actions/configure-aws-credentials asks for; a role's trust policy
  # asserts it again, so this list being wrong shows up as a refused
  # AssumeRoleWithWebIdentity rather than as a wider trust.
  client_id_list = ["sts.amazonaws.com"]

  tags = { Name = "${var.project}-github-actions" }
}
