output "vpc_id" {
  description = "ID of the VPC."
  value       = aws_vpc.this.id
}

output "vpc_cidr" {
  description = "CIDR block of the VPC."
  value       = aws_vpc.this.cidr_block
}

output "public_subnet_ids" {
  description = "Public subnet IDs, one per AZ. For the ALB in #102."
  value       = aws_subnet.public[*].id
}

output "private_subnet_ids" {
  description = "Private subnet IDs, one per AZ. For ECS tasks in #102."
  value       = aws_subnet.private[*].id
}

output "private_subnet_cidrs" {
  description = "CIDR blocks of the private subnets -- where the ECS tasks run. NOT the trusted-proxy list: #101 exported this for #33 on the assumption that the load balancer's address would be a private one, and it is not. See public_subnet_cidrs."
  value       = aws_subnet.private[*].cidr_block
}

output "public_subnet_cidrs" {
  description = <<-EOT
    CIDR blocks of the public subnets, which is where the load balancer's
    network interfaces live -- modules/alb attaches the ALB to
    public_subnet_ids, so an ECS task in a private subnet sees a peer address
    from *this* range, not from its own.

    This is what HTTP_TRUSTED_PROXIES has to contain (#33). Getting it wrong by
    naming the private ranges instead leaves the load balancer untrusted, which
    is not a loud failure: ClientIP() returns the ALB's address for every
    request, the per-address login budget collapses into one bucket shared by
    every user, and a single attacker locks everybody out.
  EOT
  value       = aws_subnet.public[*].cidr_block
}

output "data_subnet_ids" {
  description = "Data subnet IDs, one per AZ. RDS and ElastiCache only; these have no route off the VPC."
  value       = aws_subnet.data[*].id
}

output "nat_gateway_public_ips" {
  description = "Public IPs of the NAT gateways -- the addresses this environment egresses from, for any third party that allowlists by IP. Empty when nat_gateway_count is 0."
  value       = aws_eip.nat[*].public_ip
}
