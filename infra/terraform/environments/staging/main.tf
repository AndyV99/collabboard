data "aws_caller_identity" "current" {}

locals {
  name_prefix = "${var.project}-${var.environment}"

  # Secrets Manager namespace for this environment. #56 creates
  # `collabboard/staging/db/app` and `collabboard/staging/db/owner` underneath
  # it; the execution role is already scoped to read anything here, so #56 adds
  # secrets rather than also having to widen an IAM policy.
  secret_name_prefix = "${var.project}/${var.environment}/"
}

# ---------------------------------------------------------------------------
# KMS
# ---------------------------------------------------------------------------

# One customer-managed key for this environment's data at rest: RDS storage and
# its automated backups, the RDS-managed master secret, ElastiCache, S3
# attachments, and the secrets #56 will add.
#
# One key rather than four because the access boundary is the same for all of
# them -- this environment -- and the `kms:ViaService` conditions in the IAM
# module already prevent a role from using it for a service it has no business
# using. Four keys would be $3/mo more for a distinction the policies already
# make. A CMK at all rather than the free AWS-managed keys because destroying
# this key is a single, auditable action that renders every byte of the
# environment's data unreadable, which AWS-managed keys cannot offer.
resource "aws_kms_key" "data" {
  description             = "${local.name_prefix} data at rest"
  deletion_window_in_days = 30
  enable_key_rotation     = true
}

resource "aws_kms_alias" "data" {
  name          = "alias/${local.name_prefix}-data"
  target_key_id = aws_kms_key.data.key_id
}

# ---------------------------------------------------------------------------
# Network
# ---------------------------------------------------------------------------

module "network" {
  source = "../../modules/network"

  name_prefix        = local.name_prefix
  vpc_cidr           = var.vpc_cidr
  availability_zones = var.availability_zones
  nat_gateway_count  = var.nat_gateway_count
}

module "security_groups" {
  source = "../../modules/security-groups"

  name_prefix = local.name_prefix
  vpc_id      = module.network.vpc_id

  api_container_port = var.api_container_port
  web_container_port = var.web_container_port
  alb_api_port       = var.alb_api_port

  # The whole of ADR 0014's exposure argument, in one expression. The Go API's
  # only client is the Next.js tier, which lives in a private subnet and reaches
  # an internet-facing load balancer through this environment's own NAT gateway
  # -- so the set of addresses that legitimately talk to the API is exactly the
  # set of NAT gateway addresses, plus whatever an operator has explicitly opened
  # for debugging.
  #
  # This is also why `nat_gateway_count = 0` is no longer a supported setting
  # once the ECS service exists: with no NAT there is no egress address, the list
  # is empty, and the security-groups module rejects it at plan time rather than
  # building a listener nothing can reach.
  api_ingress_cidrs = concat(
    [for ip in module.network.nat_gateway_public_ips : "${ip}/32"],
    var.api_admin_ingress_cidrs,
  )
}

# ---------------------------------------------------------------------------
# Data layer. Both of these sit in the data subnets, which have no route to an
# internet or NAT gateway -- see modules/network/main.tf.
# ---------------------------------------------------------------------------

module "database" {
  source = "../../modules/database"

  name_prefix        = local.name_prefix
  subnet_ids         = module.network.data_subnet_ids
  security_group_ids = [module.security_groups.database_security_group_id]
  kms_key_arn        = aws_kms_key.data.arn

  instance_class        = var.db_instance_class
  allocated_storage     = var.db_allocated_storage
  max_allocated_storage = var.db_max_allocated_storage
  multi_az              = var.db_multi_az
  backup_retention_days = var.db_backup_retention_days
  deletion_protection   = var.db_deletion_protection
  skip_final_snapshot   = var.db_skip_final_snapshot
  apply_immediately     = var.apply_immediately
}

module "cache" {
  source = "../../modules/cache"

  name_prefix        = local.name_prefix
  subnet_ids         = module.network.data_subnet_ids
  security_group_ids = [module.security_groups.cache_security_group_id]
  kms_key_arn        = aws_kms_key.data.arn

  node_type         = var.cache_node_type
  node_count        = var.cache_node_count
  apply_immediately = var.apply_immediately
}

module "storage" {
  source = "../../modules/storage"

  name_prefix   = local.name_prefix
  bucket_suffix = data.aws_caller_identity.current.account_id
  kms_key_arn   = aws_kms_key.data.arn
  force_destroy = var.attachments_force_destroy
}

# ---------------------------------------------------------------------------
# IAM for the ECS service that arrives in #102
# ---------------------------------------------------------------------------

module "iam" {
  source = "../../modules/iam"

  name_prefix            = local.name_prefix
  secret_name_prefix     = local.secret_name_prefix
  attachments_bucket_arn = module.storage.bucket_arn
  kms_key_arn            = aws_kms_key.data.arn
  ecr_repository_prefix  = var.project

  # The single most important line in this file. See modules/iam/main.tf and
  # ADR 0001: no ECS role may read the RDS master credential, so the application
  # cannot be wired to a role that bypasses row-level security. #102 added two
  # more roles -- the web tier's and the administrative task's -- and both carry
  # the same Deny, including the one whose entire job is to run SQL as the master
  # user. See ADR 0013 for why that is not a contradiction.
  denied_secret_arns = [module.database.master_user_secret_arn]
}

# ---------------------------------------------------------------------------
# Secrets
#
# Containers only. There is no `aws_secretsmanager_secret_version` anywhere in
# this configuration and no variable that accepts a credential, so no secret
# value passes through a plan, an output or Terraform state -- which is what
# keeps `terraform.tfvars` safe to commit.
#
# Creating the empty secrets here rather than in #56 is a change from what
# #101's OPERATOR-INPUTS.md anticipated, and it is deliberate: a task
# definition's `secrets:` entry needs an ARN at plan time. #102 creates the
# named container, #56 and the operator write the values. A task started against
# a secret with no version fails to start with a ResourceInitializationError,
# which is a loud and accurate way to say "nobody has run #56 yet".
# ---------------------------------------------------------------------------

