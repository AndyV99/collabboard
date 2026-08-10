# Three subnet tiers per AZ. The split is not decoration: the data tier has no
# route to a NAT gateway or an internet gateway at all, so "the database cannot
# reach the internet" is a property of the route table rather than a property of
# a security group somebody could widen later. See ADR 0012.

locals {
  az_count = length(var.availability_zones)

  # /16 -> six /20s, with 10 spare /20s left for tiers this project does not
  # have yet. Fixed offsets rather than sequential indices so that adding a
  # third AZ later extends each tier instead of renumbering all of them.
  public_cidrs  = [for i in range(local.az_count) : cidrsubnet(var.vpc_cidr, 4, i)]
  private_cidrs = [for i in range(local.az_count) : cidrsubnet(var.vpc_cidr, 4, i + 4)]
  data_cidrs    = [for i in range(local.az_count) : cidrsubnet(var.vpc_cidr, 4, i + 8)]
}

resource "aws_vpc" "this" {
  cidr_block = var.vpc_cidr

  # Both required for RDS and ElastiCache to be addressable by their DNS names,
  # which is the only way anything is meant to reach them.
  enable_dns_support   = true
  enable_dns_hostnames = true

  tags = { Name = var.name_prefix }
}

resource "aws_internet_gateway" "this" {
  vpc_id = aws_vpc.this.id

  tags = { Name = var.name_prefix }
}

# --------------------------------------------------------------------------
# Public tier -- ALB (#102) and NAT gateways. Nothing else belongs here.
# --------------------------------------------------------------------------

resource "aws_subnet" "public" {
  count = local.az_count

  vpc_id            = aws_vpc.this.id
  cidr_block        = local.public_cidrs[count.index]
  availability_zone = var.availability_zones[count.index]

  # Off on purpose. The only things intended to live here are an ALB and a NAT
  # gateway, both of which bring their own addresses. Anything that needs this
  # to be true has been put in the wrong tier.
  map_public_ip_on_launch = false

  tags = {
    Name = "${var.name_prefix}-public-${var.availability_zones[count.index]}"
    Tier = "public"
  }
}

resource "aws_route_table" "public" {
  vpc_id = aws_vpc.this.id

  tags = { Name = "${var.name_prefix}-public" }
}

resource "aws_route" "public_internet" {
  route_table_id         = aws_route_table.public.id
  destination_cidr_block = "0.0.0.0/0"
  gateway_id             = aws_internet_gateway.this.id
}

resource "aws_route_table_association" "public" {
  count = local.az_count

  subnet_id      = aws_subnet.public[count.index].id
  route_table_id = aws_route_table.public.id
}

# --------------------------------------------------------------------------
# NAT
# --------------------------------------------------------------------------

resource "aws_eip" "nat" {
  count = var.nat_gateway_count

  domain = "vpc"

  # Tagged by AZ rather than by index, to match the NAT gateway it is attached
  # to. An Elastic IP is a billable line of its own if it ever ends up detached,
  # and `-nat-0` is not attributable in a cost report.
  tags = { Name = "${var.name_prefix}-nat-${var.availability_zones[count.index]}" }
}

resource "aws_nat_gateway" "this" {
  count = var.nat_gateway_count

  allocation_id = aws_eip.nat[count.index].id
  subnet_id     = aws_subnet.public[count.index].id

  tags = { Name = "${var.name_prefix}-nat-${var.availability_zones[count.index]}" }

  depends_on = [aws_internet_gateway.this]
}

# --------------------------------------------------------------------------
# Private tier -- ECS tasks (#102). Egress out, nothing in from the internet.
# --------------------------------------------------------------------------

resource "aws_subnet" "private" {
  count = local.az_count

  vpc_id            = aws_vpc.this.id
  cidr_block        = local.private_cidrs[count.index]
  availability_zone = var.availability_zones[count.index]

  tags = {
    Name = "${var.name_prefix}-private-${var.availability_zones[count.index]}"
    Tier = "private"
  }
}

