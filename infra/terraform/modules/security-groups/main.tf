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

# Deliberately no ingress rule here. The ALB and its listener arrive in #102,
# and the rule admitting it should land in the same change as the thing it
# admits -- an ingress rule with no corresponding listener is a hole that looks
# intentional.

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
