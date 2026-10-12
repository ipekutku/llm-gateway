module "eks" {
  source  = "terraform-aws-modules/eks/aws"
  version = "21.29.0"

  name = local.name

  # The newest version in standard support. Extended support costs six times
  # as much per hour, so the upgrade policy never moves the cluster into it.
  kubernetes_version = "1.37"
  upgrade_policy = {
    support_type = "STANDARD"
  }

  vpc_id     = module.vpc.vpc_id
  subnet_ids = module.vpc.private_subnets

  # kubectl from the operator's address only; nodes reach the API privately.
  endpoint_public_access       = true
  endpoint_public_access_cidrs = [var.admin_cidr]
  endpoint_private_access      = true

  # Access entries only, no aws-auth ConfigMap. Whoever applies gets cluster
  # admin through an access entry.
  authentication_mode                      = "API"
  enable_cluster_creator_admin_permissions = true

  # The project's SCPs deny IAM identity providers, so IRSA cannot exist.
  # Add-ons that need AWS permissions use EKS Pod Identity instead.
  enable_irsa = false

  # EKS already encrypts Secrets with an AWS-owned key. A customer-managed key
  # would bill monthly and stay pending deletion for days after every destroy.
  create_kms_key    = false
  encryption_config = null

  # Control-plane logs are billed by volume; application logs come later.
  enabled_log_types           = []
  create_cloudwatch_log_group = false

  # The network and Pod Identity agents start with the nodes; the others need
  # running nodes. Versions are the newest for this Kubernetes version.
  addons = {
    vpc-cni = {
      before_compute = true
    }
    eks-pod-identity-agent = {
      before_compute = true
    }
    kube-proxy     = {}
    coredns        = {}
    metrics-server = {}
  }

  # On-demand nodes: the project's SCPs deny Spot, and the Free plan launches
  # only free-tier-eligible types. Of those, c7i-flex.large is the smallest
  # with 4 GiB, enough for the later add-ons; it is x86, and both images are
  # built for amd64 too. Two nodes let the chart spread its two replicas.
  eks_managed_node_groups = {
    default = {
      ami_type       = "AL2023_x86_64_STANDARD"
      instance_types = ["c7i-flex.large"]
      capacity_type  = "ON_DEMAND"
      min_size       = 2
      max_size       = 3
      desired_size   = 2
    }
  }
}
