variable "name_prefix" {
  description = "Prefix for every resource name, e.g. `collabboard-staging`."
  type        = string
}

variable "app_env" {
  description = "Value of APP_ENV in both task definitions. Anything other than `development` makes the API require AUTH_JWT_SECRET and refuse to default its WebSocket origin allow-list, which is the behaviour a deployed environment wants."
  type        = string

  validation {
    condition     = var.app_env != "development"
    error_message = "app_env must not be `development` in a deployed environment: apps/api/internal/config relaxes the JWT secret requirement and defaults the realtime origin allow-list to localhost when it sees that value."
  }
}

variable "log_level" {
  description = "LOG_LEVEL for both containers."
  type        = string
  default     = "info"
}

# ---------------------------------------------------------------------------
# Placement
# ---------------------------------------------------------------------------

variable "private_subnet_ids" {
  description = "Subnets the tasks run in. Private: they have a route out through the NAT gateway and no route in."
  type        = list(string)
}

variable "app_security_group_id" {
  description = "Security group for the API tasks. It is the source of the database and cache ingress rules."
  type        = string
}

variable "web_security_group_id" {
  description = "Security group for the Next.js tasks. Deliberately not admitted to Postgres or Redis."
  type        = string
}

variable "admin_security_group_id" {
  description = "Security group for the one-shot administrative task. Reaches Postgres only."
  type        = string
}

# ---------------------------------------------------------------------------
# Identity
# ---------------------------------------------------------------------------

variable "api_execution_role_arn" {
  description = "Execution role for the API task definitions. Resolves the `secrets:` entries below before the container starts."
  type        = string
}

variable "api_task_role_arn" {
  description = "Runtime identity for the API container."
  type        = string
}

variable "web_execution_role_arn" {
  description = "Execution role for the web task definition. Cannot read this environment's secrets."
  type        = string
}

variable "admin_execution_role_arn" {
  description = "Execution role for the one-shot administrative task."
  type        = string
}

variable "admin_task_role_arn" {
  description = "Runtime identity for the one-shot administrative task: ECS Exec and session logging only."
  type        = string
}

variable "api_log_group_name" {
  description = "CloudWatch Logs group for the API container."
  type        = string
}

variable "web_log_group_name" {
  description = "CloudWatch Logs group for the web container."
  type        = string
}

variable "admin_log_group_name" {
  description = "CloudWatch Logs group for the administrative task's container output."
  type        = string
}

variable "exec_log_group_name" {
  description = "CloudWatch Logs group the cluster writes ECS Exec session transcripts to."
  type        = string
}

# ---------------------------------------------------------------------------
# Images
# ---------------------------------------------------------------------------

variable "ecr_namespace" {
  description = "First path component of both repository names. #101's execution role is scoped to `repository/<namespace>/*`, so this must match `ecr_repository_prefix` there -- a mismatch denies the pull and surfaces as CannotPullContainerError."
  type        = string
}

variable "api_image_tag" {
  description = <<-EOT
    Tag of the API image the task definition runs.

    An image must exist at this tag before the first apply. Nothing in this
    configuration builds one -- #103 does -- so the first apply of a fresh
    environment is: create the repositories, push an image, then apply again.
    Otherwise the service never reaches steady state and, with
    `wait_for_steady_state`, apply fails after roughly ten minutes with a
    CannotPullContainerError on the task.

    After #103 exists this value stops mattering for what is *running*: the
    service ignores changes to its task definition, and the pipeline rolls new
    revisions. It still decides the shape Terraform would deploy from cold.
  EOT
  type        = string
}

variable "web_image_tag" {
  description = "Tag of the Next.js image. See api_image_tag."
  type        = string
}

variable "admin_image" {
  description = <<-EOT
    Fully qualified image for the one-shot administrative task. A stock Postgres
    client image, because what it is for is holding a `psql` session inside the
    VPC (ADR 0013). Fargate pulls from ECR Public anonymously, so the execution
    role needs no ECR permission for it.

    Pin to a digest rather than a tag in an environment where reproducibility
    matters more than convenience.
  EOT
  type        = string
  default     = "public.ecr.aws/docker/library/postgres:16-alpine"
}

variable "ecr_untagged_expiry_days" {
  description = "Days an untagged image layer survives in ECR. Untagged images are the residue of overwritten builds and nothing can run them."
  type        = number
  default     = 7
}

