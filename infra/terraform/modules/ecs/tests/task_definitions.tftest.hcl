# What these assert is the set of properties that would still let `validate`
# pass, `plan` succeed and every one of the repository's Go tests stay green
# while the deployed system was quietly wrong:
#
#   * the serving container connecting as an identity that bypasses row-level
#     security, or holding the schema owner's password and therefore able to run
#     DDL (ADR 0001, ADR 0006);
#   * a credential written as a plaintext environment variable instead of a
#     `secrets` entry, which puts it in the task definition, in the console, and
#     in anything that can call DescribeTaskDefinition;
#   * REALTIME_PING_INTERVAL drifting away from the value the ALB's idle timeout
#     was validated against, which kills realtime silently;
#   * ECS Exec quietly on for the service that holds live tenant data.
#
# Mocked provider, so no credentials and no API calls. The container definitions
# are `jsonencode`d configuration, which Terraform evaluates itself, so decoding
# and asserting on them tests the real thing rather than a fixture.
#
#   cd infra/terraform/modules/ecs && terraform init && terraform test

mock_provider "aws" {
  override_data {
    target = data.aws_region.current
    values = {
      region = "us-east-1"
    }
  }

  # Deterministic image strings. A mocked provider would otherwise generate a
  # random repository_url, which is fine but makes a failure message unreadable.
  override_resource {
    target = aws_ecr_repository.api
    values = {
      repository_url = "000000000000.dkr.ecr.us-east-1.amazonaws.com/collabboard/api"
    }
  }

  override_resource {
    target = aws_ecr_repository.web
    values = {
      repository_url = "000000000000.dkr.ecr.us-east-1.amazonaws.com/collabboard/web"
    }
  }
}

variables {
  name_prefix = "collabboard-test"
  app_env     = "staging"

  private_subnet_ids      = ["subnet-00000000000000001", "subnet-00000000000000002"]
  app_security_group_id   = "sg-00000000000000001"
  web_security_group_id   = "sg-00000000000000002"
  admin_security_group_id = "sg-00000000000000003"

  api_execution_role_arn   = "arn:aws:iam::000000000000:role/collabboard-test-exec"
  api_task_role_arn        = "arn:aws:iam::000000000000:role/collabboard-test-task"
  web_execution_role_arn   = "arn:aws:iam::000000000000:role/collabboard-test-web-exec"
  admin_execution_role_arn = "arn:aws:iam::000000000000:role/collabboard-test-admin-exec"
  admin_task_role_arn      = "arn:aws:iam::000000000000:role/collabboard-test-admin-task"

  api_log_group_name   = "/ecs/collabboard-test/api"
  web_log_group_name   = "/ecs/collabboard-test/web"
  admin_log_group_name = "/ecs/collabboard-test/admin"
  exec_log_group_name  = "/ecs/collabboard-test/exec"

  ecr_namespace = "collabboard"
  api_image_tag = "bootstrap"
  web_image_tag = "bootstrap"

  api_target_group_arn = "arn:aws:elasticloadbalancing:us-east-1:000000000000:targetgroup/collabboard-test-api/0000000000000001"
  web_target_group_arn = "arn:aws:elasticloadbalancing:us-east-1:000000000000:targetgroup/collabboard-test-web/0000000000000002"
  api_url              = "https://api.staging.collabboard.test:8443"
  web_hostname         = "staging.collabboard.test"
  api_hostname         = "api.staging.collabboard.test"

  database_host = "collabboard-test-postgres.abcdef.us-east-1.rds.amazonaws.com"
  database_port = 5432
  database_name = "collabboard"
  cache_host    = "collabboard-test-redis.abcdef.ng.0001.use1.cache.amazonaws.com"
  cache_port    = 6379

  app_database_secret_arn   = "arn:aws:secretsmanager:us-east-1:000000000000:secret:collabboard/test/db/app-AbCdEf"
  owner_database_secret_arn = "arn:aws:secretsmanager:us-east-1:000000000000:secret:collabboard/test/db/owner-AbCdEf"
  jwt_secret_arn            = "arn:aws:secretsmanager:us-east-1:000000000000:secret:collabboard/test/auth/jwt-AbCdEf"

  # Skipped so the mocked run does not model a service reaching steady state,
  # which a mock cannot do.
  wait_for_steady_state = false
}

# ---------------------------------------------------------------------------
# ADR 0001 / ADR 0006: which identity the serving container connects as
# ---------------------------------------------------------------------------

