output "configure_kubectl_command" {
  description = "Command to run which will allow kubectl access."
  value       = "gcloud container clusters get-credentials ${google_container_cluster.main.name} --region ${var.default_region} --project ${var.project_id}"
}

output "aibrix_service_public_ip" {
  description = "Public IP address for AIBrix service."
  value       = data.kubernetes_service.aibrix_service.status.0.load_balancer.0.ingress.0.ip
}