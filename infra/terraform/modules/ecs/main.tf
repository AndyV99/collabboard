# The compute tier: two long-running services, three one-shot task definitions,
# and the registry their images come from.
#
# The one-shot definitions are the part worth reading first, because they are the
# answer to two questions #101 left open. `api_migrate` and `api_provision` are
# how schema changes and the serving role's password reach a deployed database.
# `admin` is how #56's `bootstrap-owner.sql` gets a Postgres connection at all --
# after #101 there is no network path to RDS from anywhere. ADR 0013 is the
# reasoning; this file is the mechanism.

data "aws_region" "current" {}

locals {
  region = data.aws_region.current.region

  # Every task definition shares this. Written once because a log configuration
  # that differs between the serving task and the migration task is how a failed
  # migration ends up with no log to explain it.
  log_options = {
    "awslogs-region"        = local.region
    "awslogs-stream-prefix" = "ecs"
  }

  # Non-secret configuration common to every mode of the API binary. Note what is
  # NOT here: POSTGRES_PASSWORD and AUTH_JWT_SECRET arrive as `secrets` entries,
  # resolved by the execution role before the process starts, so the running
  # container never holds a credential it could re-read (ADR 0006).
  api_common_environment = {
    APP_ENV            = var.app_env
    LOG_LEVEL          = var.log_level
    POSTGRES_HOST      = var.database_host
    POSTGRES_PORT      = tostring(var.database_port)
    POSTGRES_DB        = var.database_name
    POSTGRES_SSLMODE   = var.database_sslmode
    POSTGRES_MAX_CONNS = tostring(var.database_max_conns)

    # In the COMMON block, not the serve block, and that is the point. `api
    # migrate` connects as the schema owner -- the one credential in the system
    # that can drop a row-level-security policy -- so it is the last connection
    # that should be encrypted-but-unverified. Every mode of the binary that
    # opens a Postgres connection gets the same trust anchors.
    #
    # The path is a contract with apps/api/Dockerfile, which copies the bundle
    # to exactly this location. Terraform cannot see inside the image, so the
    # coupling is enforced by the comment at each end and by nothing else.
    POSTGRES_SSLROOTCERT = var.database_ssl_root_cert
  }

  api_serve_environment = merge(local.api_common_environment, {
    HTTP_PORT = tostring(var.api_container_port)

    # The serving role. Deliberately not POSTGRES_MIGRATION_USER -- a serving
    # task has no owner credential and therefore cannot run DDL even if a code
    # path tried to.
    POSTGRES_USER = var.database_app_user

    REDIS_HOST = var.cache_host
    REDIS_PORT = tostring(var.cache_port)

    # A literal rather than a variable, deliberately. modules/cache sets
    # `transit_encryption_enabled = true` as a constant, not behind a variable,
    # so a `true` here is the same constant written a second time and the two
    # cannot drift. A variable would have to default to something, and a
    # variable defaulting to `false` reintroduces exactly the failure this
    # setting exists to prevent: a plaintext connection to a listener that
    # refuses it, /healthz answering 503, and the apply timing out on
    # `wait_for_steady_state` roughly ten minutes later with an error that
    # names the service rather than the setting.
    #
    # If `transit_encryption_enabled` ever becomes configurable, this becomes a
    # variable threaded from the same source in the same commit.
    REDIS_TLS_ENABLED = "true"

    # These two are set from the same variables that validate the ALB's idle
    # timeout, in modules/alb/variables.tf. Changing the ping interval here
    # without changing the idle timeout is a plan-time error rather than a
    # realtime feature that stops working in production three weeks later.
    REALTIME_PING_INTERVAL = "${var.realtime_ping_interval_seconds}s"
    REALTIME_PONG_TIMEOUT  = "${var.realtime_pong_timeout_seconds}s"

    # coder/websocket matches OriginPatterns against the Origin header's *host*,
    # which is why these are bare hostnames rather than URLs -- the development
    # default in apps/api/internal/config is `localhost:3000`, the same shape.
    #
    # In practice no handshake carries an Origin at all: the client is the Next
    # server (ADR 0010), not a browser, and coder/websocket admits a request with
    # no Origin. This is set anyway, because an allow-list that is only correct
    # while nothing sends an Origin is an allow-list that breaks the first time
    # something does.
    REALTIME_ALLOWED_ORIGINS = join(",", [var.web_hostname, var.api_hostname])
  })

  # `api migrate` connects as the schema owner and nothing else. It does not get
  # the serving role's password, and it does not get Redis: `api migrate` exits
  # before anything touches a cache.
  api_migrate_environment = merge(local.api_common_environment, {
    POSTGRES_MIGRATION_USER = var.database_owner_user
  })

  # `api provision` needs both identities at once, and that is the whole point of
  # it: it connects as the owner in order to set the serving role's password to
  # the value in POSTGRES_PASSWORD, so the secret in Secrets Manager and the
  # password in Postgres are the same string by construction (ADR 0006).
  api_provision_environment = merge(local.api_migrate_environment, {
    POSTGRES_USER = var.database_app_user
  })
}

