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

# Retrieve zones in the default region for usage in machine type lookup.
data "google_compute_zones" "default_region" {
  region = var.default_region
}

data "google_compute_machine_types" "available" {
  for_each = toset(var.node_pool_zone != "" ? [var.node_pool_zone] : data.google_compute_zones.default_region.names)

  # Filter for instances in the A3, A2, or G2 lines. These instances have NVidia GPUs attached by default.
  filter = "name = \"a3*\" OR name = \"a2*\" OR name = \"g2*\""
  zone   = each.value
}

data "google_container_cluster" "main" {
  name = module.cluster.name

  depends_on = [module.cluster]
}

data "google_client_config" "default" {
  depends_on = [module.cluster]
}

data "kubernetes_service" "aibrix_service" {
  metadata {
    name      = module.aibrix.aibrix_service.metadata[0].name
    namespace = module.aibrix.aibrix_service.metadata[0].namespace
  }

  depends_on = [module.aibrix]
}
