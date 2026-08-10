# Card attachments. Tenant-uploaded content, so the bucket is private in four
# independent ways: public access blocked at the bucket, ACLs disabled entirely,
# a policy that denies non-TLS and unencrypted writes, and no bucket policy
# granting any principal outside this account.

resource "aws_s3_bucket" "attachments" {
  bucket        = "${var.name_prefix}-attachments-${var.bucket_suffix}"
  force_destroy = var.force_destroy

  tags = { Name = "${var.name_prefix}-attachments" }
}

resource "aws_s3_bucket_public_access_block" "attachments" {
  bucket = aws_s3_bucket.attachments.id

  block_public_acls       = true
  block_public_policy     = true
  ignore_public_acls      = true
  restrict_public_buckets = true
}

# BucketOwnerEnforced disables ACLs outright, so an object cannot be made public
# by an ACL on the object -- the failure mode the public access block above is
# the second line of defence against.
resource "aws_s3_bucket_ownership_controls" "attachments" {
  bucket = aws_s3_bucket.attachments.id

  rule {
    object_ownership = "BucketOwnerEnforced"
  }
}

resource "aws_s3_bucket_server_side_encryption_configuration" "attachments" {
  bucket = aws_s3_bucket.attachments.id

  rule {
    apply_server_side_encryption_by_default {
      sse_algorithm     = "aws:kms"
      kms_master_key_id = var.kms_key_arn
    }
    bucket_key_enabled = true
  }
}

# An attachment overwritten or deleted by a bug is recoverable rather than gone.
# The lifecycle rule below is what stops that becoming unbounded storage.
resource "aws_s3_bucket_versioning" "attachments" {
  bucket = aws_s3_bucket.attachments.id

  versioning_configuration {
    status = "Enabled"
  }
}

resource "aws_s3_bucket_lifecycle_configuration" "attachments" {
  bucket = aws_s3_bucket.attachments.id

  rule {
    id     = "expire-noncurrent-versions"
    status = "Enabled"

    filter {}

    noncurrent_version_expiration {
      noncurrent_days = var.noncurrent_version_expiration_days
    }

    # A multipart upload abandoned partway through bills for its parts forever
    # and is invisible in the object listing. This is pure cost hygiene.
    abort_incomplete_multipart_upload {
      days_after_initiation = 7
    }
  }

  depends_on = [aws_s3_bucket_versioning.attachments]
}

data "aws_iam_policy_document" "attachments" {
  statement {
    sid    = "DenyInsecureTransport"
    effect = "Deny"

    principals {
      type        = "AWS"
      identifiers = ["*"]
    }

    actions = ["s3:*"]
    resources = [
      aws_s3_bucket.attachments.arn,
      "${aws_s3_bucket.attachments.arn}/*",
    ]

    condition {
      test     = "Bool"
      variable = "aws:SecureTransport"
      values   = ["false"]
    }
  }

  # Denies a write that explicitly asks for weaker encryption than SSE-KMS,
  # which bucket-default encryption does not prevent.
  #
  # The `Null` condition is load-bearing. An IAM condition on an absent key
  # matches StringNotEquals, so without it this would also deny every upload
  # that omits the header -- which is every upload the AWS SDK for Go makes
  # unless the caller sets it explicitly. Attachments would fail with
  # AccessDenied and the bucket policy would look correct while doing it.
  # Requiring the header to be present before comparing it means a headerless
  # upload falls through to bucket default encryption, which is SSE-KMS under
  # this environment's key.
  statement {
    sid    = "DenyUnencryptedObjectUploads"
    effect = "Deny"

    principals {
      type        = "AWS"
      identifiers = ["*"]
    }

    actions   = ["s3:PutObject"]
    resources = ["${aws_s3_bucket.attachments.arn}/*"]

    condition {
      test     = "Null"
      variable = "s3:x-amz-server-side-encryption"
      values   = ["false"]
    }

    condition {
      test     = "StringNotEquals"
      variable = "s3:x-amz-server-side-encryption"
      values   = ["aws:kms"]
    }
  }
}

resource "aws_s3_bucket_policy" "attachments" {
  bucket = aws_s3_bucket.attachments.id
  policy = data.aws_iam_policy_document.attachments.json

  depends_on = [aws_s3_bucket_public_access_block.attachments]
}
