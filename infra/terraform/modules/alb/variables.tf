variable "name_prefix" {
  description = "Prefix for every resource name, e.g. `collabboard-staging`."
  type        = string
}

variable "vpc_id" {
  description = "VPC the target groups belong to."
  type        = string
}

variable "public_subnet_ids" {
  description = "Public subnets for the load balancer's nodes. At least two, in different AZs."
  type        = list(string)

  validation {
    condition     = length(var.public_subnet_ids) >= 2
    error_message = "An Application Load Balancer requires subnets in at least two availability zones."
  }
}

variable "security_group_id" {
  description = "Security group for the load balancer. It is what makes the API listener non-public; see modules/security-groups."
  type        = string
}

# ---------------------------------------------------------------------------
# Names and certificate
# ---------------------------------------------------------------------------

variable "web_hostname" {
  description = "Fully qualified name the product is served from, e.g. `staging.collabboard.example.com`. Open to the internet."
  type        = string
}

variable "api_hostname" {
  description = "Fully qualified name the Go API is served from, e.g. `api.staging.collabboard.example.com`. Resolves publicly, but the listener behind it admits only `api_ingress_cidrs` -- see ADR 0014."
  type        = string

  validation {
    condition     = var.api_hostname != var.web_hostname
    error_message = "api_hostname and web_hostname must differ: they are served by different listener ports with different exposure, and one name cannot be both."
  }
}

variable "route53_zone_id" {
  description = <<-EOT
    Route 53 hosted zone containing both hostnames. Used for the ACM
    certificate's DNS validation records and for the alias records pointing at
    the load balancer.

    This is the one place the configuration requires the operator's domain to
    live in Route 53 in this account. A certificate validated another way would
    have to be created outside Terraform and this module changed to accept its
    ARN.
  EOT
  type        = string

  validation {
    condition     = can(regex("^Z[A-Z0-9]+$", var.route53_zone_id))
    error_message = "route53_zone_id must look like a Route 53 hosted zone ID (`Z` followed by upper-case alphanumerics). A zone *name* here fails at apply with a not-found error that reads like a permissions problem."
  }
}

variable "ssl_policy" {
  description = "Predefined TLS policy for both HTTPS listeners. The default admits TLS 1.2 and 1.3 only."
  type        = string
  default     = "ELBSecurityPolicy-TLS13-1-2-Res-2021-06"
}

# ---------------------------------------------------------------------------
# Ports
# ---------------------------------------------------------------------------

variable "http_port" {
  description = "Listener port that does nothing but redirect to HTTPS."
  type        = number
  default     = 80
}

variable "web_port" {
  description = "HTTPS listener port for the web tier."
  type        = number
  default     = 443
}

variable "api_port" {
  description = "HTTPS listener port for the Go API. Not 443, because the security group is the only per-port control an ALB has. See modules/security-groups/variables.tf."
  type        = number
  default     = 8443
}

variable "api_container_port" {
  description = "Port the Go API container listens on."
  type        = number
  default     = 8080
}

variable "web_container_port" {
  description = "Port the Next.js container listens on."
  type        = number
  default     = 3000
}

# ---------------------------------------------------------------------------
# The WebSocket-critical setting
# ---------------------------------------------------------------------------

