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
  # ADR 0001: neither ECS role may read the RDS master credential, so the
  # application cannot be wired to a role that bypasses row-level security.
  denied_secret_arns = [module.database.master_user_secret_arn]
}