# ---------------------------------------------------------------------------
# Registry
#
# Slash-separated names, because #101's execution role is scoped to
# `repository/<namespace>/*` and a flat `collabboard-api` would not match. The
# failure is a denied pull reported as CannotPullContainerError, which is
# indistinguishable at a glance from having no NAT gateway.
#
# AES256 rather than the environment CMK. Container images are build artefacts,
# not tenant data, and KMS-encrypted repositories add a grant and a per-layer key
# operation to every pull for no confidentiality this project needs.
# ---------------------------------------------------------------------------

resource "aws_ecr_repository" "api" {
  name         = "${var.ecr_namespace}/api"
  force_delete = var.ecr_force_delete

  # A tag that cannot be moved. #103 tags by git SHA, so nothing needs to
  # overwrite one, and immutability means the image a task definition names is
  # the image that was reviewed.
  image_tag_mutability = "IMMUTABLE"

  # Cheap, and the standard asks for image scanning. It does not replace the
  # Trivy gate in #39 -- that one runs before a push is allowed, which is the
  # half that can actually stop a bad image reaching the registry.
  image_scanning_configuration {
    scan_on_push = true
  }

  encryption_configuration {
    encryption_type = "AES256"
  }

  tags = { Name = "${var.name_prefix}-api" }
}

resource "aws_ecr_repository" "web" {
  name         = "${var.ecr_namespace}/web"
  force_delete = var.ecr_force_delete

  image_tag_mutability = "IMMUTABLE"

  image_scanning_configuration {
    scan_on_push = true
  }

  encryption_configuration {
    encryption_type = "AES256"
  }

  tags = { Name = "${var.name_prefix}-web" }
}

locals {
  ecr_lifecycle_policy = jsonencode({
    rules = [
      {
        rulePriority = 1
        description  = "Expire untagged images after ${var.ecr_untagged_expiry_days} days"
        selection = {
          tagStatus   = "untagged"
          countType   = "sinceImagePushed"
          countUnit   = "days"
          countNumber = var.ecr_untagged_expiry_days
        }
        action = { type = "expire" }
      },
      {
        rulePriority = 2
        description  = "Keep the most recent ${var.ecr_tagged_image_count} images"
        selection = {
          tagStatus   = "any"
          countType   = "imageCountMoreThan"
          countNumber = var.ecr_tagged_image_count
        }
        action = { type = "expire" }
      },
    ]
  })
}

resource "aws_ecr_lifecycle_policy" "api" {
  repository = aws_ecr_repository.api.name
  policy     = local.ecr_lifecycle_policy
}

resource "aws_ecr_lifecycle_policy" "web" {
  repository = aws_ecr_repository.web.name
  policy     = local.ecr_lifecycle_policy
}

