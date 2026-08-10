output "primary_endpoint_address" {
  description = "Endpoint to write to. Resolves only inside the VPC."
  value       = aws_elasticache_replication_group.this.primary_endpoint_address
}

output "reader_endpoint_address" {
  description = "Read-only endpoint. Equal to the primary when node_count is 1."
  value       = aws_elasticache_replication_group.this.reader_endpoint_address
}

output "port" {
  description = "Port the replication group listens on."
  value       = aws_elasticache_replication_group.this.port
}

output "transit_encryption_enabled" {
  description = "Whether clients must use TLS. Exported so #102 cannot build a connection string without confronting it."
  value       = aws_elasticache_replication_group.this.transit_encryption_enabled
}