variable "ecr_tagged_image_count" {
  description = "Tagged images kept per repository. Every one is a rollback target and roughly 30-80 MB of storage at $0.10/GB-month."
  type        = number
  default     = 30
}

variable "ecr_force_delete" {
  description = "Allow `terraform destroy` to delete a repository with images still in it. True in an environment whose whole point is being destroyable."
  type        = bool
  default     = false
}

# ---------------------------------------------------------------------------
# Sizing
# ---------------------------------------------------------------------------

variable "api_cpu" {
  description = "Fargate CPU units for the API task. 256 = 0.25 vCPU."
  type        = number
  default     = 256
}

variable "api_memory" {
  description = "Fargate memory (MiB) for the API task. Must be a combination Fargate accepts for api_cpu."
  type        = number
  default     = 512
}

variable "web_cpu" {
  description = "Fargate CPU units for the web task."
  type        = number
  default     = 256
}

variable "web_memory" {
  description = "Fargate memory (MiB) for the web task."
  type        = number
  default     = 512
}

variable "api_min_capacity" {
  description = <<-EOT
    Floor for the API service.

    Two, not one, and not only for availability. ADR 0005's Redis pub/sub fan-out
    is what lets any instance serve any board, and with a single task that code
    path never executes -- the project's headline feature would be running in a
    shape that has never been exercised in the environment it is demonstrated
    from. One task also makes every scale-in and every deploy a disconnect for
    100% of connected clients rather than half of them.
  EOT
  type        = number
  default     = 2

  validation {
    condition     = var.api_min_capacity >= 1
    error_message = "api_min_capacity must be at least 1."
  }
}

variable "api_max_capacity" {
  description = "Ceiling for the API service. This is the real upper bound on the Fargate line of the bill."
  type        = number
  default     = 6
}

variable "api_cpu_target_percent" {
  description = "Average CPU utilisation the autoscaling policy holds the API service at."
  type        = number
  default     = 60
}

variable "web_desired_count" {
  description = <<-EOT
    Fixed task count for the web service, deliberately not autoscaled.

    #69: refresh-token rotation is per-process, so two web tasks handed requests
    from the same browser can race and the API's reuse detection revokes the
    session -- the user is signed out at random. Until that is fixed, running one
    web task is a correctness requirement, not a cost decision, and autoscaling
    it would make an unfixed bug intermittent instead of absent.
  EOT
  type        = number
  default     = 1
}

# ---------------------------------------------------------------------------
# Load balancer wiring
# ---------------------------------------------------------------------------

variable "api_target_group_arn" {
  description = "Target group the API service registers into."
  type        = string
}

variable "web_target_group_arn" {
  description = "Target group the web service registers into."
  type        = string
}

variable "api_container_port" {
  description = "Port the Go API container listens on (HTTP_PORT)."
  type        = number
  default     = 8080
}

variable "web_container_port" {
  description = "Port the Next.js container listens on (PORT)."
  type        = number
  default     = 3000
}

variable "api_url" {
  description = "Absolute base URL of the Go API, including its non-default port. Becomes API_URL in the web task definition, from which apps/web derives both its server-side REST base and the wss:// URL it opens the relayed WebSocket on."
  type        = string
}

variable "web_hostname" {
  description = "Public hostname of the web tier. Becomes REALTIME_ALLOWED_ORIGINS on the API."
  type        = string
}

variable "api_hostname" {
  description = "Public hostname of the API. Also added to REALTIME_ALLOWED_ORIGINS so a same-origin handshake is accepted."
  type        = string
}

# ---------------------------------------------------------------------------
# Dependencies
# ---------------------------------------------------------------------------

variable "database_host" {
  description = "RDS endpoint hostname."
  type        = string
}

variable "database_port" {
  description = "RDS port."
  type        = number
}

variable "database_name" {
  description = "Database the API connects to."
  type        = string
}

variable "database_sslmode" {
  description = "libpq sslmode for the API's DSN. The instance's parameter group sets `rds.force_ssl = 1`, so anything below `require` is refused by the server at connect time. `verify-full` would need a CA bundle in the image and a way to point at it, which apps/api does not have today."
  type        = string
  default     = "require"

  validation {
    condition     = contains(["require", "verify-ca", "verify-full"], var.database_sslmode)
    error_message = "database_sslmode must be require, verify-ca or verify-full. RDS is configured with rds.force_ssl = 1, so `disable`, `allow` and `prefer` produce a connection the server rejects."
  }
}