# ---------------------------------------------------------------------------
# Cluster
# ---------------------------------------------------------------------------

resource "aws_ecs_cluster" "this" {
  name = var.name_prefix

  configuration {
    # Every ECS Exec session is transcribed to CloudWatch. The only thing that
    # ever uses Exec here is the break-glass administrative task, which exists to
    # run SQL against production data -- so "who opened a shell, when, and what
    # they typed" is exactly the record that should survive.
    #
    # `kms_key_id` is deliberately unset. The channel is TLS-encrypted
    # regardless; encrypting the transcript with the environment CMK would
    # require the admin task role to hold kms:GenerateDataKey on a key whose
    # every other grant is conditioned on a specific service, and the
    # confidentiality gained over an already-restricted log group is small.
    #
    # Consequence for the operator, recorded here because it is not obvious:
    # anything visible in an Exec session is written to that log group. Supply
    # the database password to `psql -W` at its prompt, never on a command line.
    execute_command_configuration {
      logging = "OVERRIDE"

      log_configuration {
        cloud_watch_log_group_name = var.exec_log_group_name
      }
    }
  }

  setting {
    name  = "containerInsights"
    value = var.container_insights ? "enabled" : "disabled"
  }

  tags = { Name = var.name_prefix }
}

resource "aws_ecs_cluster_capacity_providers" "this" {
  cluster_name       = aws_ecs_cluster.this.name
  capacity_providers = ["FARGATE"]

  # FARGATE only. Fargate Spot is roughly 70% cheaper and is the obvious next
  # cost lever, but a Spot reclamation is a two-minute warning followed by a task
  # disappearing -- which for the WebSocket tier means every connected client on
  # that task is disconnected. That is a fine trade for a batch worker and a poor
  # one for the feature this project is built to demonstrate.
  default_capacity_provider_strategy {
    capacity_provider = "FARGATE"
    weight            = 1
    base              = 0
  }
}

# ---------------------------------------------------------------------------
# API task definitions
# ---------------------------------------------------------------------------

locals {
  api_image = "${aws_ecr_repository.api.repository_url}:${var.api_image_tag}"

  # ARM64, and it must agree with what the pipeline actually builds. An amd64
  # image on an ARM64 task definition -- or the reverse -- fails at task start
  # with an exec format error: the task never reaches healthy, the apply times
  # out on wait_for_steady_state, and nothing in the error names an
  # architecture. So this is asserted in tests/ and the two Dockerfiles default
  # to the matching value, which makes a one-sided edit a test failure rather
  # than a deploy.
  #
  # Worth ~20% of the Fargate line (about $5.40/month at the committed staging
  # sizing, running continuously), and it makes the environment consistently
  # Graviton: RDS and ElastiCache were already t4g.
  #
  # The two tiers share one value deliberately. They could differ -- and an
  # earlier draft of this let them -- but a second platform local is a second
  # thing to keep in step with a second Dockerfile, and there is no reason for
  # them to diverge.
  runtime_platform = {
    operating_system_family = "LINUX"
    cpu_architecture        = "ARM64"
  }
}

