# Every rule reaching Postgres or Redis is in this file. Read top to bottom, the
# reachability story is: the app SG can egress; the database and cache SGs admit
# exactly one port from exactly one source SG and can egress nowhere.
#
# Rules are separate `aws_vpc_security_group_*_rule` resources rather than inline
# `ingress`/`egress` blocks so that each rule is its own plan line with its own
# description -- an inline block change reads as "the whole security group
# changed", which is exactly the diff you do not want to skim on a review.
#
# Note on egress: AWS attaches an allow-all egress rule to every new security
# group. The `aws_security_group` resource removes it on create, so a group with
# no egress rule resource below genuinely has no egress.

# --------------------------------------------------------------------------
# App tier -- attached to the ECS task ENI in #102. Created here because it is
# the *source* half of every rule below; without it there is no way to express
# "only the API" other than a CIDR, which would also match anything else that
# ever lands in the private subnets.
# --------------------------------------------------------------------------

resource "aws_security_group" "app" {
  # `name_prefix`, not `name`: paired with create_before_destroy below, a fixed
  # name would collide with itself on any replacement, because the new group is
  # created before the old one is gone.
  name_prefix = "${var.name_prefix}-app-"
  description = "CollabBoard API tasks"
  vpc_id      = var.vpc_id

  tags = { Name = "${var.name_prefix}-app" }

  lifecycle {
    create_before_destroy = true
  }
}

# #101 left this group with no ingress on purpose, so that the rule admitting the
# load balancer would land in the same change as the load balancer. #102 is that
# change, and the rule is below (`app_from_alb`).

resource "aws_vpc_security_group_egress_rule" "app_all" {
  security_group_id = aws_security_group.app.id
  description       = "Outbound to anywhere: image pulls, Secrets Manager, Stripe, OTLP export"
  ip_protocol       = "-1"
  cidr_ipv4         = "0.0.0.0/0"
}

# --------------------------------------------------------------------------
# Database tier
# --------------------------------------------------------------------------

resource "aws_security_group" "database" {
  name_prefix = "${var.name_prefix}-database-"
  description = "RDS Postgres. Reachable only from the app tier."
  vpc_id      = var.vpc_id

  tags = { Name = "${var.name_prefix}-database" }

  lifecycle {
    create_before_destroy = true
  }
}

resource "aws_vpc_security_group_ingress_rule" "database_from_app" {
  security_group_id            = aws_security_group.database.id
  description                  = "Postgres from the API tasks"
  referenced_security_group_id = aws_security_group.app.id
  ip_protocol                  = "tcp"
  from_port                    = var.postgres_port
  to_port                      = var.postgres_port
}

# No egress rule. RDS does not initiate connections for anything this project
# uses -- backups, logs and Performance Insights all move over the AWS service
# path, not the VPC route table. Combined with the data subnets having no
# default route, this is two independent reasons the database cannot reach out.

# --------------------------------------------------------------------------
# Cache tier
# --------------------------------------------------------------------------

resource "aws_security_group" "cache" {
  name_prefix = "${var.name_prefix}-cache-"
  description = "ElastiCache Redis. Reachable only from the app tier."
  vpc_id      = var.vpc_id

  tags = { Name = "${var.name_prefix}-cache" }

  lifecycle {
    create_before_destroy = true
  }
}

resource "aws_vpc_security_group_ingress_rule" "cache_from_app" {
  security_group_id            = aws_security_group.cache.id
  description                  = "Redis from the API tasks"
  referenced_security_group_id = aws_security_group.app.id
  ip_protocol                  = "tcp"
  from_port                    = var.redis_port
  to_port                      = var.redis_port
}

# No egress rule, for the same reason as the database.

# --------------------------------------------------------------------------
# Load balancer -- #102
#
# One ALB, three listener ports, and the ports are what separate "public" from
# "not public". 80 and 443 serve the Next.js web tier and are open to the
# internet, because that is the product. 8443 serves the Go API and is open to
# nothing but this environment's own NAT egress addresses.
#
# That asymmetry is the whole security argument of ADR 0014. Nothing in a
# browser ever calls the Go API -- ADR 0010 made the Next server hold the
# WebSocket and relay it to the page as SSE -- so the API's only client is a task
# in a private subnet, which reaches an internet-facing ALB via its own NAT
# gateway and therefore arrives from a known, fixed address. Publishing
# /api/v1/auth/login to the internet would buy nothing and would make #73's
# unbudgeted address-existence oracle and #33's broken per-address login budget
# reachable by anyone.
# --------------------------------------------------------------------------

resource "aws_security_group" "alb" {
  name_prefix = "${var.name_prefix}-alb-"
  description = "CollabBoard application load balancer"
  vpc_id      = var.vpc_id

  tags = { Name = "${var.name_prefix}-alb" }

  lifecycle {
    create_before_destroy = true
  }
}

resource "aws_vpc_security_group_ingress_rule" "alb_http" {
  # checkov:skip=CKV_AWS_260:The listener on this port issues a 301 to HTTPS and routes to no target group -- see modules/alb, aws_lb_listener.http. Refusing port 80 outright would not improve security; it would mean a person typing the hostname without a scheme gets a connection timeout instead of being redirected, and browsers try http:// first. Skipped inline rather than in .checkov.yml on purpose: a DIFFERENT rule opening port 80 to the internet should still fail the build.
  security_group_id = aws_security_group.alb.id
  description       = "HTTP from anywhere; the listener does nothing but redirect to HTTPS"
  cidr_ipv4         = "0.0.0.0/0"
  ip_protocol       = "tcp"
  from_port         = var.alb_http_port
  to_port           = var.alb_http_port
}

