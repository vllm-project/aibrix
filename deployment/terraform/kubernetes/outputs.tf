output "aibrix_service" {
  description = "The service definition for the model"
  value       = data.kubernetes_service.aibrix_service
}