run "the_serving_container_connects_as_the_app_role_and_cannot_run_ddl" {
  command = apply

  # The positive half: it uses the serving role.
  assert {
    condition = anytrue([
      for entry in jsondecode(aws_ecs_task_definition.api.container_definitions)[0].environment :
      entry.name == "POSTGRES_USER" && entry.value == var.database_app_user
    ])
    error_message = "the serving container's POSTGRES_USER must be the app role -- ADR 0001's isolation rests on it owning nothing and being subject to row-level security"
  }

  # The negative half, and the one that matters. `api migrate` connects with
  # MigrationDSN, which is built from POSTGRES_MIGRATION_USER and
  # POSTGRES_MIGRATION_PASSWORD. A serving task that carries the owner's password
  # is a serving task that can run DDL and drop a policy.
  assert {
    condition = !anytrue([
      for entry in jsondecode(aws_ecs_task_definition.api.container_definitions)[0].secrets :
      entry.name == "POSTGRES_MIGRATION_PASSWORD"
    ])
    error_message = "the serving container must not receive the schema owner's password; migrations run from their own task definition"
  }

  assert {
    condition = !anytrue([
      for entry in jsondecode(aws_ecs_task_definition.api.container_definitions)[0].environment :
      entry.name == "POSTGRES_MIGRATION_USER"
    ])
    error_message = "the serving container must not name the schema owner at all"
  }

  # The master user is not merely absent from the task definition -- it must not
  # be nameable from it. If this ever fails, the Deny in modules/iam is the only
  # thing left standing between the application and a role that bypasses RLS.
  assert {
    condition = !anytrue([
      for entry in jsondecode(aws_ecs_task_definition.api.container_definitions)[0].environment :
      strcontains(lower(entry.value), "master")
    ])
    error_message = "no value in the serving container's environment may name the RDS master user"
  }
}

# The execution/task split is only meaningful if credentials actually travel the
# `secrets` path. A password written as a plain `environment` entry is stored in
# the task definition itself and is readable by anything that can call
# DescribeTaskDefinition -- which is a much larger set than the execution role.
run "no_credential_is_a_plaintext_environment_variable" {
  command = apply

  assert {
    condition = alltrue([
      for definition in [
        jsondecode(aws_ecs_task_definition.api.container_definitions)[0],
        jsondecode(aws_ecs_task_definition.api_migrate.container_definitions)[0],
        jsondecode(aws_ecs_task_definition.api_provision.container_definitions)[0],
        jsondecode(aws_ecs_task_definition.web.container_definitions)[0],
        jsondecode(aws_ecs_task_definition.admin.container_definitions)[0],
      ] :
      alltrue([
        for entry in lookup(definition, "environment", []) :
        !contains(
          ["POSTGRES_PASSWORD", "POSTGRES_MIGRATION_PASSWORD", "AUTH_JWT_SECRET", "REDIS_PASSWORD"],
          entry.name,
        )
      ])
    ])
    error_message = "a credential must never be a plaintext `environment` entry -- it belongs in `secrets`, resolved by the execution role before the container starts (ADR 0006)"
  }

  # And every secret that IS declared must point at a Secrets Manager ARN rather
  # than a bare name, which resolves to nothing and fails at task start.
  assert {
    condition = alltrue([
      for definition in [
        jsondecode(aws_ecs_task_definition.api.container_definitions)[0],
        jsondecode(aws_ecs_task_definition.api_migrate.container_definitions)[0],
        jsondecode(aws_ecs_task_definition.api_provision.container_definitions)[0],
      ] :
      alltrue([
        for entry in lookup(definition, "secrets", []) : startswith(entry.valueFrom, "arn:")
      ])
    ])
    error_message = "every `secrets` entry must name a full ARN"
  }
}