resource "aws_ecs_task_definition" "api" {
  family                   = "${var.name_prefix}-api"
  requires_compatibilities = ["FARGATE"]
  network_mode             = "awsvpc"
  cpu                      = var.api_cpu
  memory                   = var.api_memory
  execution_role_arn       = var.api_execution_role_arn
  task_role_arn            = var.api_task_role_arn

  runtime_platform {
    operating_system_family = local.runtime_platform.operating_system_family
    cpu_architecture        = local.runtime_platform.cpu_architecture
  }

  container_definitions = jsonencode([
    {
      name      = "api"
      image     = local.api_image
      essential = true

      portMappings = [
        {
          containerPort = var.api_container_port
          protocol      = "tcp"
        },
      ]

      environment = [
        for key, value in local.api_serve_environment : { name = key, value = value }
      ]

      secrets = [
        # `:password::` selects a key out of a JSON secret. #56 writes
        # {"username": ..., "password": ...} under this ARN; the shape matches
        # what RDS itself produces for a managed credential, so an operator
        # reading either sees the same thing.
        {
          name      = "POSTGRES_PASSWORD"
          valueFrom = "${var.app_database_secret_arn}:password::"
        },
        {
          name      = "AUTH_JWT_SECRET"
          valueFrom = var.jwt_secret_arn
        },
      ]

      # Deliberately no `healthCheck`. A container health check runs a command
      # inside the container, and the API image is a static Go binary with no
      # shell to run one from. The target group's health check answers the same
      # question from outside, and is the one the load balancer actually acts on.

      stopTimeout = var.stop_timeout_seconds

      logConfiguration = {
        logDriver = "awslogs"
        options   = merge(local.log_options, { "awslogs-group" = var.api_log_group_name })
      }
    },
  ])

  tags = { Name = "${var.name_prefix}-api" }
}

# The migration runner. A task definition rather than a step inside the API's
# entrypoint, because a migration that runs on container start runs once per
# task: three tasks rolling means three concurrent migration attempts, and
# whether that is safe depends entirely on goose's advisory lock holding. Running
# it exactly once, before anything is rolled, makes the question not arise.
#
# See ADR 0013 for what happens when it fails halfway.
resource "aws_ecs_task_definition" "api_migrate" {
  family                   = "${var.name_prefix}-api-migrate"
  requires_compatibilities = ["FARGATE"]
  network_mode             = "awsvpc"
  cpu                      = var.api_cpu
  memory                   = var.api_memory
  execution_role_arn       = var.api_execution_role_arn
  task_role_arn            = var.api_task_role_arn

  runtime_platform {
    operating_system_family = local.runtime_platform.operating_system_family
    cpu_architecture        = local.runtime_platform.cpu_architecture
  }

  container_definitions = jsonencode([
    {
      name      = "migrate"
      image     = local.api_image
      essential = true
      command   = ["migrate", "up"]

      environment = [
        for key, value in local.api_migrate_environment : { name = key, value = value }
      ]

      secrets = [
        {
          name      = "POSTGRES_MIGRATION_PASSWORD"
          valueFrom = "${var.owner_database_secret_arn}:password::"
        },
        # Not used by `api migrate`, and not optional either: config.Load runs
        # before the subcommand is dispatched, and it refuses to return a
        # configuration with no AUTH_JWT_SECRET outside development. Without this
        # the migration task exits 1 with a message about a signing key, which
        # reads like the wrong task definition was run.
        {
          name      = "AUTH_JWT_SECRET"
          valueFrom = var.jwt_secret_arn
        },
      ]

      logConfiguration = {
        logDriver = "awslogs"
        options   = merge(local.log_options, { "awslogs-group" = var.api_log_group_name })
      }
    },
  ])

  tags = { Name = "${var.name_prefix}-api-migrate" }
}

# `api provision` sets the serving role's password in Postgres to the value in
# Secrets Manager. It must run AFTER `api migrate up` -- the role it alters is
# created by a migration -- and it refuses to run against a role that is a
# superuser or has BYPASSRLS, which is the check that keeps ADR 0001 honest.
resource "aws_ecs_task_definition" "api_provision" {
  family                   = "${var.name_prefix}-api-provision"
  requires_compatibilities = ["FARGATE"]
  network_mode             = "awsvpc"
  cpu                      = var.api_cpu
  memory                   = var.api_memory
  execution_role_arn       = var.api_execution_role_arn
  task_role_arn            = var.api_task_role_arn

  runtime_platform {
    operating_system_family = local.runtime_platform.operating_system_family
    cpu_architecture        = local.runtime_platform.cpu_architecture
  }

  container_definitions = jsonencode([
    {
      name      = "provision"
      image     = local.api_image
      essential = true
      command   = ["provision"]

      environment = [
        for key, value in local.api_provision_environment : { name = key, value = value }
      ]

      secrets = [
        {
          name      = "POSTGRES_MIGRATION_PASSWORD"
          valueFrom = "${var.owner_database_secret_arn}:password::"
        },
        {
          name      = "POSTGRES_PASSWORD"
          valueFrom = "${var.app_database_secret_arn}:password::"
        },
        {
          name      = "AUTH_JWT_SECRET"
          valueFrom = var.jwt_secret_arn
        },
      ]

      logConfiguration = {
        logDriver = "awslogs"
        options   = merge(local.log_options, { "awslogs-group" = var.api_log_group_name })
      }
    },
  ])

  tags = { Name = "${var.name_prefix}-api-provision" }
}

