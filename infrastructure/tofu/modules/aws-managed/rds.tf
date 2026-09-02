# ---------------------------------------------------------------------------
# Managed PostgreSQL for NEXUS.
#
# This module attaches to an existing VPC and an existing EKS cluster. It does
# not create either, because both are shared infrastructure with a much longer
# lifecycle than one application - and because a module that can delete a VPC
# is a module nobody wants to run.
# ---------------------------------------------------------------------------

locals {
  identifier = "${var.name_prefix}-${var.environment}"

  common_tags = merge(
    {
      "Name"        = local.identifier
      "Application" = "nexus"
      "Environment" = var.environment
      "ManagedBy"   = "opentofu"
      "Module"      = "aws-managed"
    },
    var.tags,
  )

  # The parameter group family follows the engine major version.
  parameter_group_family = "postgres${split(".", var.postgres_version)[0]}"

  master_password = var.master_password != null ? var.master_password : random_password.master.result

  # sslmode=require, not disable: the parameter group below sets rds.force_ssl,
  # so a plaintext connection is refused by the server anyway. verify-full is
  # better still, but it needs the RDS CA bundle mounted into the pods, which
  # is a decision for whoever owns the cluster image.
  database_url = format(
    "postgres://%s:%s@%s:%d/%s?sslmode=require",
    var.master_username,
    local.master_password,
    aws_db_instance.this.address,
    aws_db_instance.this.port,
    var.database_name,
  )
}

# special = false for the same reason as the local module: this password ends
# up inside a URL-shaped DSN, and percent-encoding it correctly through every
# layer that re-parses that DSN is a recurring source of authentication bugs.
# RDS also rejects several punctuation characters outright.
resource "random_password" "master" {
  length  = 40
  special = false
}

# A stable suffix so the final snapshot name is unique per instance without
# using timestamp(), which would make every plan show a change.
resource "random_id" "final_snapshot" {
  byte_length = 4
}

resource "aws_db_subnet_group" "this" {
  name        = "${local.identifier}-db"
  description = "Private subnets for the ${local.identifier} PostgreSQL instance"
  subnet_ids  = var.database_subnet_ids

  tags = local.common_tags
}

resource "aws_security_group" "database" {
  name        = "${local.identifier}-db"
  description = "Ingress to PostgreSQL for the ${local.identifier} NEXUS installation"
  vpc_id      = var.vpc_id

  tags = local.common_tags

  # The security group is referenced by the instance, so replacing it must
  # happen in the right order or the apply fails with a dependency violation.
  lifecycle {
    create_before_destroy = true
  }
}

# Discrete rule resources rather than inline blocks: inline ingress/egress
# blocks are authoritative for the whole group, so anything added out of band
# (or by another module) is silently deleted on the next apply.
resource "aws_vpc_security_group_ingress_rule" "from_security_groups" {
  for_each = toset(var.allowed_security_group_ids)

  security_group_id            = aws_security_group.database.id
  referenced_security_group_id = each.value
  from_port                    = 5432
  to_port                      = 5432
  ip_protocol                  = "tcp"
  description                  = "PostgreSQL from ${each.value}"

  tags = local.common_tags
}

resource "aws_vpc_security_group_ingress_rule" "from_cidrs" {
  for_each = toset(var.allowed_cidr_blocks)

  security_group_id = aws_security_group.database.id
  cidr_ipv4         = each.value
  from_port         = 5432
  to_port           = 5432
  ip_protocol       = "tcp"
  description       = "PostgreSQL from ${each.value}"

  tags = local.common_tags
}

# No egress rules at all. RDS does not initiate outbound connections for
# anything NEXUS uses, and an empty egress set is the correct default rather
# than an oversight.

