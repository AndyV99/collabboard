variable "name_prefix" {
  description = "Prefix for every resource name, e.g. `collabboard-staging`."
  type        = string
}

variable "vpc_id" {
  description = "VPC the security groups belong to."
  type        = string
}

variable "postgres_port" {
  description = "Port RDS Postgres listens on."
  type        = number
  default     = 5432
}

variable "redis_port" {
  description = "Port ElastiCache Redis listens on."
  type        = number
  default     = 6379
}

# ---------------------------------------------------------------------------
# Added by #102. Everything below describes the load balancer and the two ECS
# services that sit behind it.
# ---------------------------------------------------------------------------

variable "api_container_port" {
  description = "Port the Go API container listens on (HTTP_PORT). The ALB reaches the API tasks here."
  type        = number
  default     = 8080
}

variable "web_container_port" {
  description = "Port the Next.js container listens on (PORT). The ALB reaches the web tasks here."
  type        = number
  default     = 3000
}

variable "alb_http_port" {
  description = "Listener port that redirects to HTTPS. Open to the internet."
  type        = number
  default     = 80
}

variable "alb_web_port" {
  description = "HTTPS listener port serving the web tier. Open to the internet -- this is the product's front door."
  type        = number
  default     = 443
}

variable "alb_api_port" {
  description = <<-EOT
    HTTPS listener port serving the Go API. Deliberately NOT 443 and
    deliberately NOT open to the internet -- see `api_ingress_cidrs`.

    A separate listener port rather than a second hostname on 443 because an
    ALB's security group is the only per-port access control it has. Two
    hostnames on one listener share one set of ingress rules, so the API would
    inherit the web tier's "open to the world"; two listener ports do not.
  EOT
  type        = number
  default     = 8443
}

variable "api_ingress_cidrs" {
  description = <<-EOT
    The only addresses allowed to reach the API listener. Normally exactly this
    environment's NAT gateway Elastic IPs, because the only client of the Go API
    is the Next.js web tier (ADR 0010: the browser talks to the web origin and
    the web server holds the WebSocket), and a task in a private subnet reaches
    an internet-facing ALB through its own NAT gateway.

    An empty list is rejected: it would produce a listener nothing can reach,
    which looks like a working deployment until the first page render.
  EOT
  type        = list(string)

  validation {
    condition     = length(var.api_ingress_cidrs) > 0
    error_message = "api_ingress_cidrs must name at least one address -- normally the NAT gateway EIPs. An empty list creates an API listener the web tier cannot reach, and the symptom is a working ALB serving a web app whose every request times out."
  }
}