# ---------------------------------------------------------------------------
# Administrative task -- the thing #56 was blocked on
#
# Started by hand with `aws ecs run-task --enable-execute-command`, exec'd into,
# and left to expire. It carries no secret, holds no AWS permission beyond
# opening its own Exec channel, and sleeps for an hour so that a forgotten
# break-glass session cannot become a permanent one.
# ---------------------------------------------------------------------------

resource "aws_ecs_task_definition" "admin" {
  family                   = "${var.name_prefix}-admin"
  requires_compatibilities = ["FARGATE"]
  network_mode             = "awsvpc"
  cpu                      = 256
  memory                   = 512
  execution_role_arn       = var.admin_execution_role_arn
  task_role_arn            = var.admin_task_role_arn

  runtime_platform {
    operating_system_family = local.runtime_platform.operating_system_family
    cpu_architecture        = local.runtime_platform.cpu_architecture
  }

  container_definitions = jsonencode([
    {
      name      = "admin"
      image     = var.admin_image
      essential = true

      # An hour is long enough for the one-shot bootstrap in #56 and short enough
      # that walking away from it costs $0.01 rather than a standing shell into
      # the database. `run-task` again to get another hour.
      command = ["sleep", "3600"]

      # Only the host and port. Everything that would authenticate is absent by
      # design: the operator types the password at psql's prompt.
      environment = [
        { name = "PGHOST", value = var.database_host },
        { name = "PGPORT", value = tostring(var.database_port) },
        { name = "PGDATABASE", value = var.database_name },

        # RDS enforces rds.force_ssl, so a psql that does not ask for TLS is
        # refused. Setting it here means the operator does not have to remember.
        #
        # Its own variable, and deliberately weaker than the API's. This task
        # runs a stock postgres image nothing in this repository builds, so
        # there is no way to put the RDS trust anchors inside it: the bundle
        # #115 vendored is copied into apps/api's image, and RDS certificates
        # chain to a private Amazon CA that appears in no public trust store.
        # Pointing this at  would produce a break-glass session
        # that cannot connect, discovered at the moment somebody needs it.
        #
        # What that gives up is small and bounded: the session is human-started,
        # short-lived, and reaches RDS from inside the data subnet. The operator
        # can confirm the endpoint out of band, which is exactly the check
        # verify-full automates for a process that cannot.
        { name = "PGSSLMODE", value = var.admin_database_sslmode },
      ]

      logConfiguration = {
        logDriver = "awslogs"
        options   = merge(local.log_options, { "awslogs-group" = var.admin_log_group_name })
      }
    },
  ])

  tags = { Name = "${var.name_prefix}-admin" }
}

# ---------------------------------------------------------------------------
# Web task definition
# ---------------------------------------------------------------------------

