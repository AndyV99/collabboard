# The invariant ADR 0001 rests on: neither ECS role may reach the RDS master
# credential. Getting this wrong is the failure mode ADR 0001 calls out by name
# -- every policy becomes silently decorative while all 771 tests still pass --
# so it is asserted here rather than argued in a comment.
#
# `mock_provider` means no credentials and no API calls. That has one limit
# worth stating plainly: the provider computes `aws_iam_policy_document.json`,
# so under a mock that attribute is a stub and asserting on the rendered JSON
# would prove nothing. These assertions therefore read the *configured*
# statements, which Terraform evaluates itself. That is enough to catch a Deny
# that is missing, targets the wrong ARN, is not attached to both roles, or has
# been quietly turned into an Allow -- and it is not enough to catch AWS
# evaluating a well-formed policy differently than expected. The command in
# OPERATOR-INPUTS.md step 8 (`iam simulate-principal-policy`) is what closes
# that last gap, against a real account, after apply.
#
#   cd infra/terraform/modules/iam && terraform init && terraform test

# `json` is overridden on every policy document because a mocked provider
# returns a stub string for it, and aws_iam_role/aws_iam_policy validate that
# argument as JSON before anything else runs -- so without these the module
# cannot even be evaluated. The overrides supply a syntactically valid document
# and touch nothing else: the `statement` blocks these tests assert on are
# configured input, which Terraform evaluates itself, so they are unaffected.
#
# This is also precisely why none of the assertions below reads `.json`. Under a
# mock it is a fixture, and asserting on a fixture proves only that the fixture
# was written correctly.
mock_provider "aws" {
  override_data {
    target = data.aws_iam_policy_document.ecs_tasks_assume
    values = {
      json = "{\"Version\":\"2012-10-17\",\"Statement\":[]}"
    }
  }

  override_data {
    target = data.aws_iam_policy_document.deny_master_secret
    values = {
      json = "{\"Version\":\"2012-10-17\",\"Statement\":[]}"
    }
  }

  override_data {
    target = data.aws_iam_policy_document.execution
    values = {
      json = "{\"Version\":\"2012-10-17\",\"Statement\":[]}"
    }
  }

  override_data {
    target = data.aws_iam_policy_document.task
    values = {
      json = "{\"Version\":\"2012-10-17\",\"Statement\":[]}"
    }
  }

  # #102's three additional roles, same reason.
  override_data {
    target = data.aws_iam_policy_document.web_execution
    values = {
      json = "{\"Version\":\"2012-10-17\",\"Statement\":[]}"
    }
  }

  override_data {
    target = data.aws_iam_policy_document.admin_execution
    values = {
      json = "{\"Version\":\"2012-10-17\",\"Statement\":[]}"
    }
  }

  override_data {
    target = data.aws_iam_policy_document.admin_task
    values = {
      json = "{\"Version\":\"2012-10-17\",\"Statement\":[]}"
    }
  }

  # Same reason, one layer up: a mocked provider generates a random string for
  # `arn`, and aws_iam_role_policy_attachment validates that it parses as an
  # ARN. The assertions comparing an attachment's policy_arn to this resource's
  # arn still compare two references to the same value, so the override does not
  # weaken them -- it only makes the resource constructible offline.
  override_resource {
    target = aws_iam_policy.deny_master_secret
    values = {
      arn = "arn:aws:iam::000000000000:policy/collabboard-test-deny-master-secret"
    }
  }
}

variables {
  name_prefix            = "collabboard-test"
  secret_name_prefix     = "collabboard/test/"
  attachments_bucket_arn = "arn:aws:s3:::collabboard-test-attachments-000000000000"
  kms_key_arn            = "arn:aws:kms:us-east-1:000000000000:key/00000000-0000-0000-0000-000000000000"
  ecr_repository_prefix  = "collabboard"
  denied_secret_arns     = ["arn:aws:secretsmanager:us-east-1:000000000000:secret:rds!db-abcdef-AbCdEf"]
}

