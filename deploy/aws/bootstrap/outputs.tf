output "state_bucket_name" {
  description = "Bucket for bootstrap and future platform state, with separate keys."
  value       = aws_s3_bucket.state.id
}

output "ecr_repository_urls" {
  description = "Private repository URLs, keyed by repository name."
  value       = { for name, repository in aws_ecr_repository.images : name => repository.repository_url }
}
