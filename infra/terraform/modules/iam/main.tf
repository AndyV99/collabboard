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
#
# Unconditional, deliberately. These three resources were previously gated on
# `length(var.denied_secret_arns) > 0`, so an empty list produced no Deny and no
# error -- the control vanished silently in exactly the case where it had been
# misconfigured. `denied_secret_arns` now validates non-empty instead, which
# makes the gate both unnecessary and misleading: a `count` here would tell the
# next reader that an instantiation without the Deny is a supported shape.
data "aws_iam_policy_document" "deny_master_secret" {
  statement {
    sid       = "DenyRdsMasterCredential"
    effect    = "Deny"
    actions   = ["secretsmanager:*"]
    resources = var.denied_secret_arns
  }
}

resource "aws_iam_policy" "deny_master_secret" {
  name_prefix = "${var.name_prefix}-deny-master-secret-"
  description = "Denies the RDS master credential to ECS roles. See ADR 0001 and ADR 0006."
  policy      = data.aws_iam_policy_document.deny_master_secret.json
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
  role       = aws_iam_role.execution.name
  policy_arn = aws_iam_policy.deny_master_secret.arn
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
  role       = aws_iam_role.task.name
  policy_arn = aws_iam_policy.deny_master_secret.arn
}

# ===========================================================================
# #102 additions: the web tier and the one-shot administrative task.
# ===========================================================================

resource "aws_cloudwatch_log_group" "web" {
  name              = "/ecs/${var.name_prefix}/web"
  retention_in_days = var.log_retention_days

  tags = { Name = "${var.name_prefix}-web" }
}

# --------------------------------------------------------------------------
# Web execution role
#
# A second execution role rather than reusing the API's, and the reason is one
# statement: the API's execution role can read every secret under
# `collabboard/<env>/`, which is the database password and the JWT signing key.
# The Next.js tier needs neither -- its only configuration is API_URL, which is a
# hostname -- so reusing that role would hand the rendering tier the API's
# credentials for no reason at all.
#
# It also cannot read the API's log group, and the API's role cannot write to
# this one, so a log group is attributable to exactly one service.
# --------------------------------------------------------------------------

resource "aws_iam_role" "web_execution" {
  name_prefix        = "${var.name_prefix}-web-exec-"
  description        = "ECS agent role for the Next.js tasks: image pull and log stream creation"
  assume_role_policy = data.aws_iam_policy_document.ecs_tasks_assume.json
}

data "aws_iam_policy_document" "web_execution" {
  statement {
    sid       = "EcrAuth"
    effect    = "Allow"
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
    sid    = "WriteWebLogs"
    effect = "Allow"
    actions = [
      "logs:CreateLogStream",
      "logs:PutLogEvents",
    ]
    resources = ["${aws_cloudwatch_log_group.web.arn}:*"]
  }

  # Deliberately absent: any secretsmanager or kms action. There is no secret in
  # the web task definition, and the day somebody adds one this policy is what
  # makes them think about which tier is holding it.
}

resource "aws_iam_role_policy" "web_execution" {
  name_prefix = "${var.name_prefix}-web-exec-"
  role        = aws_iam_role.web_execution.id
  policy      = data.aws_iam_policy_document.web_execution.json
}

resource "aws_iam_role_policy_attachment" "web_execution_deny_master_secret" {
  role       = aws_iam_role.web_execution.name
  policy_arn = aws_iam_policy.deny_master_secret.arn
}

# There is deliberately NO web task role. `taskRoleArn` is optional, and omitting
# it means the Next.js process has no AWS credentials at all -- not a role with
# an empty policy, but no identity to assume. It calls one thing, the Go API,
# over HTTPS, so an AWS identity would be a credential in the container serving
# unauthenticated pages with nothing to spend it on.

# --------------------------------------------------------------------------
# Administrative task
#
# The break-glass path #56 needs and #101 could not provide: after #101 the data
# subnets have no route off the VPC and RDS is not publicly accessible, so
# `bootstrap-owner.sql` has nowhere to run from. This role belongs to a one-shot
# Fargate task an operator starts by hand, holds an ECS Exec shell into, and
# stops. See ADR 0013.
#
# The line worth reading twice: this role carries the SAME Deny on the RDS master
# credential as the API's two roles. That is not an oversight in a role whose
# entire purpose is to run `bootstrap-owner.sql` as the master user -- it is the
# design. The operator reads the master password with their own IAM identity and
# supplies it to `psql -W` interactively. No role in this account's ECS layer can
# read that secret, including this one, which means "the application connects as
# the master user" has no machine identity available to it anywhere.
# --------------------------------------------------------------------------