resource "aws_secretsmanager_secret" "database_app" {
  name        = "${local.secret_name_prefix}db/app"
  description = "Credential for the serving role ${var.database_app_user}. JSON with `username` and `password`. Written by #56, read by the API execution role only."
  kms_key_id  = aws_kms_key.data.arn

  recovery_window_in_days = var.secret_recovery_window_days

  tags = { Name = "${local.name_prefix}-db-app" }
}

resource "aws_secretsmanager_secret" "database_owner" {
  name        = "${local.secret_name_prefix}db/owner"
  description = "Credential for the schema owner ${var.database_owner_user}. JSON with `username` and `password`. Consumed only by the migrate and provision task definitions."
  kms_key_id  = aws_kms_key.data.arn

  recovery_window_in_days = var.secret_recovery_window_days

  tags = { Name = "${local.name_prefix}-db-owner" }
}

# Not covered by #56, and it has to exist before anything starts: apps/api
# refuses to load a configuration without it outside development, because a
# short or absent signing key makes every access token forgeable. Terraform does
# not generate it -- a `random_password` would put it in state, which is the one
# thing this stack has been careful never to do.
resource "aws_secretsmanager_secret" "jwt" {
  name        = "${local.secret_name_prefix}auth/jwt"
  description = "AUTH_JWT_SECRET. A plain string of at least 32 bytes, written once by an operator. See OPERATOR-INPUTS.md."
  kms_key_id  = aws_kms_key.data.arn

  recovery_window_in_days = var.secret_recovery_window_days

  tags = { Name = "${local.name_prefix}-auth-jwt" }
}

# ---------------------------------------------------------------------------
# Public entry point and compute -- #102
# ---------------------------------------------------------------------------

module "alb" {
  source = "../../modules/alb"

  name_prefix       = local.name_prefix
  vpc_id            = module.network.vpc_id
  public_subnet_ids = module.network.public_subnet_ids
  security_group_id = module.security_groups.alb_security_group_id

  web_hostname    = var.web_hostname
  api_hostname    = var.api_hostname
  route53_zone_id = var.route53_zone_id

  api_port           = var.alb_api_port
  api_container_port = var.api_container_port
  web_container_port = var.web_container_port

  idle_timeout_seconds = var.alb_idle_timeout_seconds

  # Passed in so the idle timeout above can be validated against them rather than
  # compared by hand. The same two variables set REALTIME_PING_INTERVAL and
  # REALTIME_PONG_TIMEOUT in the API task definition below, so there is exactly
  # one place either number is written.
  realtime_ping_interval_seconds = var.realtime_ping_interval_seconds
  realtime_pong_timeout_seconds  = var.realtime_pong_timeout_seconds

  enable_deletion_protection = var.alb_deletion_protection
}

module "ecs" {
  source = "../../modules/ecs"

  name_prefix = local.name_prefix
  app_env     = var.environment

  private_subnet_ids      = module.network.private_subnet_ids
  app_security_group_id   = module.security_groups.app_security_group_id
  web_security_group_id   = module.security_groups.web_security_group_id
  admin_security_group_id = module.security_groups.admin_security_group_id

  api_execution_role_arn   = module.iam.execution_role_arn
  api_task_role_arn        = module.iam.task_role_arn
  web_execution_role_arn   = module.iam.web_execution_role_arn
  admin_execution_role_arn = module.iam.admin_execution_role_arn
  admin_task_role_arn      = module.iam.admin_task_role_arn

  api_log_group_name   = module.iam.api_log_group_name
  web_log_group_name   = module.iam.web_log_group_name
  admin_log_group_name = module.iam.admin_log_group_name
  exec_log_group_name  = module.iam.exec_log_group_name

  # Must match `ecr_repository_prefix` on the iam module above, or the execution
  # role's pull is denied.
  ecr_namespace    = var.project
  api_image_tag    = var.api_image_tag
  web_image_tag    = var.web_image_tag
  ecr_force_delete = var.ecr_force_delete

  api_target_group_arn = module.alb.api_target_group_arn
  web_target_group_arn = module.alb.web_target_group_arn
  api_container_port   = var.api_container_port
  web_container_port   = var.web_container_port
  api_url              = module.alb.api_url
  web_hostname         = var.web_hostname
  api_hostname         = var.api_hostname

  api_cpu           = var.api_cpu
  api_memory        = var.api_memory
  web_cpu           = var.web_cpu
  web_memory        = var.web_memory
  api_min_capacity  = var.api_min_capacity
  api_max_capacity  = var.api_max_capacity
  web_desired_count = var.web_desired_count

  database_host       = module.database.address
  database_port       = module.database.port
  database_name       = module.database.database_name
  database_app_user   = var.database_app_user
  database_owner_user = var.database_owner_user
  cache_host          = module.cache.primary_endpoint_address
  cache_port          = module.cache.port

  app_database_secret_arn   = aws_secretsmanager_secret.database_app.arn
  owner_database_secret_arn = aws_secretsmanager_secret.database_owner.arn
  jwt_secret_arn            = aws_secretsmanager_secret.jwt.arn

  realtime_ping_interval_seconds = var.realtime_ping_interval_seconds
  realtime_pong_timeout_seconds  = var.realtime_pong_timeout_seconds

  container_insights    = var.container_insights
  wait_for_steady_state = var.wait_for_steady_state
}
