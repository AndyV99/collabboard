# The identity GitHub Actions assumes to deploy this environment (#103).
#
# One role, assumable only by this repository, only from the refs named in
# `allowed_subjects`, and only for the audience the OIDC provider was created
# with. It holds no credential of its own: a workflow exchanges its
# short-lived OIDC token for session credentials that expire in an hour, so
# there is nothing in a repository secret to leak, rotate, or forget to revoke.
#
# ---------------------------------------------------------------------------
# WHAT THIS ROLE CAN DO, STATED PLAINLY
# ---------------------------------------------------------------------------
# Push images, register task definitions, update the two services, and run the
# migrate task. It cannot read a secret -- the `secrets:` entries in a task
# definition are resolved by the *execution* role at container start, not by
# whoever registered the definition -- and it carries the same explicit Deny on
# the RDS master credential that every other role in this environment does.
#
# It CAN, however, register a task definition naming an arbitrary image and run
# it as the execution role, and the execution role can read this environment's
# secrets. That is not a hole this module can close: PassRole plus
# RegisterTaskDefinition plus RunTask is what deploying to ECS *is*. What
# bounds it is that the role is unassumable except by a workflow running from
# this repository on a ref somebody had to merge to, which is the same boundary
# that protects `main` itself. Said out loud here because a reader who works it
# out from the policy will assume nobody else did.
# ---------------------------------------------------------------------------

data "aws_caller_identity" "current" {}
data "aws_region" "current" {}
data "aws_partition" "current" {}

# Looked up rather than passed in. The provider is an account-wide singleton
# created by the `bootstrap` stack, whose state is local and therefore
# unreachable from here by any remote-state data source. Looking it up by URL
# means an environment can be destroyed and rebuilt without touching
# account-level identity, and a missing provider fails at plan with a message
# naming it rather than at apply with an opaque trust-policy error.
data "aws_iam_openid_connect_provider" "github" {
  url = "https://token.actions.githubusercontent.com"
}

locals {
  account_id = data.aws_caller_identity.current.account_id
  region     = data.aws_region.current.region
  partition  = data.aws_partition.current.partition

  ecr_repository_arn_pattern = "arn:${local.partition}:ecr:${local.region}:${local.account_id}:repository/${var.ecr_repository_prefix}/*"
}

# ---------------------------------------------------------------------------
# Trust policy -- the only thing standing between this role and the internet
# ---------------------------------------------------------------------------

data "aws_iam_policy_document" "assume" {
  statement {
    sid     = "GitHubActionsOIDC"
    effect  = "Allow"
    actions = ["sts:AssumeRoleWithWebIdentity"]

    principals {
      type        = "Federated"
      identifiers = [data.aws_iam_openid_connect_provider.github.arn]
    }

    # Both conditions are required, and the second is the one people leave out.
    #
    # `aud` alone is worthless: every GitHub Actions token in the world carries
    # `sts.amazonaws.com` once a workflow asks for it. Without a `sub`
    # condition, ANY repository on GitHub could assume this role.
    condition {
      test     = "StringEquals"
      variable = "token.actions.githubusercontent.com:aud"
      values   = ["sts.amazonaws.com"]
    }

    # StringLike rather than StringEquals so an `environment:` subject can be
    # written without enumerating every ref that might reach it. Every entry is
    # validated in variables.tf against the shapes that are safe -- a bare
    # `repo:owner/name:*` would let a pull_request_target job from a fork
    # assume this role, which is the credential-exfiltration route #103 named.
    condition {
      test     = "StringLike"
      variable = "token.actions.githubusercontent.com:sub"
      values   = var.allowed_subjects
    }
  }
}

resource "aws_iam_role" "deploy" {
  name               = "${var.name_prefix}-deploy"
  description        = "Assumed by GitHub Actions via OIDC to deploy ${var.name_prefix}. No long-lived credential exists for it."
  assume_role_policy = data.aws_iam_policy_document.assume.json

  # An hour. Long enough for an image build, a migration and a service rollout
  # to steady state; short enough that a leaked session credential is a
  # narrower problem than a leaked key. The workflow's own timeout is shorter.
  max_session_duration = 3600

  tags = { Name = "${var.name_prefix}-deploy" }
}

# ---------------------------------------------------------------------------
# Registry
# ---------------------------------------------------------------------------

