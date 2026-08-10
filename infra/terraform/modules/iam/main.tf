# Two roles, because ECS uses two and conflating them is how a task ends up able
# to read every secret it was ever configured with:
#
#   execution role -- assumed by the ECS *agent*, before the container starts.
#                     Pulls the image, creates the log stream, and resolves
#                     `secrets:` entries into environment variables. The
#                     application code never holds these credentials.
#   task role      -- assumed by the *application*. This is the identity the Go
#                     process has at runtime, and the only one an application
#                     bug or an RCE can use.
#
# The database password is therefore on the execution role and not the task
# role: the task receives the resolved value as an environment variable and has
# no ability to re-read the secret, rotate it, or enumerate its neighbours.
#
# The service that assumes both arrives in #102. They are defined here because
# the resources they are scoped to -- the bucket, the key, the log group, the
# secret namespace -- are defined here, and a policy written next to the thing
# it names is a policy that can be checked.

data "aws_caller_identity" "current" {}
data "aws_region" "current" {}
data "aws_partition" "current" {}

locals {
  account_id = data.aws_caller_identity.current.account_id
  region     = data.aws_region.current.region
  partition  = data.aws_partition.current.partition

  # Secrets Manager appends a random six-character suffix to every secret ARN,
  # so even an exact-name grant has to end in `-*`. The wildcard here is that
  # suffix plus the names under the environment's namespace -- not a shortcut.
  secret_arn_pattern = "arn:${local.partition}:secretsmanager:${local.region}:${local.account_id}:secret:${var.secret_name_prefix}*"

  ecr_repository_arn_pattern = "arn:${local.partition}:ecr:${local.region}:${local.account_id}:repository/${var.ecr_repository_prefix}/*"
}

resource "aws_cloudwatch_log_group" "api" {
  name              = "/ecs/${var.name_prefix}/api"
  retention_in_days = var.log_retention_days

  # Not encrypted with the CMK. A KMS-encrypted log group needs a key policy
  # granting `logs.<region>.amazonaws.com`, which would mean this module owning
  # the key policy rather than the environment that creates the key. Logs are
  # still encrypted at rest with an AWS-managed key by default, and the
  # structured logs here are not meant to carry secrets -- #95 and #96 are the
  # issues that make what they *do* carry deliberate.

  tags = { Name = "${var.name_prefix}-api" }
}

# --------------------------------------------------------------------------
# Trust policy, shared shape
# --------------------------------------------------------------------------

data "aws_iam_policy_document" "ecs_tasks_assume" {
  statement {
    effect  = "Allow"
    actions = ["sts:AssumeRole"]

    principals {
      type        = "Service"
      identifiers = ["ecs-tasks.amazonaws.com"]
    }

    # Confused-deputy guard: without it, ECS in *any* account could be asked to
    # assume this role. `aws:SourceArn` scoped to the specific cluster is
    # stricter still and belongs in #102, where the cluster exists.
    condition {
      test     = "StringEquals"
      variable = "aws:SourceAccount"
      values   = [local.account_id]
    }
  }
}

# --------------------------------------------------------------------------
# The Deny that ADR 0001 rests on
# --------------------------------------------------------------------------

# An explicit Deny in IAM cannot be overridden by any Allow, in any policy,
# attached later, by anyone. Both roles carry it. The effect is that "the
# application connects as the RDS master user" is not a configuration mistake
# that is available to be made: the task cannot read that secret even if a task
# definition in #102 names it.
data "aws_iam_policy_document" "deny_master_secret" {
  count = length(var.denied_secret_arns) > 0 ? 1 : 0

  statement {
    sid       = "DenyRdsMasterCredential"
    effect    = "Deny"
    actions   = ["secretsmanager:*"]
    resources = var.denied_secret_arns
  }
}

resource "aws_iam_policy" "deny_master_secret" {
  count = length(var.denied_secret_arns) > 0 ? 1 : 0

  name_prefix = "${var.name_prefix}-deny-master-secret-"
  description = "Denies the RDS master credential to ECS roles. See ADR 0001 and ADR 0006."
  policy      = data.aws_iam_policy_document.deny_master_secret[0].json
}

# --------------------------------------------------------------------------
# Execution role
# --------------------------------------------------------------------------

resource "aws_iam_role" "execution" {
  name_prefix        = "${var.name_prefix}-exec-"
  description        = "ECS agent role: image pull, log stream creation, secret resolution"
  assume_role_policy = data.aws_iam_policy_document.ecs_tasks_assume.json
}

