output "cluster_name" {
  description = "EKS cluster name."
  value       = module.eks.cluster_name
}

output "database_address" {
  description = "Private host name of the PostgreSQL instance."
  value       = aws_db_instance.postgres.address
}

output "database_name" {
  description = "Database created on the instance."
  value       = aws_db_instance.postgres.db_name
}

output "database_master_secret_arn" {
  description = "ARN of the RDS-managed secret holding the master username and password (JSON)."
  value       = aws_db_instance.postgres.master_user_secret[0].secret_arn
}

output "redis_address" {
  description = "Private host name of the Valkey primary endpoint (TLS, port 6379)."
  value       = aws_elasticache_replication_group.redis.primary_endpoint_address
}

output "redis_auth_token_secret_name" {
  description = "Secrets Manager secret holding the Valkey AUTH token."
  value       = aws_secretsmanager_secret.redis_auth_token.name
}

output "operator_secret_names" {
  description = "Secrets whose values the operator writes with put-secret-value."
  value       = sort([for secret in aws_secretsmanager_secret.operator : secret.name])
}

output "external_secrets_role_arn" {
  description = "IAM role External Secrets Operator receives through EKS Pod Identity."
  value       = aws_iam_role.external_secrets.arn
}

output "configure_kubectl" {
  description = "Adds the cluster to the local kubeconfig as context llm-gateway-aws."
  value       = "aws eks update-kubeconfig --region eu-north-1 --name ${module.eks.cluster_name} --alias llm-gateway-aws"
}