data "aws_iam_policy_document" "deploy" {
  # GetAuthorizationToken is account-scoped by the API -- it takes no resource
  # -- so `*` here is the only expressible form rather than a widening. It
  # returns a token for repositories the caller can already reach, so it grants
  # nothing on its own.
  statement {
    sid       = "EcrLogin"
    effect    = "Allow"
    actions   = ["ecr:GetAuthorizationToken"]
    resources = ["*"]
  }

  statement {
    sid    = "EcrPush"
    effect = "Allow"
    actions = [
      "ecr:BatchCheckLayerAvailability",
      "ecr:CompleteLayerUpload",
      "ecr:InitiateLayerUpload",
      "ecr:PutImage",
      "ecr:UploadLayerPart",
      # Read as well as write: `docker buildx` consults the registry for cache
      # and for the base layers it already pushed, and a push that cannot read
      # re-uploads every layer on every build.
      "ecr:BatchGetImage",
      "ecr:GetDownloadUrlForLayer",
      "ecr:DescribeImages",
    ]
    resources = [local.ecr_repository_arn_pattern]
  }

  # ---------------------------------------------------------------------------
  # Task definitions and services
  # ---------------------------------------------------------------------------

  # RegisterTaskDefinition takes no resource -- the ARN of the thing it creates
  # does not exist until it has been created -- so this cannot be scoped and
  # says so rather than looking scoped. What bounds it is the PassRole
  # statement below: a definition naming a role this identity cannot pass is
  # rejected at registration.
  statement {
    sid    = "RegisterTaskDefinitions"
    effect = "Allow"
    actions = [
      "ecs:RegisterTaskDefinition",
      "ecs:DescribeTaskDefinition",
      "ecs:ListTaskDefinitions",
    ]
    resources = ["*"]
  }

  statement {
    sid    = "UpdateServices"
    effect = "Allow"
    actions = [
      "ecs:UpdateService",
      "ecs:DescribeServices",
    ]
    resources = [
      for name in var.service_names :
      "arn:${local.partition}:ecs:${local.region}:${local.account_id}:service/${var.cluster_name}/${name}"
    ]
  }

  # The migration task, and nothing else. Scoped to the cluster by condition
  # rather than by resource because RunTask's resource is the task definition,
  # so without this a definition registered in this account could be run on
  # someone else's cluster.
  statement {
    sid    = "RunOneShotTasks"
    effect = "Allow"
    actions = [
      "ecs:RunTask",
      "ecs:DescribeTasks",
      "ecs:StopTask",
    ]
    resources = [
      "arn:${local.partition}:ecs:${local.region}:${local.account_id}:task-definition/${var.name_prefix}-*",
      "arn:${local.partition}:ecs:${local.region}:${local.account_id}:task/${var.cluster_name}/*",
    ]

    condition {
      test     = "ArnEquals"
      variable = "ecs:cluster"
      values   = ["arn:${local.partition}:ecs:${local.region}:${local.account_id}:cluster/${var.cluster_name}"]
    }
  }

  # ---------------------------------------------------------------------------
  # PassRole -- the statement that decides how much the two above are worth
  # ---------------------------------------------------------------------------

  # Enumerated, never wildcarded. `iam:PassRole` on `*` combined with
  # RegisterTaskDefinition is full account compromise: register a definition
  # naming any role in the account, run it, and read whatever that role can
  # reach. Scoped to exactly the roles this environment's own task definitions
  # already name, the blast radius is what those roles can do -- which is the
  # blast radius of a deploy, and unavoidable.
  #
  # The ecs-tasks.amazonaws.com condition means these roles cannot be passed to
  # any other service, so this grant cannot be reused to start an EC2 instance
  # or a Lambda as one of them.
  statement {
    sid       = "PassTaskRoles"
    effect    = "Allow"
    actions   = ["iam:PassRole"]
    resources = var.passable_role_arns

    condition {
      test     = "StringEquals"
      variable = "iam:PassedToService"
      values   = ["ecs-tasks.amazonaws.com"]
    }
  }

  # ---------------------------------------------------------------------------
  # Reading what happened
  # ---------------------------------------------------------------------------

  # A migration that fails leaves its reason in CloudWatch and nowhere else, so
  # a pipeline that cannot read the log group reports "task exited 1" and stops
  # there. Read-only: the deploy identity has no reason to write or delete a
  # log, and deleting one is how a bad deploy erases its own evidence.
  statement {
    sid    = "ReadDeployLogs"
    effect = "Allow"
    actions = [
      "logs:GetLogEvents",
      "logs:DescribeLogStreams",
      "logs:DescribeLogGroups",
      "logs:FilterLogEvents",
    ]
    resources = [
      "arn:${local.partition}:logs:${local.region}:${local.account_id}:log-group:${var.log_group_prefix}*",
      "arn:${local.partition}:logs:${local.region}:${local.account_id}:log-group:${var.log_group_prefix}*:log-stream:*",
    ]
  }
}

resource "aws_iam_role_policy" "deploy" {
  name   = "${var.name_prefix}-deploy"
  role   = aws_iam_role.deploy.id
  policy = data.aws_iam_policy_document.deploy.json
}

# ---------------------------------------------------------------------------
# The Deny every role in this environment carries
# ---------------------------------------------------------------------------

# ADR 0001 again, on the newest role in the account. The deploy identity has no
# Allow on secretsmanager at all, so this changes nothing today -- and that is
# the point: an explicit Deny cannot be overridden by any Allow attached later,
# so the day somebody grants this role "just read access to check the secret
# exists", the master credential is still out of reach and the diff that would
# change that is a diff deleting a Deny.
data "aws_iam_policy_document" "deny_master_secret" {
  statement {
    sid       = "DenyRdsMasterCredential"
    effect    = "Deny"
    actions   = ["secretsmanager:GetSecretValue"]
    resources = var.denied_secret_arns
  }
}

resource "aws_iam_role_policy" "deny_master_secret" {
  name   = "${var.name_prefix}-deploy-deny-master-secret"
  role   = aws_iam_role.deploy.id
  policy = data.aws_iam_policy_document.deny_master_secret.json
}
