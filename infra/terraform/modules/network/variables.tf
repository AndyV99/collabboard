variable "name_prefix" {
  description = "Prefix for every resource name, e.g. `collabboard-staging`."
  type        = string
}

variable "vpc_cidr" {
  description = "CIDR block for the VPC. Must be a /16 through /20; subnets are carved from it with a 4-bit offset."
  type        = string

  validation {
    condition     = can(cidrsubnet(var.vpc_cidr, 4, 8))
    error_message = "vpc_cidr must be a valid CIDR with room for a 4-bit subnet split (i.e. /20 or larger)."
  }
}

variable "availability_zones" {
  description = "AZs to spread subnets across. Two is the minimum -- an RDS subnet group requires at least two even for a single-AZ instance."
  type        = list(string)

  validation {
    condition     = length(var.availability_zones) >= 2
    error_message = "At least two availability zones are required (RDS DB subnet groups mandate it)."
  }
}

variable "nat_gateway_count" {
  description = <<-EOT
    Number of NAT gateways. This is the single largest recurring line on the
    bill, so it is a deliberate knob rather than a hardcoded shape:

      0 -- no egress from private subnets. Correct and cheapest while nothing
           runs in them. The data tier is unaffected (it never has egress) and
           S3 still works through the gateway endpoint. Costs $0.
      1 -- one NAT in the first AZ, shared by both private route tables.
           ~$32.85/mo plus data processing. An AZ failure takes egress out for
           both AZs, not just its own. This is the staging default.
      2 -- one NAT per AZ, no shared failure domain. ~$65.70/mo.

    A Fargate task cannot pull its container image without egress, so this must
    be at least 1 before the ECS service (#102) will start.
  EOT
  type        = number

  validation {
    condition     = var.nat_gateway_count >= 0 && var.nat_gateway_count <= length(var.availability_zones)
    error_message = "nat_gateway_count must be between 0 and the number of availability zones."
  }
}