# config.Load runs before the subcommand is dispatched, and it refuses a
# configuration with no AUTH_JWT_SECRET outside development -- so the migration
# task needs a signing key it never uses. Leaving it out produces a migration
# that exits 1 complaining about a signing key, which reads like the wrong task
# definition was run rather than like a missing variable.
run "the_one_shot_tasks_carry_the_signing_key_config_load_demands" {
  command = apply

  assert {
    condition = anytrue([
      for entry in jsondecode(aws_ecs_task_definition.api_migrate.container_definitions)[0].secrets :
      entry.name == "AUTH_JWT_SECRET"
    ])
    error_message = "the migrate task needs AUTH_JWT_SECRET even though `api migrate` never uses it: config.Load runs before the subcommand is dispatched and refuses to return without one"
  }

  assert {
    condition     = jsondecode(aws_ecs_task_definition.api_migrate.container_definitions)[0].command == ["migrate", "up"]
    error_message = "the migrate task definition must run `api migrate up`"
  }

  # `api provision` is the one place both identities are legitimately present: it
  # connects as the owner in order to set the serving role's password.
  assert {
    condition = alltrue([
      anytrue([
        for entry in jsondecode(aws_ecs_task_definition.api_provision.container_definitions)[0].secrets :
        entry.name == "POSTGRES_PASSWORD"
      ]),
      anytrue([
        for entry in jsondecode(aws_ecs_task_definition.api_provision.container_definitions)[0].secrets :
        entry.name == "POSTGRES_MIGRATION_PASSWORD"
      ]),
    ])
    error_message = "`api provision` needs both the owner's password (to connect) and the serving role's (to set); missing either makes it a no-op or an error"
  }
}

# ---------------------------------------------------------------------------
# The break-glass task, and why it is safe to have one
# ---------------------------------------------------------------------------

run "the_administrative_task_holds_no_credential" {
  command = apply

  assert {
    condition     = length(lookup(jsondecode(aws_ecs_task_definition.admin.container_definitions)[0], "secrets", [])) == 0
    error_message = "the administrative task must carry no secret at all -- the operator supplies the database password to `psql -W` at its prompt, which is what keeps the RDS master credential a human-only capability (ADR 0013)"
  }

  # It self-terminates. A break-glass path that has to be cleaned up by hand is
  # one that gets left running.
  assert {
    condition     = jsondecode(aws_ecs_task_definition.admin.container_definitions)[0].command[0] == "sleep"
    error_message = "the administrative task must expire on its own rather than run until somebody remembers to stop it"
  }

  # RDS sets rds.force_ssl = 1, so a psql that does not ask for TLS is refused.
  assert {
    condition = anytrue([
      for entry in jsondecode(aws_ecs_task_definition.admin.container_definitions)[0].environment :
      entry.name == "PGSSLMODE" && entry.value != "disable"
    ])
    error_message = "the administrative task must default psql to TLS; the instance's parameter group refuses anything else and the error does not say so clearly"
  }
}

# ---------------------------------------------------------------------------
# The setting that kills realtime silently
# ---------------------------------------------------------------------------

# The ALB's idle timeout is validated against these two numbers. That check is
# only worth anything if the container is actually configured with the same
# values -- otherwise the load balancer is tuned for a ping interval the
# application is not using.
run "the_container_pings_at_the_interval_the_idle_timeout_was_validated_against" {
  command = apply

  assert {
    condition = anytrue([
      for entry in jsondecode(aws_ecs_task_definition.api.container_definitions)[0].environment :
      entry.name == "REALTIME_PING_INTERVAL" && entry.value == "${var.realtime_ping_interval_seconds}s"
    ])
    error_message = "REALTIME_PING_INTERVAL must come from the same variable the ALB idle timeout is validated against, or the two drift and the load balancer starts closing live connections"
  }

  assert {
    condition = anytrue([
      for entry in jsondecode(aws_ecs_task_definition.api.container_definitions)[0].environment :
      entry.name == "REALTIME_PONG_TIMEOUT" && entry.value == "${var.realtime_pong_timeout_seconds}s"
    ])
    error_message = "REALTIME_PONG_TIMEOUT must come from the same variable as the idle-timeout validation"
  }

  # coder/websocket matches OriginPatterns against the Origin header's host, so a
  # full URL here silently matches nothing and every browser handshake is
  # refused.
  assert {
    condition = anytrue([
      for entry in jsondecode(aws_ecs_task_definition.api.container_definitions)[0].environment :
      entry.name == "REALTIME_ALLOWED_ORIGINS" &&
      !strcontains(entry.value, "://") &&
      strcontains(entry.value, var.web_hostname)
    ])
    error_message = "REALTIME_ALLOWED_ORIGINS must be bare hostnames including the web tier's -- coder/websocket compares the Origin header's host, so a scheme makes the pattern match nothing"
  }
}

# ---------------------------------------------------------------------------
# Capability decisions
# ---------------------------------------------------------------------------

