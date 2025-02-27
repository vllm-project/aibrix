variable "project_id" {
  description = "GCP project to deploy resources within"
}

variable "default_region" {
  description = "Default region to deploy resources within"
}

variable "cluster_name" {
  description = "Name of the GKE cluster"
  default     = "aibrix-inference-cluster"
}

variable "cluster_region" {
  description = "Region to deploy cluster within. If not provided will be set to default region"
  default     = ""
}