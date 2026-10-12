variable "aws_account_id" {
  description = "Expected AWS account ID; prevents applying to another account."
  type        = string
  nullable    = false

  validation {
    condition     = can(regex("^[0-9]{12}$", var.aws_account_id))
    error_message = "aws_account_id must contain exactly 12 digits."
  }
}

variable "admin_cidr" {
  description = "The only IPv4 range that may reach the public Kubernetes API endpoint, usually the operator's address as /32."
  type        = string
  nullable    = false

  validation {
    condition     = can(cidrhost(var.admin_cidr, 0)) && can(regex("^([0-9]{1,3}\\.){3}[0-9]{1,3}/(2[4-9]|3[0-2])$", var.admin_cidr))
    error_message = "admin_cidr must be an IPv4 CIDR of /24 or narrower, never an open range such as 0.0.0.0/0."
  }
}
