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

resource "google_container_cluster" "main" {
  name     = var.cluster_name
  location = var.cluster_location

  # Default node pool which immediately gets deleted.
  remove_default_node_pool = true
  initial_node_count       = 1

  # Allows cluster to be deleted by terraform.
  deletion_protection = var.cluster_deletion_protection
}

resource "google_container_node_pool" "node_pool" {
  name     = var.node_pool_name
  location = google_container_cluster.main.location
  cluster  = google_container_cluster.main.id

  node_locations = [var.node_pool_zone]
  node_count     = var.node_pool_machine_count

  node_config {
    machine_type = var.node_pool_machine_type

    # Google recommends custom service accounts that have cloud-platform scope and permissions granted via IAM Roles.
    service_account = data.google_service_account.node_pool.email
    oauth_scopes = [
      "https://www.googleapis.com/auth/cloud-platform"
    ]
  }
}
