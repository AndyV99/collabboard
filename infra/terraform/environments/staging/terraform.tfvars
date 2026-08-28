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
# 0 was a supported setting until #102 and is not any more, for two independent
# reasons. A Fargate task cannot pull its image without egress -- the failure
# mode is a task stuck in PROVISIONING with a CannotPullContainerError, which
# does not obviously read as "network". And with no NAT gateway there is no
# egress address, so `api_ingress_cidrs` is empty and the security-groups module
# rejects it at plan time rather than building an API listener the web tier
# cannot reach.
#
# #101 offered `nat_gateway_count = 0` as the one-variable way to shed half the
# bill. That escape hatch is gone; `terraform destroy` is now the answer, which
# staging is configured to make safe (skip_final_snapshot, force_destroy, no
# deletion protection anywhere).
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

# ===========================================================================
# #102 -- the load balancer and the services behind it.
# ===========================================================================

# ---------------------------------------------------------------------------
# PLACEHOLDERS. These three are the only values in this file that describe
# something outside this repository, and all three must be changed before the
# first apply. `example.com` is reserved by RFC 2606 precisely so that a
# forgotten placeholder cannot resolve to somebody's real domain.
#
# The zone must be in Route 53 in this account: Terraform writes the ACM
# validation records and both alias records into it. A zone that does not exist
# fails during certificate validation, which takes several minutes to give up
# and reports a not-found error that reads like a permissions problem.
# ---------------------------------------------------------------------------

web_hostname    = "staging.collabboard.example.com"
api_hostname    = "api.staging.collabboard.example.com"
route53_zone_id = "Z00000000000000000000"

# Empty: nothing outside this environment's own NAT gateway may reach the API.
# See ADR 0014. An operator's own address is a legitimate temporary entry;
# 0.0.0.0/0 is rejected by a validation.
api_admin_ingress_cidrs = []

# ---------------------------------------------------------------------------
# COST. Roughly $45/month on top of the ~$61 base, and this is the issue that
# finally makes the existing NAT gateway earn its $32.85 -- until now it has
# been paid for and unused.
#
#   Load balancer            ~$16.43/mo + LCU (pennies at this traffic)
#   API, 2 x 0.25 vCPU/0.5GB ~$18.02/mo
#   Web, 1 x 0.25 vCPU/0.5GB  ~$9.01/mo
#   3 Secrets Manager secrets  ~$1.20/mo
#   3 CloudWatch alarms        ~$0.30/mo
#   ECR storage, logs           ~$1/mo
#
# The next cost lever, in order: Fargate Spot (~70% off the task lines, at the
# price of a two-minute eviction notice that disconnects every WebSocket on the
# task), then ARM64 task definitions (~20% off, contingent on #103 building
# arm64 images), then api_min_capacity = 1 -- which is the cheapest and the one
# that costs the most, because the Redis fan-out that makes this project
# interesting has no second instance to fan out to.
# ---------------------------------------------------------------------------

# 256 CPU units = 0.25 vCPU; 512 MiB. Argon2id at the configured cost is ~19 MiB
# and ~50ms of a full vCPU per hash, so a login on a quarter vCPU is nearer
# 200ms and AUTH_ARGON2_MAX_CONCURRENT = 4 serialises the rest. That is an
# acceptable staging login latency and would not be an acceptable production one.
api_cpu    = 256
api_memory = 512
web_cpu    = 256
web_memory = 512

# Two, and not for redundancy alone: with one task, ADR 0005's Redis pub/sub
# fan-out never executes, so the project's headline feature would be running in
# a shape nothing has exercised.
api_min_capacity = 2
api_max_capacity = 6

# One, and this one IS a constraint rather than a cost choice. #69: refresh
# token rotation is per-process, so two web tasks can race on the same browser's
# session and the API's reuse detection signs the user out. Raise this only
# after #69 lands.
web_desired_count = 1

# 120s. The API's hub pings every 25s and reaps a peer that has not answered
# within 10s, so bytes cross an idle WebSocket at least every 25s; the module
# refuses anything below 2 x (25 + 10) = 70. AWS's own default is 60, which
# would work today and would silently stop working the moment somebody raised
# the ping interval.
alb_idle_timeout_seconds       = 120
realtime_ping_interval_seconds = 25
realtime_pong_timeout_seconds  = 10

# Nothing in this repository builds an image yet -- that is #103, and the API
# has no Dockerfile at all. An image must exist at these tags before the first
# apply, or the services never reach steady state and apply fails.
api_image_tag = "bootstrap"
web_image_tag = "bootstrap"

# Same reasoning as attachments_force_destroy: an environment that exists to be
# destroyed should not fail its destroy on a repository holding test images.
ecr_force_delete = true

# 0 purges a deleted secret immediately instead of leaving it billing $0.40/mo
# for a week. Wrong for prod, where a deleted secret is usually a mistake.
secret_recovery_window_days = 0

# Off: per-task custom metrics cost about as much as the tasks at this size, and
# the Observability standard wants a Prometheus endpoint (#12), not CloudWatch
# custom metrics.
container_insights = false

# Both point the same way as db_deletion_protection: this environment must be
# destroyable, because leaving it running is the cost.
alb_deletion_protection = false

# ---------------------------------------------------------------------------
# #103: the deploy identity
# ---------------------------------------------------------------------------

# Half of the only condition protecting the deploy role. See variables.tf.
github_repository = "AndyV99/collabboard"