run "master_credential_is_denied_to_both_roles" {
  command = apply

  assert {
    condition     = data.aws_iam_policy_document.deny_master_secret.statement[0].effect == "Deny"
    error_message = "the master-credential statement must be a Deny -- an Allow, or a missing effect, inverts the guarantee"
  }

  assert {
    condition = contains(
      data.aws_iam_policy_document.deny_master_secret.statement[0].resources,
      "arn:aws:secretsmanager:us-east-1:000000000000:secret:rds!db-abcdef-AbCdEf",
    )
    error_message = "the Deny must name the RDS master-user secret ARN passed in by the environment"
  }

  # Not just GetSecretValue: DescribeSecret, ListSecretVersionIds and the
  # replication APIs all leak or reach the credential in their own ways.
  assert {
    condition     = contains(data.aws_iam_policy_document.deny_master_secret.statement[0].actions, "secretsmanager:*")
    error_message = "the Deny must cover every secretsmanager action, not only GetSecretValue"
  }

  # Attached to BOTH roles. The execution role is the one that resolves secrets
  # before the container starts, so it is the role most likely to be handed the
  # master secret by a task definition; the task role is the one an application
  # bug can use. Missing either attachment leaves a live path.
  assert {
    condition     = aws_iam_role_policy_attachment.execution_deny_master_secret.role == aws_iam_role.execution.name
    error_message = "the Deny must be attached to the execution role"
  }

  assert {
    condition     = aws_iam_role_policy_attachment.task_deny_master_secret.role == aws_iam_role.task.name
    error_message = "the Deny must be attached to the task role"
  }

  assert {
    condition = alltrue([
      aws_iam_role_policy_attachment.execution_deny_master_secret.policy_arn == aws_iam_policy.deny_master_secret.arn,
      aws_iam_role_policy_attachment.task_deny_master_secret.policy_arn == aws_iam_policy.deny_master_secret.arn,
    ])
    error_message = "both attachments must reference the deny policy itself"
  }
}

# The other half of the same guarantee. The Deny stops the master credential
# specifically; this stops the task role from reading ANY secret, which is what
# makes the execution/task split meaningful rather than cosmetic -- the running
# application receives a resolved environment variable and has no ability to
# re-read, rotate or enumerate secrets (ADR 0006).
run "task_role_cannot_read_any_secret" {
  command = apply

  assert {
    condition = alltrue([
      for s in data.aws_iam_policy_document.task.statement :
      !anytrue([for a in s.actions : startswith(a, "secretsmanager:") || startswith(a, "ssm:")])
    ])
    error_message = "the task role must hold no secretsmanager or ssm permission -- its database password arrives as an env var resolved by the execution role"
  }

  # The execution role's read is scoped to this environment's namespace. A
  # widened resource here would let it read another environment's secrets.
  assert {
    condition = anytrue([
      for s in data.aws_iam_policy_document.execution.statement :
      contains(s.actions, "secretsmanager:GetSecretValue") &&
      alltrue([for r in s.resources : startswith(r, "arn:") && strcontains(r, "collabboard/test/")])
    ])
    error_message = "the execution role's secret read must be scoped to this environment's secret namespace"
  }
}

# #102 added three roles, and the Deny has to grow with them or it stops being a
# property of the stack and becomes a property of two roles somebody remembered.
#
# The last one is the interesting one. The administrative task exists to run
# `bootstrap-owner.sql` as the RDS master user (ADR 0013) -- and it carries the
# Deny anyway. That is the design, not an oversight: the operator reads the
# master password with their own IAM identity and gives it to `psql -W` at the
# prompt, so "no ECS role in this account can read the RDS master credential"
# stays true even of the role whose job is to use it.
run "every_ecs_role_carries_the_deny_including_the_administrative_one" {
  command = apply

  assert {
    condition = alltrue([
      aws_iam_role_policy_attachment.execution_deny_master_secret.role == aws_iam_role.execution.name,
      aws_iam_role_policy_attachment.task_deny_master_secret.role == aws_iam_role.task.name,
      aws_iam_role_policy_attachment.web_execution_deny_master_secret.role == aws_iam_role.web_execution.name,
      aws_iam_role_policy_attachment.admin_execution_deny_master_secret.role == aws_iam_role.admin_execution.name,
      aws_iam_role_policy_attachment.admin_task_deny_master_secret.role == aws_iam_role.admin_task.name,
    ])
    error_message = "all five ECS roles must carry the master-credential Deny; a role added without it is a live path to a credential that bypasses row-level security"
  }

  assert {
    condition = alltrue([
      for attachment in [
        aws_iam_role_policy_attachment.execution_deny_master_secret,
        aws_iam_role_policy_attachment.task_deny_master_secret,
        aws_iam_role_policy_attachment.web_execution_deny_master_secret,
        aws_iam_role_policy_attachment.admin_execution_deny_master_secret,
        aws_iam_role_policy_attachment.admin_task_deny_master_secret,
      ] : attachment.policy_arn == aws_iam_policy.deny_master_secret.arn
    ])
    error_message = "every attachment must reference the deny policy itself"
  }
}

