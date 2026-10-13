# The parent zone stays with the registrar. Delegate this child zone by hand
# using its four name servers after applying; no registrar credentials are needed.
resource "aws_route53_zone" "gateway" {
  count = var.domain_name == "" ? 0 : 1

  name          = var.domain_name
  comment       = "Persistent gateway delegation and ACM validation"
  force_destroy = false

  lifecycle {
    prevent_destroy = true
  }
}

# Request in eu-north-1, the future ALB's Region. Do not wait for issuance here:
# the first apply must finish so the operator can read the delegation outputs.
resource "aws_acm_certificate" "gateway" {
  count = var.domain_name == "" ? 0 : 1

  domain_name       = var.domain_name
  validation_method = "DNS"
  key_algorithm     = "RSA_2048"

  options {
    certificate_transparency_logging_preference = "ENABLED"
    export                                      = "DISABLED"
  }

  lifecycle {
    create_before_destroy = true
  }
}

# A single hostname with no wildcard or SANs has exactly one validation record.
# Keep it between sessions; ACM uses it for validation and eligible renewals.
resource "aws_route53_record" "certificate_validation" {
  count = var.domain_name == "" ? 0 : 1

  zone_id = aws_route53_zone.gateway[0].zone_id
  name    = one(aws_acm_certificate.gateway[0].domain_validation_options).resource_record_name
  type    = one(aws_acm_certificate.gateway[0].domain_validation_options).resource_record_type
  records = [one(aws_acm_certificate.gateway[0].domain_validation_options).resource_record_value]
  ttl     = 300
}