data "aws_iam_policy_document" "execution" {
  # Written by hand rather than attaching AmazonECSTaskExecutionRolePolicy. That
  # managed policy grants ecr:GetDownloadUrlForLayer, ecr:BatchGetImage and
  # logs:PutLogEvents on `Resource: "*"` -- every repository and every log group
  # in the account. Here the log group is named exactly, and ECR is narrowed to
  # one repository namespace rather than one repository, because the
  # repositories themselves do not exist until #103.
  statement {
    sid    = "EcrAuth"
    effect = "Allow"
    # The only genuinely resource-less action in this file: GetAuthorizationToken
    # takes no resource, so `*` is the only value AWS accepts. It returns a token
    # scoped to what the caller can already pull, so it grants no access on its
    # own.
    actions   = ["ecr:GetAuthorizationToken"]
    resources = ["*"]
  }

  statement {
    sid    = "EcrPull"
    effect = "Allow"
    actions = [
      "ecr:BatchCheckLayerAvailability",
      "ecr:GetDownloadUrlForLayer",
      "ecr:BatchGetImage",
    ]
    resources = [local.ecr_repository_arn_pattern]
  }

  statement {
    sid    = "WriteApiLogs"
    effect = "Allow"
    actions = [
      "logs:CreateLogStream",
      "logs:PutLogEvents",
    ]
    resources = ["${aws_cloudwatch_log_group.api.arn}:*"]
  }

  # Resolves `secrets:` entries in the task definition. Scoped to this
  # environment's namespace; the RDS master secret lives outside it (RDS names
  # its managed secrets `rds!db-...`) and is Denied above regardless.
  statement {
    sid       = "ReadEnvironmentSecrets"
    effect    = "Allow"
    actions   = ["secretsmanager:GetSecretValue"]
    resources = [local.secret_arn_pattern]
  }

  statement {
    sid       = "DecryptEnvironmentSecrets"
    effect    = "Allow"
    actions   = ["kms:Decrypt"]
    resources = [var.kms_key_arn]

    # The key is shared with S3 and RDS storage. This condition means the
    # execution role can only use it via Secrets Manager -- it cannot decrypt an
    # attachment with it.
    condition {
      test     = "StringEquals"
      variable = "kms:ViaService"
      values   = ["secretsmanager.${local.region}.amazonaws.com"]
    }
  }
}

resource "aws_iam_role_policy" "execution" {
  name_prefix = "${var.name_prefix}-exec-"
  role        = aws_iam_role.execution.id
  policy      = data.aws_iam_policy_document.execution.json
}

resource "aws_iam_role_policy_attachment" "execution_deny_master_secret" {
  count = length(var.denied_secret_arns) > 0 ? 1 : 0

  role       = aws_iam_role.execution.name
  policy_arn = aws_iam_policy.deny_master_secret[0].arn
}

# --------------------------------------------------------------------------
# Task role
# --------------------------------------------------------------------------

resource "aws_iam_role" "task" {
  name_prefix        = "${var.name_prefix}-task-"
  description        = "CollabBoard API runtime identity"
  assume_role_policy = data.aws_iam_policy_document.ecs_tasks_assume.json
}

data "aws_iam_policy_document" "task" {
  statement {
    sid    = "AttachmentObjects"
    effect = "Allow"
    actions = [
      "s3:GetObject",
      "s3:PutObject",
      "s3:DeleteObject",
      "s3:AbortMultipartUpload",
    ]
    resources = ["${var.attachments_bucket_arn}/*"]
  }

  statement {
    sid    = "AttachmentBucket"
    effect = "Allow"
    actions = [
      "s3:ListBucket",
      "s3:GetBucketLocation",
    ]
    resources = [var.attachments_bucket_arn]
  }

  statement {
    sid    = "EncryptAttachments"
    effect = "Allow"
    actions = [
      "kms:Decrypt",
      "kms:GenerateDataKey",
    ]
    resources = [var.kms_key_arn]

    # Mirror of the execution role's condition, in the other direction: the task
    # can use the key for S3 and for nothing else. In particular it cannot
    # decrypt a Secrets Manager secret with it, which is what keeps the
    # execution/task split meaningful rather than cosmetic.
    condition {
      test     = "StringEquals"
      variable = "kms:ViaService"
      values   = ["s3.${local.region}.amazonaws.com"]
    }
  }

  # Deliberately absent: any secretsmanager or ssm action. The task receives its
  # database password as an environment variable that the execution role
  # resolved before the process started (ADR 0006), so the running application
  # has no need and no way to read a secret.
  #
  # Also absent: ssmmessages:* for ECS Exec. Shell access into a task that holds
  # live tenant data is a capability decision, not a convenience, and it belongs
  # in #102 with the service it would apply to.
}

resource "aws_iam_role_policy" "task" {
  name_prefix = "${var.name_prefix}-task-"
  role        = aws_iam_role.task.id
  policy      = data.aws_iam_policy_document.task.json
}

resource "aws_iam_role_policy_attachment" "task_deny_master_secret" {
  count = length(var.denied_secret_arns) > 0 ? 1 : 0

  role       = aws_iam_role.task.name
  policy_arn = aws_iam_policy.deny_master_secret[0].arn
}
