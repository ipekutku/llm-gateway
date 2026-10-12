# Valkey for the shared rate limits: one cache.t4g.micro node, no replica, in
# the private subnets, reachable only from the nodes, over TLS with an AUTH
# token. CI tests the gateway against Redis 8; the gateway uses only basic
# commands and Lua scripts; gateway compatibility still needs the later live
# AWS deployment check, not a claim based on CI.
resource "aws_elasticache_subnet_group" "redis" {
  name       = local.name
  subnet_ids = module.vpc.private_subnets
}

resource "aws_security_group" "redis" {
  name        = "${local.name}-redis"
  description = "Valkey from the EKS nodes only"
  vpc_id      = module.vpc.vpc_id
}

resource "aws_vpc_security_group_ingress_rule" "redis_from_nodes" {
  security_group_id            = aws_security_group.redis.id
  description                  = "Valkey from the EKS nodes and their pods"
  referenced_security_group_id = module.eks.node_security_group_id
  ip_protocol                  = "tcp"
  from_port                    = 6379
  to_port                      = 6379
}

# A replication group with one node, because a single cache cluster cannot
# take an AUTH token.
resource "aws_elasticache_replication_group" "redis" {
  replication_group_id = local.name
  description          = "Shared rate limits for llm-gateway"

  # Pinned for repeatable sessions; CI still tests Redis 8, not this engine.
  engine                     = "valkey"
  engine_version             = "9.1"
  node_type                  = "cache.t4g.micro"
  port                       = 6379
  cluster_mode               = "disabled"
  auto_minor_version_upgrade = false

  num_cache_clusters         = 1
  automatic_failover_enabled = false
  multi_az_enabled           = false

  subnet_group_name  = aws_elasticache_subnet_group.redis.name
  security_group_ids = [aws_security_group.redis.id]

  # Clients connect with rediss://; the server certificate chains to Amazon's
  # public roots, which standard trust stores already hold.
  at_rest_encryption_enabled = true
  transit_encryption_enabled = true
  transit_encryption_mode    = "required"

  # The same token the gateway reads from Secrets Manager (see secrets.tf).
  auth_token                 = random_password.redis_auth_token.result
  auth_token_update_strategy = "SET"

  snapshot_retention_limit = 0
  apply_immediately        = true
}
