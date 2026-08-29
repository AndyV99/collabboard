# The deploy role has no network boundary, no password and no key. Its trust
# policy IS its access control, and every property asserted below would survive
# `fmt`, `validate` and a plan while leaving the role assumable by somebody it
# should not be, or able to do considerably more than deploy.
#
# The `json` overrides and the decision never to assert on `.json` follow
# modules/iam/tests for the same reason given there: under a mocked provider
# that attribute is a stub, so asserting on it would prove only that the stub
# was written correctly. These assertions read the *configured* statements,
# which Terraform evaluates itself. What that cannot catch is AWS evaluating a
# well-formed policy differently than expected; `aws iam
# simulate-principal-policy` against a real account is what closes that, and
# #105's runbook carries the command.
#
#   cd infra/terraform/modules/cicd && terraform init && terraform test

mock_provider "aws" {
  override_data {
    target = data.aws_caller_identity.current
    values = { account_id = "000000000000" }
  }

  override_data {
    target = data.aws_region.current
    values = { region = "us-east-1" }
  }

  override_data {
    target = data.aws_partition.current
    values = { partition = "aws" }
  }

  override_data {
    target = data.aws_iam_openid_connect_provider.github
    values = {
      arn = "arn:aws:iam::000000000000:oidc-provider/token.actions.githubusercontent.com"
    }
  }

  override_data {
    target = data.aws_iam_policy_document.assume
    values = { json = "{\"Version\":\"2012-10-17\",\"Statement\":[]}" }
  }

  override_data {
    target = data.aws_iam_policy_document.deploy
    values = { json = "{\"Version\":\"2012-10-17\",\"Statement\":[]}" }
  }

  override_data {
    target = data.aws_iam_policy_document.deny_master_secret
    values = { json = "{\"Version\":\"2012-10-17\",\"Statement\":[]}" }
  }
}

variables {
  name_prefix      = "collabboard-test"
  allowed_subjects = ["repo:AndyV99/collabboard:ref:refs/heads/main"]

  cluster_name  = "collabboard-test"
  service_names = ["collabboard-test-api", "collabboard-test-web"]

  passable_role_arns = [
    "arn:aws:iam::000000000000:role/collabboard-test-exec",
    "arn:aws:iam::000000000000:role/collabboard-test-task",
  ]

  ecr_repository_prefix = "collabboard"
  log_group_prefix      = "/ecs/collabboard-test"

  denied_secret_arns = ["arn:aws:secretsmanager:us-east-1:000000000000:secret:rds!db-abc"]
}

# ---------------------------------------------------------------------------
# The trust policy, which is the entire access boundary
# ---------------------------------------------------------------------------

run "the_role_is_assumable_only_by_this_repository_on_a_named_ref" {
  command = apply

  # Both conditions, not one. `aud` alone is worthless: every GitHub Actions
  # token in the world carries sts.amazonaws.com once a workflow asks for it,
  # so a trust policy with `aud` and no `sub` is assumable by every repository
  # on GitHub. That is a one-line omission that looks complete.
  assert {
    condition = alltrue([
      for want in ["token.actions.githubusercontent.com:aud", "token.actions.githubusercontent.com:sub"] :
      anytrue([
        for statement in data.aws_iam_policy_document.assume.statement :
        anytrue([for c in statement.condition : c.variable == want])
      ])
    ])
    error_message = "the trust policy must condition on BOTH aud and sub; aud alone admits every repository on GitHub"
  }

  assert {
    condition = anytrue([
      for statement in data.aws_iam_policy_document.assume.statement :
      anytrue([
        for c in statement.condition :
        c.variable == "token.actions.githubusercontent.com:sub" &&
        contains(tolist(c.values), "repo:AndyV99/collabboard:ref:refs/heads/main")
      ])
    ])
    error_message = "the trust policy must name the repository and the ref it trusts"
  }

  # Federated to the GitHub provider specifically. An `AWS` principal here would
  # be an entirely different and much wider trust model wearing the same shape.
  assert {
    condition = anytrue([
      for statement in data.aws_iam_policy_document.assume.statement :
      anytrue([
        for p in statement.principals :
        p.type == "Federated" &&
        anytrue([for i in tolist(p.identifiers) : strcontains(i, "oidc-provider/token.actions.githubusercontent.com")])
      ])
    ])
    error_message = "the trust policy must federate to the GitHub OIDC provider"
  }

  # Web identity only. A plain sts:AssumeRole would open a path this module
  # does not control.
  assert {
    condition = alltrue([
      for statement in data.aws_iam_policy_document.assume.statement :
      length(statement.actions) == 1 && contains(tolist(statement.actions), "sts:AssumeRoleWithWebIdentity")
    ])
    error_message = "the deploy role must be assumable by sts:AssumeRoleWithWebIdentity alone"
  }
}

