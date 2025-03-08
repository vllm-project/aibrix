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

variable "project_id" {
  description = "GCP project to deploy resources within."
  type        = string
}

variable "default_region" {
  description = "Default region to deploy resources within."
  type        = string
}

variable "cluster_name" {
  description = "Name of the GKE cluster."
  type        = string
  default     = "aibrix-inference-cluster"
}

variable "cluster_zone" {
  description = "Zone to deploy cluster within. If not provided will be deployed to default region."
  type        = string
  default     = ""
}

variable "node_pool_name" {
  description = "Name of the GPU node pool."
  type        = string
  default     = "aibrix-gpu-nodes"
}

variable "node_pool_zone" {
  description = "Zone to deploy GPU node pool within. If not provided will be deployed to zone in default region which has capacity for machine type."
  type        = string
  default     = ""
}

variable "node_pool_machine_type" {
  description = "Machine type for the node pool. Must be in the A3, A2, or G2 series."
  type        = string
  default     = "g2-standard-4"
  validation {
    condition     = provider::assert::contains(local.available_gpu_machine_types, var.node_pool_machine_type)
    error_message = "Machine type not valid or not available at location. Valid machine types at location are: ${join(", ", local.available_gpu_machine_types)}"
  }
}

variable "node_pool_machine_count" {
  description = "Machine count for the node pool."
  type        = number
  default     = 1
}

variable "aibrix_release_version" {
  description = "The version of AIBRix to deploy."
  type        = string
  default     = "v0.2.0"
}

variable "deploy_example_model" {
  description = "Whether to deploy the example model."
  type        = bool
  default     = true
}