variable "idle_timeout_seconds" {
  description = <<-EOT
    Seconds the load balancer holds a connection open with no bytes in either
    direction before closing it.

    This is the single setting on the load balancer that can kill the realtime
    feature silently. The Go hub pings every `realtime_ping_interval_seconds`
    and reaps a peer that does not answer within
    `realtime_pong_timeout_seconds`; those pings are the only traffic on an idle
    board, so they are what keeps this timer from firing. Set below the ping
    interval and every live board goes dead on a timer, with no error anywhere
    -- the symptom is "realtime works locally but not deployed", which is a
    miserable thing to debug.

    The validation below refuses a value that is not at least twice the worst
    case gap between bytes (ping interval + pong wait). 60 against a 25s ping and
    a 10s pong wait is 70 -- so the default here is 120, which is 4.8x the ping
    interval and leaves room to raise the ping interval later without silently
    crossing a line.
  EOT
  type        = number
  default     = 120

  validation {
    condition     = var.idle_timeout_seconds >= 1 && var.idle_timeout_seconds <= 4000
    error_message = "idle_timeout_seconds must be between 1 and 4000 (AWS's own bounds)."
  }

  validation {
    condition     = var.idle_timeout_seconds >= 2 * (var.realtime_ping_interval_seconds + var.realtime_pong_timeout_seconds)
    error_message = "idle_timeout_seconds must be at least twice REALTIME_PING_INTERVAL + REALTIME_PONG_TIMEOUT. Below that margin the load balancer starts closing live WebSocket connections that the application believes are healthy, and nothing logs an error."
  }
}

variable "realtime_ping_interval_seconds" {
  description = "Mirror of REALTIME_PING_INTERVAL in the API task definition. Passed in purely so `idle_timeout_seconds` can be validated against it -- the two are set from one variable in the environment, so they cannot drift."
  type        = number

  validation {
    condition     = var.realtime_ping_interval_seconds > 0
    error_message = "realtime_ping_interval_seconds must be positive; the application treats a non-positive value as 'use the default', which would make the idle-timeout check here meaningless."
  }
}

variable "realtime_pong_timeout_seconds" {
  description = "Mirror of REALTIME_PONG_TIMEOUT in the API task definition. See realtime_ping_interval_seconds."
  type        = number

  validation {
    condition     = var.realtime_pong_timeout_seconds > 0
    error_message = "realtime_pong_timeout_seconds must be positive."
  }
}

# ---------------------------------------------------------------------------
# Target group behaviour
# ---------------------------------------------------------------------------

variable "health_check_path" {
  description = <<-EOT
    Path the target groups probe. Both tiers serve `/healthz`: the Go API's
    probes Postgres and Redis and answers 503 if either is down, and the web
    tier's reports only whether its own configuration resolved.

    Note that this path is ALSO blocked by a listener rule (see main.tf). Those
    are not in conflict: a target group health check is issued by the load
    balancer straight to the target's address and port, and never traverses a
    listener or its rules. Reachable by the health check and reachable from the
    internet are different properties, and this configuration wants the first
    without the second.
  EOT
  type        = string
  default     = "/healthz"
}

variable "deregistration_delay_seconds" {
  description = "How long the load balancer keeps sending existing connections to a target that is draining. Must exceed the application's own shutdown budget (HTTP_SHUTDOWN_TIMEOUT, 15s) or a rolling deploy cuts connections the hub was about to close politely."
  type        = number
  default     = 30
}

variable "blocked_paths" {
  description = <<-EOT
    Paths answered with a fixed 404 by both HTTPS listeners instead of being
    forwarded to a target group.

    `/healthz` and `/metrics` are here because neither is a product surface.
    `/healthz` names the dependencies it probed and, per #31, currently discloses
    the raw error from each; `/metrics` does not exist yet (#12) and blocking it
    now means it does not arrive publicly reachable by default.

    404 rather than 403 on purpose: a 403 confirms that something is there.
  EOT
  type        = list(string)
  default     = ["/healthz", "/metrics"]

  validation {
    condition     = length(var.blocked_paths) > 0
    error_message = "blocked_paths must not be empty. If a future change genuinely wants /metrics served publicly, that should be a visible edit to this list rather than an empty default nobody notices."
  }
}

variable "enable_deletion_protection" {
  description = "Block deletion of the load balancer. False in an environment that is meant to be destroyable."
  type        = bool
  default     = false
}

# ---------------------------------------------------------------------------
# Alerting
# ---------------------------------------------------------------------------

variable "target_5xx_alarm_threshold" {
  description = "Number of 5xx responses from targets in one five-minute period that raises the alarm. The Observability standard asks for at least one meaningful alert; this is it."
  type        = number
  default     = 10
}