# `repo:owner/name:*` is the mistake these validations exist for. It reads like
# "this repository" and matches a pull_request run, so on a public repository
# anyone who opens a PR could assume the deploy role -- exactly the fork-PR
# credential-exfiltration route #103 asked to make impossible.
run "rejects_a_subject_that_would_admit_a_fork_pull_request" {
  command = plan

  variables {
    allowed_subjects = ["repo:AndyV99/collabboard:*"]
  }

  expect_failures = [var.allowed_subjects]
}

run "rejects_a_subject_naming_pull_request" {
  command = plan

  variables {
    allowed_subjects = ["repo:AndyV99/collabboard:pull_request"]
  }

  expect_failures = [var.allowed_subjects]
}

run "rejects_an_empty_subject_list" {
  command = plan

  variables {
    allowed_subjects = []
  }

  expect_failures = [var.allowed_subjects]
}

run "rejects_a_subject_that_does_not_name_a_repository" {
  command = plan

  variables {
    allowed_subjects = ["*"]
  }

  expect_failures = [var.allowed_subjects]
}

# ---------------------------------------------------------------------------
# PassRole, which decides what everything else in the policy is worth
# ---------------------------------------------------------------------------

run "pass_role_is_enumerated_and_service_scoped" {
  command = apply

  # The single most common way an ECS deploy policy is wrong: PassRole on `*`
  # plus RegisterTaskDefinition is "register a definition naming any role in
  # the account, run it, read whatever that role can read".
  assert {
    condition = alltrue([
      for statement in data.aws_iam_policy_document.deploy.statement :
      statement.sid != "PassTaskRoles" || !contains(tolist(statement.resources), "*")
    ])
    error_message = "iam:PassRole must never be granted on `*` -- with RegisterTaskDefinition that is unrestricted privilege escalation in this account"
  }

  assert {
    condition = anytrue([
      for statement in data.aws_iam_policy_document.deploy.statement :
      statement.sid == "PassTaskRoles" &&
      anytrue([
        for c in statement.condition :
        c.variable == "iam:PassedToService" && contains(tolist(c.values), "ecs-tasks.amazonaws.com")
      ])
    ])
    error_message = "PassRole must be conditioned on ecs-tasks.amazonaws.com, or these roles could be passed to another service entirely"
  }
}

run "rejects_pass_role_on_everything" {
  command = plan

  variables {
    passable_role_arns = ["*"]
  }

  expect_failures = [var.passable_role_arns]
}

# ---------------------------------------------------------------------------
# What the role must NOT be able to do
# ---------------------------------------------------------------------------

