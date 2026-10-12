# Secrets for the gateway, read by the cluster in a later step. Secrets created
# here are deleted without a recovery window, so the next session can create
# the same names again. The database password is not here: RDS keeps it in a
# secret of its own, outside Terraform state.

# Containers only: Terraform never sees their values, which the operator
# writes with aws secretsmanager put-secret-value (see docs/aws.md).
resource "aws_secretsmanager_secret" "operator" {
  for_each = {
    "openai-api-key"    = "OpenAI API key for the gateway"
    "anthropic-api-key" = "Anthropic API key for the gateway"
    "clients"           = "Gateway clients file (clients.json)"
  }

  name                    = "${local.name}/${each.key}"
  description             = each.value
  recovery_window_in_days = 0
}

# The Valkey AUTH token is the one secret value in Terraform state, which is
# encrypted and private (see docs/aws.md). This keeps the cache and secret
# version using the same token across repeated applies. Each token dies with
# its session's cache; previous state versions still contain it.
# Alphanumeric, so it needs no escaping in a rediss:// URL.
resource "random_password" "redis_auth_token" {
  length  = 64
  special = false
}

resource "aws_secretsmanager_secret" "redis_auth_token" {
  name                    = "${local.name}/redis-auth-token"
  description             = "AUTH token of the llm-gateway Valkey cache"
  recovery_window_in_days = 0
}

resource "aws_secretsmanager_secret_version" "redis_auth_token" {
  secret_id     = aws_secretsmanager_secret.redis_auth_token.id
  secret_string = random_password.redis_auth_token.result
}
