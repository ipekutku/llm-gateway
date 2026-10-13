locals {
  name = "llm-gateway"
}

# Two AZs, because EKS, RDS, and an ALB all require subnets in at least two.
# Nodes and, later, the data stores live in the private subnets; the public
# ones hold only the NAT gateway and, later, the load balancer. The private
# /19s leave room for the VPC CNI, which gives every pod a VPC address.
module "vpc" {
  source  = "terraform-aws-modules/vpc/aws"
  version = "6.7.3"

  name = local.name
  cidr = "10.0.0.0/16"
  azs  = ["eu-north-1a", "eu-north-1b"]

  private_subnets = ["10.0.0.0/19", "10.0.32.0/19"]
  public_subnets  = ["10.0.128.0/24", "10.0.129.0/24"]

  # One NAT gateway, in the first AZ, for both private subnets: about half the
  # cost of one per AZ, at the price of losing egress if that AZ fails.
  enable_nat_gateway = true
  single_nat_gateway = true

  enable_dns_hostnames = true
  enable_dns_support   = true

  public_subnet_tags  = { "kubernetes.io/role/elb" = "1" }
  private_subnet_tags = { "kubernetes.io/role/internal-elb" = "1" }
}

# A gateway endpoint has no charge. EKS add-on images come from ECR in this
# Region, which serves layers from S3, so their downloads skip the NAT
# gateway's per-GB processing charge.
resource "aws_vpc_endpoint" "s3" {
  vpc_id            = module.vpc.vpc_id
  service_name      = "com.amazonaws.eu-north-1.s3"
  vpc_endpoint_type = "Gateway"
  route_table_ids   = module.vpc.private_route_table_ids
}
