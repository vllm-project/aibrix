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

resource "google_service_account" "node_pool" {
  account_id   = local.service_account_id
  display_name = local.service_account_display_name
}

module "cluster" {
  source = "./cluster"

  cluster_name                = var.cluster_name
  cluster_location            = local.cluster_location
  cluster_deletion_protection = false

  node_pool_name               = var.node_pool_name
  node_pool_zone               = local.available_node_pool_zones[0]
  node_pool_machine_type       = var.node_pool_machine_type
  node_pool_machine_count      = var.node_pool_machine_count
  node_pool_service_account_id = google_service_account.node_pool.id

  depends_on = [google_service_account.node_pool]
}

module "aibrix" {
  source = "../kubernetes"

  aibrix_release_version = var.aibrix_release_version

  deploy_example_model = var.deploy_example_model
}
