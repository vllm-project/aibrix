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