resource "aws_ecs_task_definition" "web" {
  family                   = "${var.name_prefix}-web"
  requires_compatibilities = ["FARGATE"]
  network_mode             = "awsvpc"
  cpu                      = var.web_cpu
  memory                   = var.web_memory
  execution_role_arn       = var.web_execution_role_arn

  # No task_role_arn. The Next.js process calls one thing -- the Go API, over
  # HTTPS -- so an AWS identity would be a credential sitting in the container
  # that serves unauthenticated pages, with nothing to spend it on.

  runtime_platform {
    operating_system_family = local.runtime_platform.operating_system_family
    cpu_architecture        = local.runtime_platform.cpu_architecture
  }

  container_definitions = jsonencode([
    {
      name      = "web"
      image     = "${aws_ecr_repository.web.repository_url}:${var.web_image_tag}"
      essential = true

      portMappings = [
        {
          containerPort = var.web_container_port
          protocol      = "tcp"
        },
      ]

      environment = [
        # The only configuration this tier has. apps/web derives everything from
        # it: the server-side REST base, and the wss:// URL it opens the relayed
        # WebSocket on. Deliberately not a build argument -- see the banner in
        # apps/web/Dockerfile and issue #16.
        #
        # A malformed value exits the process at boot rather than failing at
        # request time, which is why the service below runs a deployment circuit
        # breaker: a bad API_URL rolls itself back instead of replacing a working
        # task set with a crash loop.
        { name = "API_URL", value = var.api_url },
        { name = "PORT", value = tostring(var.web_container_port) },

        # Load-bearing: Next's standalone server binds localhost unless told
        # otherwise, and a container bound to localhost passes its own health
        # check and answers nothing from outside. The image already sets this;
        # repeating it here means a base image change cannot silently drop it.
        { name = "HOSTNAME", value = "0.0.0.0" },
      ]

      stopTimeout = var.stop_timeout_seconds

      logConfiguration = {
        logDriver = "awslogs"
        options   = merge(local.log_options, { "awslogs-group" = var.web_log_group_name })
      }
    },
  ])

  tags = { Name = "${var.name_prefix}-web" }
}

# ---------------------------------------------------------------------------
# Services
# ---------------------------------------------------------------------------

resource "aws_ecs_service" "api" {
  name            = "${var.name_prefix}-api"
  cluster         = aws_ecs_cluster.this.id
  task_definition = aws_ecs_task_definition.api.arn
  launch_type     = "FARGATE"

  desired_count = var.api_min_capacity

  # 100/200: a new task is healthy before an old one is stopped, so a deploy
  # never dips below the running count. With a WebSocket tier that matters more
  # than usual -- capacity that briefly disappears is connections that are
  # actually dropped, not requests that are briefly slower.
  deployment_minimum_healthy_percent = 100
  deployment_maximum_percent         = 200

  # A deployment that cannot reach steady state rolls itself back rather than
  # leaving the service half-updated. The web tier makes this concrete: a
  # malformed API_URL exits at boot, and without a circuit breaker the deploy
  # would sit retrying a task that can never start.
  deployment_circuit_breaker {
    enable   = true
    rollback = true
  }

  health_check_grace_period_seconds = var.health_check_grace_period_seconds

  network_configuration {
    subnets          = var.private_subnet_ids
    security_groups  = [var.app_security_group_id]
    assign_public_ip = false
  }

  load_balancer {
    target_group_arn = var.api_target_group_arn
    container_name   = "api"
    container_port   = var.api_container_port
  }

  # Off. #101 left ssmmessages off the API's task role and called the decision
  # out as belonging here; this is the decision. A shell into a task holding live
  # tenant data, with the serving role's password in its environment, is a
  # standing capability nobody needs -- the administrative task above exists so
  # that "I need a shell in the VPC" has an answer that is not this one.
  enable_execute_command = false

  wait_for_steady_state = var.wait_for_steady_state

  # Two owners, cleanly separated. Terraform owns the task definition's *shape*
  # -- roles, environment, secrets, sizing -- and application autoscaling owns
  # desired_count. #103 owns which image tag is running, by registering a new
  # revision derived from this one and calling update-service.
  #
  # The cost: a shape change made here does not roll out on its own. `terraform
  # apply` registers the new revision, and #103 (or an `update-service` naming
  # it) is what puts it in front of traffic. That is a contract #103 has to
  # honour, and it is written down in OPERATOR-INPUTS.md rather than only here.
  lifecycle {
    ignore_changes = [task_definition, desired_count]
  }

  tags = { Name = "${var.name_prefix}-api" }

  depends_on = [aws_ecs_cluster_capacity_providers.this]
}