# #101 left ssmmessages off the API's task role and recorded that the decision
# belonged here. This is it, on the other side of the same choice.
run "exec_is_off_on_both_long_running_services" {
  command = apply

  assert {
    condition = alltrue([
      aws_ecs_service.api.enable_execute_command == false,
      aws_ecs_service.web.enable_execute_command == false,
    ])
    error_message = "ECS Exec must stay off on the serving tasks: they hold live tenant data and the serving role's password in their environment. The administrative task definition is the supported way to get a shell in the VPC."
  }

  assert {
    condition = alltrue([
      aws_ecs_service.api.network_configuration[0].assign_public_ip == false,
      aws_ecs_service.web.network_configuration[0].assign_public_ip == false,
    ])
    error_message = "tasks must not get public addresses; they are reached through the load balancer and egress through the NAT gateway"
  }

  # The web tier is not admitted to Postgres or Redis, and this is what keeps
  # that true -- the database's ingress rule references the app group by ID.
  assert {
    condition     = aws_ecs_service.web.network_configuration[0].security_groups != aws_ecs_service.api.network_configuration[0].security_groups
    error_message = "the web service must not run in the app security group; the database and cache admit that group by reference, so it would give the rendering tier a direct route to Postgres"
  }

  assert {
    condition     = aws_ecs_service.api.deployment_circuit_breaker[0].rollback == true
    error_message = "a deployment that cannot reach steady state must roll back rather than sit retrying a task that can never start"
  }
}

run "autoscaling_covers_the_api_and_leaves_the_web_tier_alone" {
  command = apply

  assert {
    condition     = aws_appautoscaling_target.api.min_capacity == var.api_min_capacity
    error_message = "the autoscaling floor must be the configured minimum"
  }

  assert {
    condition     = aws_appautoscaling_target.api.min_capacity >= 2
    error_message = "the API floor must be at least two: ADR 0005's Redis fan-out has nothing to fan out to with one task, so the project's headline path would never execute in the environment it is demonstrated from"
  }

  # Scaling in disconnects every client on the task that goes away, and they all
  # reconnect within a second or two. A short scale-in cooldown turns a quiet
  # period into an oscillation.
  assert {
    condition     = aws_appautoscaling_policy.api_cpu.target_tracking_scaling_policy_configuration[0].scale_in_cooldown > aws_appautoscaling_policy.api_cpu.target_tracking_scaling_policy_configuration[0].scale_out_cooldown
    error_message = "scale-in must be slower than scale-out; removing a task drops every WebSocket on it and the reconnects land on the tasks that remain"
  }

  assert {
    condition     = aws_ecs_service.web.desired_count == var.web_desired_count
    error_message = "the web service runs a fixed task count while #69 is open"
  }
}

# ---------------------------------------------------------------------------
# Rejections
# ---------------------------------------------------------------------------

# APP_ENV=development relaxes the two checks a deployed environment most needs:
# it generates a throwaway JWT secret per process and defaults the realtime
# origin allow-list to localhost.
run "rejects_a_development_app_env" {
  command = plan

  variables {
    app_env = "development"
  }

  expect_failures = [var.app_env]
}

# The instance's parameter group sets rds.force_ssl = 1. `disable` produces a
# connection the server refuses, at startup, with an error about SSL that reads
# like a certificate problem.
run "rejects_an_sslmode_the_database_will_refuse" {
  command = plan

  variables {
    database_sslmode = "disable"
  }

  expect_failures = [var.database_sslmode]
}

# HTTP_SHUTDOWN_TIMEOUT is 15s and covers the hub drain and the HTTP drain
# together. A stop timeout below it means SIGKILL lands mid-drain and sockets are
# reset rather than closed with a reconnect hint.
run "rejects_a_stop_timeout_below_the_application_drain_budget" {
  command = plan

  variables {
    stop_timeout_seconds = 10
  }

  expect_failures = [var.stop_timeout_seconds]
}

# ---------------------------------------------------------------------------
# The architecture, which fails in a way that names nothing
# ---------------------------------------------------------------------------

