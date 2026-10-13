variable "aws_account_id" {
  description = "Expected AWS account ID; prevents applying to another account."
  type        = string
  nullable    = false

  validation {
    condition     = can(regex("^[0-9]{12}$", var.aws_account_id))
    error_message = "aws_account_id must contain exactly 12 digits."
  }
}

variable "domain_name" {
  description = "Gateway hostname delegated to Route 53; empty keeps the original state/ECR-only bootstrap. Keep this value set after creating the zone."
  type        = string
  default     = ""
  nullable    = false

  validation {
    condition = var.domain_name == "" || (
      length(var.domain_name) <= 253 &&
      can(regex("^([a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?\\.)+[a-z]([a-z0-9-]{0,61}[a-z0-9])?$", var.domain_name))
    )
    error_message = "domain_name must be empty or a lowercase DNS hostname with labels of at most 63 characters, no wildcard, and no trailing dot."
  }
}
