output "state_bucket_name" {
  description = "Bucket for bootstrap and future platform state, with separate keys."
  value       = aws_s3_bucket.state.id
}

output "ecr_repository_urls" {
  description = "Private repository URLs, keyed by repository name."
  value       = { for name, repository in aws_ecr_repository.images : name => repository.repository_url }
}

output "gateway_hosted_zone_id" {
  description = "Persistent public zone for the gateway hostname; null when domain_name is empty."
  value       = one(aws_route53_zone.gateway[*].zone_id)
}

output "gateway_name_servers" {
  description = "Add these four NS records for the gateway subdomain at the parent DNS provider after applying."
  value       = flatten(aws_route53_zone.gateway[*].name_servers)
}

output "gateway_certificate_arn" {
  description = "ACM certificate request in eu-north-1; issuance must be checked after DNS delegation. Null when domain_name is empty."
  value       = one(aws_acm_certificate.gateway[*].arn)
}
