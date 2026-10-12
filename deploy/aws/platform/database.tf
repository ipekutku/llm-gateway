# PostgreSQL 18, the major version CI tests, on the smallest class the Free
# plan allows (db.t3.micro or db.t4g.micro, Single-AZ only). It lives in the
# private subnets, accepts connections only from the nodes, and only over TLS.
resource "aws_db_subnet_group" "postgres" {
  name       = local.name
  subnet_ids = module.vpc.private_subnets
}

resource "aws_db_parameter_group" "postgres" {
  name   = "${local.name}-postgres18"
  family = "postgres18"

  # Already the default for PostgreSQL 15 and later on RDS; set here so the
  # requirement does not depend on a default.
  parameter {
    name  = "rds.force_ssl"
    value = "1"
    # Static parameter; match AWS's method rather than the provider's
    # immediate default, which otherwise produces a repeated plan diff.
    apply_method = "pending-reboot"
  }
}

resource "aws_security_group" "postgres" {
  name        = "${local.name}-postgres"
  description = "PostgreSQL from the EKS nodes only"
  vpc_id      = module.vpc.vpc_id
}

# Pods get VPC addresses on the nodes' network interfaces, which carry the
# node security group, so this admits pods and nothing else in the VPC.
resource "aws_vpc_security_group_ingress_rule" "postgres_from_nodes" {
  security_group_id            = aws_security_group.postgres.id
  description                  = "PostgreSQL from the EKS nodes and their pods"
  referenced_security_group_id = module.eks.node_security_group_id
  ip_protocol                  = "tcp"
  from_port                    = 5432
  to_port                      = 5432
}

resource "aws_db_instance" "postgres" {
  identifier     = local.name
  engine         = "postgres"
  engine_version = "18.6"
  instance_class = "db.t4g.micro"

  # Pinned exactly; the instance lives for one session, so a newer minor
  # version arrives by changing this line, not during a session.
  auto_minor_version_upgrade = false

  # gp3's minimum size. Encrypted with the AWS managed aws/rds key, which has
  # no monthly charge, unlike a customer-managed key.
  allocated_storage = 20
  storage_type      = "gp3"
  storage_encrypted = true

  db_name  = "gateway"
  username = "gateway_admin"

  # RDS generates the master password and keeps it in its own Secrets Manager
  # secret, so it never appears in Terraform configuration or state.
  manage_master_user_password = true

  db_subnet_group_name   = aws_db_subnet_group.postgres.name
  vpc_security_group_ids = [aws_security_group.postgres.id]
  parameter_group_name   = aws_db_parameter_group.postgres.name
  publicly_accessible    = false
  multi_az               = false

  # The CA that signs the server certificate; clients verify it with the
  # Region's RDS bundle (sslmode=verify-full).
  ca_cert_identifier = "rds-ca-rsa2048-g1"

  # A session database: no backups, snapshots, or deletion protection, which
  # would outlive the session and keep billing. Performance Insights, enhanced
  # monitoring, and log exports stay off for the same reason.
  backup_retention_period      = 0
  delete_automated_backups     = true
  skip_final_snapshot          = true
  deletion_protection          = false
  performance_insights_enabled = false
  apply_immediately            = true
}