resource "aws_ecs_service" "web" {
  name            = "${var.name_prefix}-web"
  cluster         = aws_ecs_cluster.this.id
  task_definition = aws_ecs_task_definition.web.arn
  launch_type     = "FARGATE"

  desired_count = var.web_desired_count

  deployment_minimum_healthy_percent = 100
  deployment_maximum_percent         = 200

  deployment_circuit_breaker {
    enable   = true
    rollback = true
  }

  health_check_grace_period_seconds = var.health_check_grace_period_seconds

  network_configuration {
    subnets          = var.private_subnet_ids
    security_groups  = [var.web_security_group_id]
    assign_public_ip = false
  }

  load_balancer {
    target_group_arn = var.web_target_group_arn
    container_name   = "web"
    container_port   = var.web_container_port
  }

  enable_execute_command = false

  wait_for_steady_state = var.wait_for_steady_state

  # desired_count is NOT ignored here, unlike the API service: nothing autoscales
  # the web tier, so Terraform is the only thing that sets it and ignoring it
  # would make `web_desired_count` a variable that does nothing.
  lifecycle {
    ignore_changes = [task_definition]
  }

  tags = { Name = "${var.name_prefix}-web" }

  depends_on = [aws_ecs_cluster_capacity_providers.this]
}

# ---------------------------------------------------------------------------
# Autoscaling -- API only
# ---------------------------------------------------------------------------

resource "aws_appautoscaling_target" "api" {
  service_namespace  = "ecs"
  resource_id        = "service/${aws_ecs_cluster.this.name}/${aws_ecs_service.api.name}"
  scalable_dimension = "ecs:service:DesiredCount"

  min_capacity = var.api_min_capacity
  max_capacity = var.api_max_capacity
}

# CPU, and the alternatives are worth naming because two of them look better than
# they are.
#
# ALBRequestCountPerTarget is the reflex answer for a service behind a load
# balancer and it is actively wrong here: a WebSocket is one request that lasts
# for hours, so a task carrying a thousand live boards and a task carrying none
# report almost the same request count.
#
# Memory is a poor signal for a Go process, whose heap is a function of GC timing
# rather than of load.
#
# The metric this service actually wants is live connections per task, which is
# the hub gauge in #44 -- blocked on #12's metrics endpoint. CPU is the honest
# stand-in until that exists, and it does correlate: fan-out work is per
# connection.
resource "aws_appautoscaling_policy" "api_cpu" {
  name               = "${var.name_prefix}-api-cpu"
  policy_type        = "TargetTrackingScaling"
  service_namespace  = aws_appautoscaling_target.api.service_namespace
  resource_id        = aws_appautoscaling_target.api.resource_id
  scalable_dimension = aws_appautoscaling_target.api.scalable_dimension

  target_tracking_scaling_policy_configuration {
    predefined_metric_specification {
      predefined_metric_type = "ECSServiceAverageCPUUtilization"
    }

    target_value = var.api_cpu_target_percent

    # Asymmetric on purpose. Scaling out is cheap: a new task takes connections
    # nothing else was serving. Scaling in is not: removing a task disconnects
    # every client on it, and they all reconnect within a second or two
    # (REALTIME_SHUTDOWN_RECONNECT_HINT), which raises CPU on the survivors and
    # can start the cycle again. Five minutes between scale-ins is what stops a
    # quiet period turning into a reconnect oscillation.
    scale_out_cooldown = 60
    scale_in_cooldown  = 300
  }
}
