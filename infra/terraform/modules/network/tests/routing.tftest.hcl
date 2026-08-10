# Unit tests for the parts of the network module that are pure computation:
# where each subnet's CIDR lands, and which NAT gateway each private route table
# points at. Both are arithmetic, both are easy to get subtly wrong, and both are
# expensive to discover wrong -- a mis-derived CIDR is immutable after apply.
#
# `mock_provider` means these run with no AWS credentials and no API calls, so
# they run in CI on a fork PR exactly as they run locally. They test Terraform's
# own evaluation, not AWS's behaviour; what AWS does with a correct
# configuration is not something a test in this repository can prove.
#
#   cd infra/terraform/modules/network && terraform init && terraform test

mock_provider "aws" {}

variables {
  name_prefix        = "collabboard-test"
  vpc_cidr           = "10.20.0.0/16"
  availability_zones = ["us-east-1a", "us-east-1b"]
  nat_gateway_count  = 1
}

# The tier offsets are fixed rather than sequential so that adding a third AZ
# extends each tier instead of renumbering all of them. That only holds if the
# offsets are what this asserts.
run "subnet_cidrs_are_derived_per_tier" {
  command = apply

  assert {
    condition = alltrue([
      aws_subnet.public[0].cidr_block == "10.20.0.0/20",
      aws_subnet.public[1].cidr_block == "10.20.16.0/20",
    ])
    error_message = "public subnets should occupy the first two /20s of the VPC"
  }

  assert {
    condition = alltrue([
      aws_subnet.private[0].cidr_block == "10.20.64.0/20",
      aws_subnet.private[1].cidr_block == "10.20.80.0/20",
    ])
    error_message = "private subnets should start at offset 4 (10.20.64.0/20)"
  }

  assert {
    condition = alltrue([
      aws_subnet.data[0].cidr_block == "10.20.128.0/20",
      aws_subnet.data[1].cidr_block == "10.20.144.0/20",
    ])
    error_message = "data subnets should start at offset 8 (10.20.128.0/20)"
  }

  # Three tiers x two AZs. A tier silently losing a subnet would still plan.
  assert {
    condition     = length(aws_subnet.public) == 2 && length(aws_subnet.private) == 2 && length(aws_subnet.data) == 2
    error_message = "each tier should have one subnet per availability zone"
  }
}

# The property ADR 0012 rests on. If a 0.0.0.0/0 route into the data route table
# is ever added, this fails -- which is the point: the isolation of the data tier
# should not depend on nobody noticing that a route would be easy to add.
run "data_tier_has_no_route_off_the_vpc" {
  command = apply

  assert {
    condition     = alltrue([for r in aws_route.private_nat : r.route_table_id != aws_route_table.data.id])
    error_message = "the data route table must never carry a default route -- RDS and ElastiCache live there"
  }

  assert {
    condition     = aws_route.public_internet.route_table_id == aws_route_table.public.id
    error_message = "the internet gateway route must be on the public route table only"
  }

  # A gateway endpoint is not a route in `aws_route`, it is a prefix-list route
  # the endpoint injects into every route table it is associated with -- so the
  # assertion above cannot see it. Associating the S3 endpoint with the data
  # route table would give the tier a route to every bucket in every AWS
  # account, which is exactly the kind of egress this tier is supposed not to
  # have, while `aws_route.private_nat` stayed empty and looked fine.
  assert {
    condition     = !contains(aws_vpc_endpoint.s3.route_table_ids, aws_route_table.data.id)
    error_message = "the S3 gateway endpoint must not be associated with the data route table -- it injects a route covering all of S3"
  }

  assert {
    condition     = length(setintersection(aws_vpc_endpoint.s3.route_table_ids, toset(aws_route_table.private[*].id))) == 2
    error_message = "the S3 gateway endpoint should be associated with every private route table"
  }
}

# Public subnets hold an ALB and NAT gateways, both of which bring their own
# addresses. Auto-assigning public IPs here would silently give an internet
# address to anything else that ever lands in the tier.
run "public_subnets_do_not_auto_assign_public_ips" {
  command = apply

  assert {
    condition     = alltrue([for s in aws_subnet.public : s.map_public_ip_on_launch == false])
    error_message = "public subnets must not auto-assign public IPs"
  }
}

# One NAT shared by both AZs. The min() indexing that makes this work is the
# single most fiddly expression in the module.
run "single_nat_is_shared_by_every_az" {
  command = apply

  variables {
    nat_gateway_count = 1
  }

  assert {
    condition     = length(aws_nat_gateway.this) == 1 && length(aws_eip.nat) == 1
    error_message = "one NAT gateway and one Elastic IP"
  }

  assert {
    condition     = length(aws_route.private_nat) == 2
    error_message = "both private route tables need a default route even when they share one NAT"
  }

  assert {
    condition     = alltrue([for r in aws_route.private_nat : r.nat_gateway_id == aws_nat_gateway.this[0].id])
    error_message = "with one NAT, every private route table must point at it"
  }
}

# The textbook shape: no shared failure domain between AZs.
run "one_nat_per_az_pairs_each_route_table_with_its_own" {
  command = apply

  variables {
    nat_gateway_count = 2
  }

  assert {
    condition     = length(aws_nat_gateway.this) == 2
    error_message = "two NAT gateways"
  }

  assert {
    condition = alltrue([
      aws_route.private_nat[0].nat_gateway_id == aws_nat_gateway.this[0].id,
      aws_route.private_nat[1].nat_gateway_id == aws_nat_gateway.this[1].id,
    ])
    error_message = "with one NAT per AZ, each private route table must use the NAT in its own AZ"
  }
}

# The cheap setting, and the one most likely to be wrong: it must produce a
# working VPC with no egress rather than a broken one. Nothing runs in the
# private subnets until #102, so this is a legitimate state to leave staging in.
run "zero_nat_creates_no_egress_and_no_charge" {
  command = apply

  variables {
    nat_gateway_count = 0
  }

  assert {
    condition     = length(aws_nat_gateway.this) == 0 && length(aws_eip.nat) == 0
    error_message = "nat_gateway_count = 0 must create no NAT gateway and no Elastic IP -- both of which bill"
  }

  assert {
    condition     = length(aws_route.private_nat) == 0
    error_message = "with no NAT there is nothing for a default route to point at"
  }

  # The tiers still exist and the data tier is unaffected, which is why 0 is a
  # usable setting rather than a broken one.
  assert {
    condition     = length(aws_subnet.private) == 2 && length(aws_subnet.data) == 2
    error_message = "removing NAT must not remove the subnets themselves"
  }
}

run "rejects_a_single_availability_zone" {
  command = plan

  variables {
    availability_zones = ["us-east-1a"]
  }

  expect_failures = [var.availability_zones]
}

run "rejects_more_nat_gateways_than_availability_zones" {
  command = plan

  variables {
    nat_gateway_count = 3
  }

  expect_failures = [var.nat_gateway_count]
}
