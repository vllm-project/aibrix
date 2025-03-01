variable "kube_host" {
  description = "Cluster API endpoint."
  type        = string
}

variable "kube_access_token" {
  description = "Kubernetes access token."
  type = string
}

variable "kube_cluster_ca_certificate" {
  description = "Kubernetes cluster CA certificate."
  type = string
}

variable "aibrix_release_version" {
  description = "Release version of AIBrix to deploy."
  type        = string
  default     = "v0.2.0"
}