variable "database_app_user" {
  description = "Serving role. Never the RDS master user and never the schema owner: it owns nothing and row-level security applies to it (ADR 0001, ADR 0006). #56 creates it."
  type        = string
  default     = "collabboard_app"
}

variable "database_owner_user" {
  description = "Schema owner, used only by the one-shot migrate and provision task definitions. Never present in the serving task definition, so a serving task cannot run DDL."
  type        = string
  default     = "collabboard_owner"
}

variable "database_max_conns" {
  description = "POSTGRES_MAX_CONNS per task. Multiplied by api_max_capacity this is the peak connection count against a db.t4g.micro, whose own max_connections is roughly 100."
  type        = number
  default     = 10

  validation {
    condition     = var.database_max_conns > 0
    error_message = "database_max_conns must be positive."
  }
}

variable "cache_host" {
  description = "ElastiCache primary endpoint, as a DNS name. Never an address: the API verifies the TLS certificate against this value, Go sends no SNI for an IP literal, and the ElastiCache certificate carries no IP SAN -- so an address fails verification with an error that reads like a broken trust store rather than like a wrong hostname."
  type        = string

  validation {
    # Not a full address parser -- it only has to catch the one shape that
    # silently breaks TLS verification, and `can(cidrnetmask(...))` is the
    # cheapest way to ask "is this an IPv4 literal" in HCL.
    condition     = !can(cidrnetmask("${var.cache_host}/32"))
    error_message = "cache_host must be the ElastiCache primary endpoint's DNS name, not an address. REDIS_TLS_ENABLED is on and the client verifies the certificate against this name; an IP literal sends no SNI and matches no SAN, so every connection fails verification."
  }
}

variable "cache_port" {
  description = "ElastiCache port."
  type        = number
}

variable "app_database_secret_arn" {
  description = "Secrets Manager ARN holding the serving role's credential as JSON with a `password` key. Its value is written by #56, not by Terraform."
  type        = string
}

variable "owner_database_secret_arn" {
  description = "Secrets Manager ARN holding the schema owner's credential. Consumed only by the migrate and provision task definitions."
  type        = string
}

variable "jwt_secret_arn" {
  description = "Secrets Manager ARN holding AUTH_JWT_SECRET. Required by every mode of the binary, including migrate and provision, because config.Load runs before the subcommand is dispatched."
  type        = string
}

# ---------------------------------------------------------------------------
# Realtime
# ---------------------------------------------------------------------------

variable "realtime_ping_interval_seconds" {
  description = "REALTIME_PING_INTERVAL. The same variable feeds the ALB module's idle-timeout validation, so the two cannot drift into a configuration where the load balancer closes connections the hub believes are alive."
  type        = number
  default     = 25
}

variable "realtime_pong_timeout_seconds" {
  description = "REALTIME_PONG_TIMEOUT. How long a peer has to answer a ping before its connection is reaped."
  type        = number
  default     = 10
}

# ---------------------------------------------------------------------------
# Behaviour
# ---------------------------------------------------------------------------

variable "container_insights" {
  description = <<-EOT
    ECS Container Insights. Off by default, and that is a cost decision rather
    than an oversight: it publishes per-task custom metrics, which at this task
    count is a bill comparable to the tasks themselves, and the Observability
    standard's actual requirement is Prometheus metrics scraped into Grafana
    (#12), not CloudWatch custom metrics.
  EOT
  type        = bool
  default     = false
}

variable "stop_timeout_seconds" {
  description = "Seconds between SIGTERM and SIGKILL. Must exceed the API's HTTP_SHUTDOWN_TIMEOUT (15s), which bounds the hub drain and the HTTP drain together -- below it, a deploy resets sockets instead of closing them with a reconnect hint."
  type        = number
  default     = 30

  validation {
    condition     = var.stop_timeout_seconds > 15 && var.stop_timeout_seconds <= 120
    error_message = "stop_timeout_seconds must be greater than the API's 15s shutdown budget and no more than 120 (Fargate's maximum)."
  }
}

variable "health_check_grace_period_seconds" {
  description = "How long after a task starts the service ignores load balancer health checks. The API answers /healthz as soon as it is listening, but its pool connects lazily, so a cold task can report 503 briefly."
  type        = number
  default     = 60
}

variable "wait_for_steady_state" {
  description = "Block `terraform apply` until the service is stable. True on purpose: without it, apply reports success on a service whose tasks are crash-looping, which is the single most common way a deployment looks fine and is not."
  type        = bool
  default     = true
}
