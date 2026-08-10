# Staging. Auto-loaded by Terraform, so there is no `-var-file` flag to forget
# and no way to plan this directory against another environment's values.
#
# Committed on purpose. Nothing here is sensitive: no variable in this
# configuration accepts a credential, so this file is sizing, region and CIDRs.

aws_region  = "us-east-1"
environment = "staging"

# 10.20.0.0/16. Reserved allocation across environments so a future peering or
# transit gateway does not collide: dev 10.10.0.0/16, staging 10.20.0.0/16,
# prod 10.30.0.0/16. Invented for this project -- nothing outside this repo
# depends on it yet, so it is cheap to change now and expensive later.
vpc_cidr = "10.20.0.0/16"

# Named rather than discovered via a data source, so `terraform plan` is stable.
availability_zones = ["us-east-1a", "us-east-1b"]

# ---------------------------------------------------------------------------
# COST. The knob below is roughly half the monthly bill of this environment.
# ---------------------------------------------------------------------------
#
# 1 NAT gateway: ~$32.85/mo + $0.045/GB processed.
#
# Set to 0 while nothing runs in the private subnets and this environment costs
# ~$27/mo instead of ~$60/mo -- RDS and ElastiCache are in the data tier, which
# has no egress in any configuration, so they are unaffected.
#
# Must be at least 1 before #102: a Fargate task cannot pull its image without
# egress, and the failure mode is a task stuck in PROVISIONING with a
# CannotPullContainerError, which does not obviously read as "network".
#
# 2 is the textbook answer. It buys AZ-independent egress and costs $32.85/mo
# more; with a single-AZ database in this environment, an AZ failure takes the
# database out regardless, so the second NAT would be protecting the less
# fragile half. See ADR 0012.
nat_gateway_count = 1

# ---------------------------------------------------------------------------
# Data layer -- deliberately the cheap shape. See ADR 0012 and the PR for what
# each of these trades away.
# ---------------------------------------------------------------------------

# db.t4g.micro: 2 vCPU burstable, 1 GiB. ~$11.68/mo.
db_instance_class = "db.t4g.micro"

# Single-AZ. Multi-AZ doubles the instance cost to ~$23.36/mo and is what makes
# an AZ failure survivable rather than a restore. Staging is not worth that;
# prod would be.
db_multi_az = false

db_allocated_storage = 20

# Storage autoscaling ceiling. It is one-way -- RDS never scales storage back
# down -- so this is the real upper bound on the storage line, not 20 GB. At
# $0.115/GB-mo, hitting this ceiling would take storage from $2.30 to $5.75/mo
# permanently, with no Terraform diff to show for it. Set equal to
# db_allocated_storage to disable autoscaling entirely and accept that a full
# disk takes the database down instead.
db_max_allocated_storage = 50

db_backup_retention_days = 7

# Both of these point the same way, and both are wrong for prod on purpose:
# this environment must be destroyable, because leaving it running IS the cost.
db_deletion_protection = false
db_skip_final_snapshot = true

# cache.t4g.micro: 0.5 GiB. ~$11.68/mo. One node, so no automatic failover --
# a node replacement drops every live WebSocket subscription and every queued
# Asynq job not yet in a snapshot.
cache_node_type  = "cache.t4g.micro"
cache_node_count = 1

# Staging attachments are disposable test uploads, and a destroy that fails on
# a non-empty bucket leaves the rest of the environment half-destroyed and
# still billing.
attachments_force_destroy = true

# Staging has no traffic to disrupt, so waiting for a maintenance window to see
# a parameter change take effect is pure delay.
apply_immediately = true