resource "aws_db_parameter_group" "this" {
  name        = "${local.identifier}-pg"
  family      = local.parameter_group_family
  description = "PostgreSQL parameters for ${local.identifier}"

  # Refuse plaintext connections at the server. Without this, sslmode in the
  # client DSN is the only thing standing between you and an unencrypted
  # session, and a client can always be misconfigured.
  parameter {
    name         = "rds.force_ssl"
    value        = "1"
    apply_method = "pending-reboot"
  }

  # Log statements slower than a second. The cost is a few log lines; the
  # alternative is having no idea which query caused an incident.
  parameter {
    name  = "log_min_duration_statement"
    value = "1000"
  }

  # Log every connection-level lock wait; deadlocks in a job queue are a
  # design smell worth seeing early.
  parameter {
    name  = "log_lock_waits"
    value = "1"
  }

  tags = local.common_tags

  lifecycle {
    create_before_destroy = true
  }
}

resource "aws_db_instance" "this" {
  identifier = local.identifier

  engine         = "postgres"
  engine_version = var.postgres_version
  instance_class = var.instance_class

  db_name  = var.database_name
  username = var.master_username
  password = local.master_password
  port     = 5432

  # gp3 rather than gp2: same or lower price, and IOPS are not tied to volume
  # size, so a small database is not automatically a slow one.
  storage_type          = "gp3"
  allocated_storage     = var.allocated_storage_gb
  max_allocated_storage = var.max_allocated_storage_gb

  storage_encrypted = true
  kms_key_id        = var.kms_key_arn

  db_subnet_group_name   = aws_db_subnet_group.this.name
  parameter_group_name   = aws_db_parameter_group.this.name
  vpc_security_group_ids = [aws_security_group.database.id]

  # This is the single most important line in the file.
  publicly_accessible = false

  multi_az                = var.multi_az
  backup_retention_period = var.backup_retention_days

  # Off-peak UTC, and the backup window must not overlap maintenance.
  backup_window      = "03:00-04:00"
  maintenance_window = "sun:04:30-sun:05:30"

  # Minor versions carry security fixes and are applied inside the maintenance
  # window; majors are never automatic, because they need a tested migration.
  auto_minor_version_upgrade  = true
  allow_major_version_upgrade = false

  # Changes wait for the maintenance window instead of restarting the database
  # the moment someone runs apply. Set it true consciously when you actually
  # want an immediate, possibly disruptive change.
  apply_immediately = false

  performance_insights_enabled = var.performance_insights_enabled

  # Ship server logs to CloudWatch so they survive an instance replacement.
  enabled_cloudwatch_logs_exports = ["postgresql", "upgrade"]

  copy_tags_to_snapshot = true
  deletion_protection   = var.deletion_protection
  skip_final_snapshot   = var.skip_final_snapshot
  final_snapshot_identifier = var.skip_final_snapshot ? null : (
    "${local.identifier}-final-${random_id.final_snapshot.hex}"
  )

  tags = local.common_tags

  lifecycle {
    ignore_changes = [
      # A password rotated out of band (break-glass, a rotation lambda) must
      # not be reverted by the next apply. Rotate deliberately by changing the
      # variable, or move rotation entirely to Secrets Manager.
      password,
    ]
  }
}

# ---------------------------------------------------------------------------
# Credential distribution
# ---------------------------------------------------------------------------

# Secrets Manager holds the same credential the Tofu state holds, so that
# things which must not read state - an external-secrets operator, an on-call
# engineer with break-glass access, a rotation function - have a supported way
# to get it.
resource "aws_secretsmanager_secret" "database" {
  count = var.store_password_in_secrets_manager ? 1 : 0

  name        = "${local.identifier}/database"
  description = "PostgreSQL credentials for the ${local.identifier} NEXUS installation"
  kms_key_id  = var.kms_key_arn

  # Without a short window here, a destroyed-then-recreated environment fails
  # because the old secret name is still reserved for up to 30 days.
  recovery_window_in_days = var.secret_recovery_window_days

  tags = local.common_tags
}

resource "aws_secretsmanager_secret_version" "database" {
  count = var.store_password_in_secrets_manager ? 1 : 0

  secret_id = aws_secretsmanager_secret.database[0].id

  secret_string = jsonencode({
    username = var.master_username
    password = local.master_password
    engine   = "postgres"
    host     = aws_db_instance.this.address
    port     = aws_db_instance.this.port
    dbname   = var.database_name
    # The assembled DSN as well as the parts, so consumers do not each
    # reimplement the sslmode and port conventions.
    NEXUS_DATABASE_URL = local.database_url
  })
}
