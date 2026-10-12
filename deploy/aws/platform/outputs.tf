output "cluster_name" {
  description = "EKS cluster name."
  value       = module.eks.cluster_name
}

output "configure_kubectl" {
  description = "Adds the cluster to the local kubeconfig as context llm-gateway-aws."
  value       = "aws eks update-kubeconfig --region eu-north-1 --name ${module.eks.cluster_name} --alias llm-gateway-aws"
}
