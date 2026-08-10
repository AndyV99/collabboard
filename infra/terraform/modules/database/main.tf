resource "aws_db_subnet_group" "this" {
  name       = "${var.name_prefix}-db"
  subnet_ids = var.subnet_ids

  tags = { Name = "${var.name_prefix}-db" }
}

# The application requires TLS to Postgres in a deployed environment, and
# `rds.force_ssl` is the only place that can be made true regardless of what any
# client's connection string says. It is a static parameter, so it takes effect
# at instance creation here rather than needing a later reboot.
#
# Consequence for #102: the API's DSN must carry `sslmode=require` at minimum
# (`verify-full` preferred). Without it the connection is refused outright,
# which is the intended failure -- loud, at startup, not silent.
resource "aws_db_parameter_group" "this" {
  name_prefix = "${var.name_prefix}-pg16-"
  family      = "postgres16"
  description = "CollabBoard Postgres 16 parameters"

  parameter {
    name         = "rds.force_ssl"
    value        = "1"
    apply_method = "pending-reboot"
  }

  # Statement-level latency data for the observability work, without the cost of
  # logging every statement. 1000ms is a starting point, not a measured one.
  parameter {
    name         = "log_min_duration_statement"
    value        = "1000"
    apply_method = "immediate"
  }

  # Connection churn is the first thing to look at when the pool misbehaves, and
  # these two are how you see it at all.
  parameter {
    name         = "log_connections"
    value        = "1"
    apply_method = "immediate"
  }

  parameter {
    name         = "log_disconnections"
    value        = "1"
    apply_method = "immediate"
  }

  lifecycle {
    create_before_destroy = true
  }
}

resource "aws_db_instance" "this" {
  identifier = "${var.name_prefix}-postgres"

  engine = "postgres"
  # Major version only: RDS resolves it to the current 16.x at creation, and
  # auto_minor_version_upgrade keeps it there. Pinning a minor version would
  # mean a patch release turns into a Terraform diff on an unrelated commit.
  engine_version             = "16"
  auto_minor_version_upgrade = true

  instance_class        = var.instance_class
  allocated_storage     = var.allocated_storage
  max_allocated_storage = var.max_allocated_storage
  storage_type          = "gp3"
  storage_encrypted     = true
  kms_key_id            = var.kms_key_arn

  db_name = "collabboard"

  # ------------------------------------------------------------------
  # The master user, and the thing this module deliberately does NOT do.
  # ------------------------------------------------------------------
  #
  # ADR 0001 and ADR 0006: the API connects as `collabboard_app`, a role that is
  # not a superuser, owns nothing and cannot bypass row-level security. RDS
  # cannot create that role -- it hands you a master user with `rds_superuser`,
  # which is precisely the identity ADR 0006 exists to stop the application and
  # the migration chain from using.
  #
  # So this module creates the master credential and then makes it as awkward as
  # possible to misuse:
  #
  #   * `manage_master_user_password` hands generation and storage to RDS. The
  #     password is never an input to Terraform, never appears in a plan, and --
  #     the part that matters -- never lands in Terraform state. There is no
  #     `random_password` here and no variable that accepts one.
  #   * The secret RDS creates is not referenced by any task, and the IAM module
  #     attaches an explicit Deny on its ARN to both ECS roles. Wiring the
  #     application to it is not a shortcut somebody can take by accident; it
  #     requires editing a Deny statement.
  #   * Nothing in this configuration creates `collabboard_owner` or
  #     `collabboard_app`. That is #56, and it stays #56 -- running
  #     `bootstrap-owner.sql` needs a Postgres connection from inside the VPC,
  #     which does not exist until #102.
  #
  # Net effect: the master credential is available to a human operator via
  # Secrets Manager for the one-shot bootstrap #56 describes, and to nothing
  # else.
  username                      = "collabboard_master"
  manage_master_user_password   = true
  master_user_secret_kms_key_id = var.kms_key_arn

  db_subnet_group_name   = aws_db_subnet_group.this.name
  vpc_security_group_ids = var.security_group_ids
  parameter_group_name   = aws_db_parameter_group.this.name

  # Belt and braces with the data tier having no internet gateway route: even if
  # a subnet were moved into a routed tier by mistake, RDS still would not
  # allocate a public address.
  publicly_accessible = false

  multi_az = var.multi_az

  backup_retention_period = var.backup_retention_days
  # UTC. Backup first, then maintenance, both in the small hours for a European
  # timezone -- an arbitrary but deliberate choice, not the AWS random default.
  backup_window            = "03:00-04:00"
  maintenance_window       = "Mon:04:30-Mon:05:30"
  copy_tags_to_snapshot    = true
  delete_automated_backups = true

  deletion_protection       = var.deletion_protection
  skip_final_snapshot       = var.skip_final_snapshot
  final_snapshot_identifier = var.skip_final_snapshot ? null : "${var.name_prefix}-postgres-final"

  # Free at 7 days' retention on every instance class. Turning it on later
  # requires a modification; turning it on now costs nothing.
  performance_insights_enabled          = true
  performance_insights_retention_period = 7
  performance_insights_kms_key_id       = var.kms_key_arn

  # Ships the Postgres log to CloudWatch Logs, which is where the parameters
  # above become readable. Bills per GB ingested; at staging volume that is
  # cents. Deliberately not exporting `upgrade`, which is empty except during a
  # major version upgrade.
  enabled_cloudwatch_logs_exports = ["postgresql"]

  apply_immediately = var.apply_immediately

  tags = { Name = "${var.name_prefix}-postgres" }
}