# One route table per AZ even when a single shared NAT makes them identical, so
# that raising nat_gateway_count from 1 to 2 is a route change rather than a
# route-table split.
resource "aws_route_table" "private" {
  count = local.az_count

  vpc_id = aws_vpc.this.id

  tags = { Name = "${var.name_prefix}-private-${var.availability_zones[count.index]}" }
}

resource "aws_route" "private_nat" {
  count = var.nat_gateway_count > 0 ? local.az_count : 0

  route_table_id         = aws_route_table.private[count.index].id
  destination_cidr_block = "0.0.0.0/0"

  # With one NAT, every AZ shares index 0. With one per AZ, each takes its own.
  nat_gateway_id = aws_nat_gateway.this[min(count.index, var.nat_gateway_count - 1)].id
}

resource "aws_route_table_association" "private" {
  count = local.az_count

  subnet_id      = aws_subnet.private[count.index].id
  route_table_id = aws_route_table.private[count.index].id
}

# --------------------------------------------------------------------------
# Data tier -- RDS and ElastiCache. No default route, in either direction.
# --------------------------------------------------------------------------

resource "aws_subnet" "data" {
  count = local.az_count

  vpc_id            = aws_vpc.this.id
  cidr_block        = local.data_cidrs[count.index]
  availability_zone = var.availability_zones[count.index]

  tags = {
    Name = "${var.name_prefix}-data-${var.availability_zones[count.index]}"
    Tier = "data"
  }
}

# A single route table, shared by both AZs, holding only the VPC-local route
# every route table gets implicitly. There is deliberately no 0.0.0.0/0 entry
# here and no code path that adds one.
resource "aws_route_table" "data" {
  vpc_id = aws_vpc.this.id

  tags = { Name = "${var.name_prefix}-data" }
}

resource "aws_route_table_association" "data" {
  count = local.az_count

  subnet_id      = aws_subnet.data[count.index].id
  route_table_id = aws_route_table.data.id
}

# --------------------------------------------------------------------------
# VPC endpoints
# --------------------------------------------------------------------------

# A gateway endpoint, which unlike an interface endpoint is free. It keeps
# attachment traffic off the NAT gateway entirely -- which is both a data
# processing charge ($0.045/GB) avoided and one fewer reason for the app tier to
# need egress at all. Interface endpoints for ECR/Logs/Secrets Manager would
# remove the remaining reasons, but at $7.30/mo each they cost more than the
# single NAT they would replace; see ADR 0012.
#
# Associated with the PRIVATE route tables only, deliberately. A gateway
# endpoint works by injecting a prefix-list route covering all of S3 in the
# region -- including every bucket in every other AWS account. Adding it to the
# data route table would therefore be a route off the VPC, from the one tier
# whose entire justification is that it has none, and would quietly turn "the
# database cannot reach the internet" into a claim that is no longer true.
# Nothing in the data tier needs it: RDS only reaches S3 for the
# aws_s3_export/aws_s3_import extensions, which this project does not use and
# has not granted, and ElastiCache backups travel the service path rather than
# the route table.
resource "aws_vpc_endpoint" "s3" {
  vpc_id            = aws_vpc.this.id
  service_name      = "com.amazonaws.${data.aws_region.current.region}.s3"
  vpc_endpoint_type = "Gateway"

  route_table_ids = aws_route_table.private[*].id

  tags = { Name = "${var.name_prefix}-s3" }
}

# The VPC's default security group is created by AWS, not by Terraform, and
# ships allowing all traffic from itself and all egress. Nothing here uses it,
# but anything launched without an explicit security group gets it, so adopting
# it and stripping every rule closes that by default. Costs nothing, which is
# the standard ADR 0012 applies to hardening.
resource "aws_default_security_group" "this" {
  vpc_id = aws_vpc.this.id

  # No ingress or egress blocks: the provider revokes every rule on adoption.

  tags = { Name = "${var.name_prefix}-default-do-not-use" }
}

data "aws_region" "current" {}