# The deploy identity registers task definitions that reference secrets by ARN.
# It never resolves them -- the execution role does that at container start --
# so it needs no secretsmanager Allow, and holding one would make every deploy
# credential also a read of this environment's secrets.
run "the_deploy_role_cannot_read_a_secret_or_widen_itself" {
  command = apply

  assert {
    condition = alltrue([
      for statement in data.aws_iam_policy_document.deploy.statement :
      !anytrue([
        for action in tolist(statement.actions) :
        startswith(action, "secretsmanager:") || startswith(action, "kms:") || startswith(action, "rds:")
      ])
    ])
    error_message = "the deploy role must hold no secretsmanager, kms or rds Allow: it registers definitions that name secrets, and the execution role is what resolves them"
  }

  # Log access is read-only. A bad deploy erasing its own evidence is the
  # failure this prevents.
  assert {
    condition = alltrue([
      for statement in data.aws_iam_policy_document.deploy.statement :
      !anytrue([
        for action in tolist(statement.actions) :
        startswith(action, "logs:Delete") || startswith(action, "logs:Put")
      ])
    ])
    error_message = "the deploy role's CloudWatch Logs access must be read-only"
  }

  # PassRole is the only iam action. Anything else lets a deploy rewrite its own
  # permissions, which would make every bound above advisory.
  assert {
    condition = alltrue([
      for statement in data.aws_iam_policy_document.deploy.statement :
      !anytrue([
        for action in tolist(statement.actions) :
        startswith(action, "iam:") && action != "iam:PassRole"
      ])
    ])
    error_message = "iam:PassRole is the only iam action this role may hold; anything else lets a deploy widen its own grant"
  }

  # And it cannot change the infrastructure itself. The pipeline deploys
  # images; `terraform apply` stays a human action (ADR 0015).
  assert {
    condition = alltrue([
      for statement in data.aws_iam_policy_document.deploy.statement :
      !anytrue([
        for action in tolist(statement.actions) :
        startswith(action, "ec2:") || startswith(action, "elasticloadbalancing:") ||
        startswith(action, "s3:") || startswith(action, "ecs:Create") ||
        startswith(action, "ecs:Delete")
      ])
    ])
    error_message = "the deploy role must not be able to change infrastructure -- it rolls images onto services that already exist"
  }
}

# ADR 0001, on the newest role in the account. It changes nothing today, since
# there is no secretsmanager Allow for it to override -- and that is exactly
# why it is here: an explicit Deny cannot be overridden by an Allow attached
# later, so granting this role secret access in future still does not reach the
# master credential.
run "the_deploy_role_carries_the_master_credential_deny" {
  command = apply

  assert {
    condition = anytrue([
      for statement in data.aws_iam_policy_document.deny_master_secret.statement :
      statement.effect == "Deny" &&
      contains(tolist(statement.actions), "secretsmanager:GetSecretValue") &&
      length(tolist(statement.resources)) > 0
    ])
    error_message = "the deploy role must carry the same explicit Deny on the RDS master credential every other role in this environment carries (ADR 0001)"
  }
}

run "rejects_a_deny_with_no_resources" {
  command = plan

  variables {
    denied_secret_arns = []
  }

  expect_failures = [var.denied_secret_arns]
}

# ---------------------------------------------------------------------------
# Scoping of the things it CAN do
# ---------------------------------------------------------------------------

run "service_and_task_access_is_scoped_to_this_cluster" {
  command = apply

  # RunTask's resource is the task definition, so without an ecs:cluster
  # condition a definition registered here could be run on any cluster in the
  # account.
  assert {
    condition = anytrue([
      for statement in data.aws_iam_policy_document.deploy.statement :
      statement.sid == "RunOneShotTasks" &&
      anytrue([
        for c in statement.condition :
        c.variable == "ecs:cluster" &&
        contains(tolist(c.values), "arn:aws:ecs:us-east-1:000000000000:cluster/collabboard-test")
      ])
    ])
    error_message = "RunTask must be conditioned on this cluster; its resource is the task definition, which does not constrain where the task runs"
  }

  # UpdateService enumerates its services rather than wildcarding the cluster,
  # so a service added later is a deliberate grant.
  assert {
    condition = anytrue([
      for statement in data.aws_iam_policy_document.deploy.statement :
      statement.sid == "UpdateServices" &&
      length(tolist(statement.resources)) == length(var.service_names) &&
      !anytrue([for r in tolist(statement.resources) : endswith(r, "/*")])
    ])
    error_message = "UpdateService must name each service; a wildcard would silently cover services added later"
  }

  # ECR push scoped to this project's namespace. `*` would let a leaked session
  # overwrite any image in the account.
  assert {
    condition = anytrue([
      for statement in data.aws_iam_policy_document.deploy.statement :
      statement.sid == "EcrPush" &&
      contains(tolist(statement.resources), "arn:aws:ecr:us-east-1:000000000000:repository/collabboard/*")
    ])
    error_message = "ECR push must be scoped to this project's repository namespace"
  }
}
