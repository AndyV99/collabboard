resource "aws_elasticache_subnet_group" "this" {
  name       = "${var.name_prefix}-cache"
  subnet_ids = var.subnet_ids

  tags = { Name = "${var.name_prefix}-cache" }
}

resource "aws_elasticache_replication_group" "this" {
  replication_group_id = "${var.name_prefix}-redis"
  description          = "CollabBoard Redis: refresh tokens, realtime pub/sub fan-out, Asynq queue"

  engine = "redis"
  # Minor-version-pinned unlike RDS, because ElastiCache treats the version
  # string as the upgrade target rather than a family, so "7" is not accepted.
  engine_version             = "7.1"
  auto_minor_version_upgrade = true
  parameter_group_name       = "default.redis7"

  node_type          = var.node_type
  num_cache_clusters = var.node_count
  port               = 6379

  subnet_group_name  = aws_elasticache_subnet_group.this.name
  security_group_ids = var.security_group_ids

  # Failover needs a replica to fail over to.
  automatic_failover_enabled = var.node_count > 1
  multi_az_enabled           = var.node_count > 1

  at_rest_encryption_enabled = true
  kms_key_id                 = var.kms_key_arn

  # Consequence for #102: the API must connect with TLS -- `rediss://` for a URL,
  # or a non-nil TLSConfig for go-redis/Asynq. A plaintext client fails to
  # connect rather than degrading, which is the right direction for this to fail.
  #
  # Deliberately no `auth_token`. Redis AUTH would add a second credential whose
  # provisioning and rotation has the same shape as the database secrets in #56,
  # and it protects against an attacker who already has a route to the node --
  # which requires membership in the app security group. Worth revisiting when
  # #56 has established the pattern; recorded in ADR 0012 rather than left as an
  # unexplained absence.
  transit_encryption_enabled = true

  snapshot_retention_limit = var.snapshot_retention_days
  snapshot_window          = "05:00-06:00"
  maintenance_window       = "mon:06:30-mon:07:30"

  apply_immediately = var.apply_immediately

  tags = { Name = "${var.name_prefix}-redis" }
}