# The web tier's configuration is one hostname. Reusing the API's execution role
# would have handed the rendering tier the database password and the JWT signing
# key, for nothing.
run "the_web_execution_role_cannot_read_this_environments_secrets" {
  command = apply

  assert {
    condition = alltrue([
      for statement in data.aws_iam_policy_document.web_execution.statement :
      !anytrue([
        for action in statement.actions :
        startswith(action, "secretsmanager:") || startswith(action, "ssm:") || startswith(action, "kms:")
      ])
    ])
    error_message = "the web execution role must hold no secretsmanager, ssm or kms permission -- there is no secret in the web task definition and the day somebody adds one, this is what makes them decide which tier holds it"
  }

  # It also must not be able to write to the API's log group, or a log group
  # stops being attributable to one service.
  assert {
    condition = alltrue([
      for statement in data.aws_iam_policy_document.web_execution.statement :
      alltrue([
        for resource in statement.resources :
        !strcontains(resource, aws_cloudwatch_log_group.api.name)
      ])
    ])
    error_message = "the web execution role must not be able to write to the API's log group"
  }
}

# ECS Exec is granted exactly once, on a role that belongs to a task an operator
# starts by hand and that exits on its own.
run "exec_is_granted_only_to_the_administrative_task_role" {
  command = apply

  assert {
    condition = anytrue([
      for statement in data.aws_iam_policy_document.admin_task.statement :
      contains(statement.actions, "ssmmessages:OpenDataChannel")
    ])
    error_message = "the administrative task role needs the SSM messages channel, or `aws ecs execute-command` fails with a timeout rather than a permissions error"
  }

  # The serving roles must not have it. #101 left it off deliberately and
  # recorded that the decision belonged in #102; this is the assertion that keeps
  # the decision from being reversed by accident.
  assert {
    condition = alltrue([
      for document in [data.aws_iam_policy_document.task, data.aws_iam_policy_document.execution, data.aws_iam_policy_document.web_execution] :
      alltrue([
        for statement in document.statement :
        !anytrue([for action in statement.actions : startswith(action, "ssmmessages:")])
      ])
    ])
    error_message = "no serving role may hold ssmmessages: a shell into a task holding live tenant data is a standing capability, and the administrative task definition exists so that nobody needs one"
  }

  assert {
    condition = alltrue([
      for statement in data.aws_iam_policy_document.admin_task.statement :
      !anytrue([for action in statement.actions : startswith(action, "secretsmanager:")])
    ])
    error_message = "the administrative task role must hold no secretsmanager permission at all -- the operator types the password, which is what keeps the master credential a human-only capability"
  }
}

# An empty list used to disable the Deny silently. It is now a plan-time error.
run "an_empty_deny_list_is_rejected" {
  command = plan

  variables {
    denied_secret_arns = []
  }

  expect_failures = [var.denied_secret_arns]
}

# A bare secret name in a Deny matches no ARN, so the statement renders fine and
# protects nothing.
run "a_bare_secret_name_is_rejected" {
  command = plan

  variables {
    denied_secret_arns = ["rds!db-abcdef"]
  }

  expect_failures = [var.denied_secret_arns]
}