resource "aws_cloudwatch_log_group" "exec" {
  name              = "/ecs/${var.name_prefix}/exec"
  retention_in_days = var.exec_log_retention_days

  tags = { Name = "${var.name_prefix}-exec" }
}

resource "aws_iam_role" "admin_execution" {
  name_prefix        = "${var.name_prefix}-admin-exec-"
  description        = "ECS agent role for the one-shot administrative task"
  assume_role_policy = data.aws_iam_policy_document.ecs_tasks_assume.json
}

data "aws_iam_policy_document" "admin_execution" {
  # No ECR statements. The administrative task runs a stock Postgres client image
  # from ECR Public, which Fargate pulls anonymously.
  statement {
    sid    = "WriteAdminLogs"
    effect = "Allow"
    actions = [
      "logs:CreateLogStream",
      "logs:PutLogEvents",
    ]
    resources = ["${aws_cloudwatch_log_group.admin.arn}:*"]
  }
}

resource "aws_cloudwatch_log_group" "admin" {
  name              = "/ecs/${var.name_prefix}/admin"
  retention_in_days = var.exec_log_retention_days

  tags = { Name = "${var.name_prefix}-admin" }
}

resource "aws_iam_role_policy" "admin_execution" {
  name_prefix = "${var.name_prefix}-admin-exec-"
  role        = aws_iam_role.admin_execution.id
  policy      = data.aws_iam_policy_document.admin_execution.json
}

resource "aws_iam_role_policy_attachment" "admin_execution_deny_master_secret" {
  role       = aws_iam_role.admin_execution.name
  policy_arn = aws_iam_policy.deny_master_secret.arn
}

resource "aws_iam_role" "admin_task" {
  name_prefix        = "${var.name_prefix}-admin-task-"
  description        = "Runtime identity for the one-shot administrative task. ECS Exec only; no secret access."
  assume_role_policy = data.aws_iam_policy_document.ecs_tasks_assume.json
}

data "aws_iam_policy_document" "admin_task" {
  # ECS Exec runs over an SSM messages channel, and these four actions are what
  # opens it. They are granted here and nowhere else: #101 left `ssmmessages` off
  # the API's task role on purpose, noting that a shell into a task holding live
  # tenant data is a capability decision. This is that decision -- yes, but on a
  # task that holds no tenant data, runs only when an operator starts it, and
  # exits on its own.
  #
  # `Resource: "*"` because the SSM messages API takes no resource. It is the
  # same shape as `ecr:GetAuthorizationToken` above: not a shortcut, the only
  # value AWS accepts.
  statement {
    sid    = "EcsExecChannel"
    effect = "Allow"
    actions = [
      "ssmmessages:CreateControlChannel",
      "ssmmessages:CreateDataChannel",
      "ssmmessages:OpenControlChannel",
      "ssmmessages:OpenDataChannel",
    ]
    resources = ["*"]
  }

  # Session logging. ECS Exec writes the session transcript using the TASK role,
  # not the execution role, so without these the cluster's logging configuration
  # silently produces no audit trail -- the session still works, which is the
  # worst version of this failing.
  statement {
    sid       = "DiscoverExecLogGroup"
    effect    = "Allow"
    actions   = ["logs:DescribeLogGroups"]
    resources = ["*"]
  }

  statement {
    sid    = "WriteExecSessionLog"
    effect = "Allow"
    actions = [
      "logs:CreateLogStream",
      "logs:PutLogEvents",
    ]
    resources = ["${aws_cloudwatch_log_group.exec.arn}:*"]
  }

  # Deliberately absent: every secretsmanager action. The operator supplies the
  # database password to `psql -W` by hand. See the block comment above.
}

resource "aws_iam_role_policy" "admin_task" {
  name_prefix = "${var.name_prefix}-admin-task-"
  role        = aws_iam_role.admin_task.id
  policy      = data.aws_iam_policy_document.admin_task.json
}

resource "aws_iam_role_policy_attachment" "admin_task_deny_master_secret" {
  role       = aws_iam_role.admin_task.name
  policy_arn = aws_iam_policy.deny_master_secret.arn
}