resource "aws_vpc_security_group_ingress_rule" "alb_web" {
  security_group_id = aws_security_group.alb.id
  description       = "HTTPS from anywhere to the web tier"
  cidr_ipv4         = "0.0.0.0/0"
  ip_protocol       = "tcp"
  from_port         = var.alb_web_port
  to_port           = var.alb_web_port
}

# The API listener. `count` rather than `for_each` deliberately: the values are
# NAT gateway public IPs, which are unknown until apply, and a for_each key must
# be known at plan time. The *length* is known -- it is nat_gateway_count -- so
# count plans cleanly where for_each would fail with "value depends on resource
# attributes that cannot be determined until apply".
resource "aws_vpc_security_group_ingress_rule" "alb_api" {
  count = length(var.api_ingress_cidrs)

  security_group_id = aws_security_group.alb.id
  description       = "HTTPS to the Go API from an approved address (normally this environment's NAT gateway)"
  cidr_ipv4         = var.api_ingress_cidrs[count.index]
  ip_protocol       = "tcp"
  from_port         = var.alb_api_port
  to_port           = var.alb_api_port
}

# Egress is scoped to the two target groups' ports rather than left open. An ALB
# only ever originates connections to its own targets, so anything wider is
# range it will never use -- and a compromised load balancer with open egress is
# a pivot point sitting in a public subnet.
resource "aws_vpc_security_group_egress_rule" "alb_to_app" {
  security_group_id            = aws_security_group.alb.id
  description                  = "To the API tasks"
  referenced_security_group_id = aws_security_group.app.id
  ip_protocol                  = "tcp"
  from_port                    = var.api_container_port
  to_port                      = var.api_container_port
}

resource "aws_vpc_security_group_egress_rule" "alb_to_web" {
  security_group_id            = aws_security_group.alb.id
  description                  = "To the web tasks"
  referenced_security_group_id = aws_security_group.web.id
  ip_protocol                  = "tcp"
  from_port                    = var.web_container_port
  to_port                      = var.web_container_port
}

resource "aws_vpc_security_group_ingress_rule" "app_from_alb" {
  security_group_id            = aws_security_group.app.id
  description                  = "HTTP from the load balancer, including the WebSocket upgrade"
  referenced_security_group_id = aws_security_group.alb.id
  ip_protocol                  = "tcp"
  from_port                    = var.api_container_port
  to_port                      = var.api_container_port
}

# --------------------------------------------------------------------------
# Web tier -- #102
#
# A group of its own rather than reusing the app group, because the database and
# cache admit the *app* group by reference. Putting the Next.js tasks in it would
# silently give the rendering tier a direct route to Postgres, which it has no
# business having and no code to use.
# --------------------------------------------------------------------------

resource "aws_security_group" "web" {
  name_prefix = "${var.name_prefix}-web-"
  description = "CollabBoard Next.js tasks"
  vpc_id      = var.vpc_id

  tags = { Name = "${var.name_prefix}-web" }

  lifecycle {
    create_before_destroy = true
  }
}

resource "aws_vpc_security_group_ingress_rule" "web_from_alb" {
  security_group_id            = aws_security_group.web.id
  description                  = "HTTP from the load balancer"
  referenced_security_group_id = aws_security_group.alb.id
  ip_protocol                  = "tcp"
  from_port                    = var.web_container_port
  to_port                      = var.web_container_port
}

# Wide egress, and it has to be: the web tier pulls its image, and every
# server-rendered page calls the Go API through the ALB's public address, which
# means out through the NAT gateway and back in. See ADR 0014 for why that
# hairpin is the price of not publishing the API.
resource "aws_vpc_security_group_egress_rule" "web_all" {
  security_group_id = aws_security_group.web.id
  description       = "Outbound to anywhere: image pulls, the API through the load balancer, OTLP export"
  ip_protocol       = "-1"
  cidr_ipv4         = "0.0.0.0/0"
}

# --------------------------------------------------------------------------
# Administrative tier -- #102
#
# The one-shot task an operator runs to hold a psql session inside the VPC, so
# that #56's `bootstrap-owner.sql` has somewhere to run. See ADR 0013.
#
# Its own group, not the app group, for two reasons. It reaches Postgres and
# nothing else -- there is no rule admitting it to Redis -- and a connection to
# the database can be attributed to "somebody ran the break-glass task" rather
# than being indistinguishable from the API's own traffic.
# --------------------------------------------------------------------------

resource "aws_security_group" "admin" {
  name_prefix = "${var.name_prefix}-admin-"
  description = "CollabBoard one-shot administrative tasks"
  vpc_id      = var.vpc_id

  tags = { Name = "${var.name_prefix}-admin" }

  lifecycle {
    create_before_destroy = true
  }
}

resource "aws_vpc_security_group_ingress_rule" "database_from_admin" {
  security_group_id            = aws_security_group.database.id
  description                  = "Postgres from the one-shot administrative task"
  referenced_security_group_id = aws_security_group.admin.id
  ip_protocol                  = "tcp"
  from_port                    = var.postgres_port
  to_port                      = var.postgres_port
}

# Needs egress for the image pull and, more importantly, for the SSM messages
# channel that ECS Exec runs over -- without it `aws ecs execute-command` fails
# with a timeout rather than a permissions error.
resource "aws_vpc_security_group_egress_rule" "admin_all" {
  security_group_id = aws_security_group.admin.id
  description       = "Outbound to anywhere: image pull and the ECS Exec SSM channel"
  ip_protocol       = "-1"
  cidr_ipv4         = "0.0.0.0/0"
}
