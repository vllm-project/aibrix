/*
Copyright 2025 The Aibrix Team.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

output "configure_kubectl_command" {
  description = "Command to run which will allow kubectl access."
  value       = "gcloud container clusters get-credentials ${data.google_container_cluster.main.name} --region ${var.default_region} --project ${var.project_id}"
}

output "aibrix_service_public_ip" {
  description = "Public IP address for AIBrix service."
  value       = data.kubernetes_service.aibrix_service.status.0.load_balancer.0.ingress.0.ip
}
