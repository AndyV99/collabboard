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
  description = "CIDR blocks of the private subnets. Exported for #33: these are the addresses the API sees as its peer once an ALB is in front of it, and therefore what a trusted-proxy list would have to contain."
  value       = aws_subnet.private[*].cidr_block
}

output "data_subnet_ids" {
  description = "Data subnet IDs, one per AZ. RDS and ElastiCache only; these have no route off the VPC."
  value       = aws_subnet.data[*].id
}

output "nat_gateway_public_ips" {
  description = "Public IPs of the NAT gateways -- the addresses this environment egresses from, for any third party that allowlists by IP. Empty when nat_gateway_count is 0."
  value       = aws_eip.nat[*].public_ip
}
