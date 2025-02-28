output "configure_kubectl_command" {
  value = "gcloud container clusters get-credentials ${google_container_cluster.main.name} --region ${var.default_region} --project ${var.project_id}"
}