# An amd64 image on an ARM64 task definition -- or the reverse -- fails at task
# start with an exec format error. The task never reaches healthy, the apply
# times out on wait_for_steady_state ten minutes later, and nothing in the error
# mentions an architecture.
#
# The Dockerfiles' TARGET_ARCH and this value are two halves of one decision
# living in two files that no tool relates to each other. This assertion is the
# relating: it pins the module's half, so a future edit that flips one side
# alone fails at `terraform test` rather than at deploy.
run "every_task_definition_declares_the_architecture_the_images_are_built_for" {
  command = apply

  assert {
    condition = alltrue([
      for definition in [
        aws_ecs_task_definition.api,
        aws_ecs_task_definition.api_migrate,
        aws_ecs_task_definition.api_provision,
        aws_ecs_task_definition.web,
        aws_ecs_task_definition.admin,
      ] :
      definition.runtime_platform[0].cpu_architecture == "ARM64"
    ])
    error_message = "every task definition must declare ARM64, matching TARGET_ARCH in apps/api/Dockerfile and apps/web/Dockerfile. A mismatch is an exec format error at task start, which names no architecture."
  }

  # The admin task is included above and deserves a word, because it is the one
  # whose image this repository does not build: it runs
  # public.ecr.aws/docker/library/postgres, which publishes a multi-arch index
  # and so resolves for arm64 on its own. If that image is ever pinned to a
  # single-architecture digest, this assertion is what will notice.
  assert {
    condition     = alltrue([for d in [aws_ecs_task_definition.api, aws_ecs_task_definition.web] : d.runtime_platform[0].operating_system_family == "LINUX"])
    error_message = "the operating system family must stay LINUX"
  }
}

# ---------------------------------------------------------------------------
# The setting that stops the service starting at all
# ---------------------------------------------------------------------------

# modules/cache creates the replication group with transit_encryption_enabled,
# so the listener refuses a plaintext connection. The Go client only offers a
# TLS one when REDIS_TLS_ENABLED says so, and it defaults to off because the
# local compose stack needs it off. The gap between those two facts is a
# deployed API whose /healthz answers 503, a target group with no healthy
# target, and an apply that fails on wait_for_steady_state about ten minutes
# in -- naming the service, not the variable.
run "the_serving_container_speaks_tls_to_elasticache" {
  command = apply

  # The value, not merely the key. `REDIS_TLS_ENABLED = "false"` would satisfy
  # a presence check and fail in exactly the way described above.
  assert {
    condition = anytrue([
      for entry in jsondecode(aws_ecs_task_definition.api.container_definitions)[0].environment :
      entry.name == "REDIS_TLS_ENABLED" && entry.value == "true"
    ])
    error_message = "the serving container must set REDIS_TLS_ENABLED=true: modules/cache sets transit_encryption_enabled and the setting defaults to false, so omitting it produces a plaintext connection the listener rejects"
  }

  # REDIS_HOST has to be the endpoint's DNS name for the certificate to verify.
  # var.cache_host is validated against the IPv4 shape; this asserts the
  # validated value is the one that actually reaches the container, which the
  # variable's own validation cannot say.
  assert {
    condition = anytrue([
      for entry in jsondecode(aws_ecs_task_definition.api.container_definitions)[0].environment :
      entry.name == "REDIS_HOST" && entry.value == var.cache_host && !can(cidrnetmask("${entry.value}/32"))
    ])
    error_message = "REDIS_HOST must be the primary endpoint's DNS name -- the client derives the TLS ServerName from it, and Go sends no SNI for an IP literal"
  }

  # The migrate and provision tasks are deliberately NOT covered here, and this
  # is the assertion that says so rather than leaving it as an absence. Neither
  # mode opens a Redis connection: api_common_environment carries no REDIS_*
  # setting at all, so there is nothing for a TLS flag to apply to, and adding
  # one would imply a dependency that does not exist.
  assert {
    condition = alltrue([
      for definition in [
        jsondecode(aws_ecs_task_definition.api_migrate.container_definitions)[0],
        jsondecode(aws_ecs_task_definition.api_provision.container_definitions)[0],
      ] :
      !anytrue([
        for entry in lookup(definition, "environment", []) : startswith(entry.name, "REDIS_")
      ])
    ])
    error_message = "the one-shot tasks must carry no REDIS_ setting: neither `api migrate` nor `api provision` opens a cache connection, and configuring one would suggest otherwise"
  }
}

# The other half of the TLS story, caught at plan time rather than at the tenth
# minute of an apply. An address here produces a task that starts, connects,
# and fails certificate verification against a name it never sent.
run "rejects_a_cache_host_that_is_an_address" {
  command = plan

  variables {
    cache_host = "10.0.32.17"
  }

  expect_failures = [var.cache_host]
}
