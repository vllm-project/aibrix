variable "cluster_name" {
  description = "Name of the GKE cluster."
  type        = string
}

variable "cluster_location" {
  description = "Location to deploy cluster."
  type        = string
  default     = ""
}

variable "cluster_deletion_protection" {
  description = "Whether to enable deletion protection for the cluster."
  type        = bool
}

variable "node_pool_name" {
  description = "Name of the node pool."
  type        = string
}

variable "node_pool_service_account_id" {
  description = "Service account id for the node pool."
}

variable "node_pool_zone" {
  description = "Zone to deploy GPU node pool within."
  type        = string
}

variable "node_pool_machine_type" {
  description = "Machine type for the node pool."
  type        = string
}

variable "node_pool_machine_count" {
  description = "Machine count for the node pool."
  type        = number
}