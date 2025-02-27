locals {}

resource "google_container_cluster" "main" {
  name     = var.cluster_name
  location = var.cluster_region != "" ? var.cluster_region : var.default_region

  # We can't create a cluster with no node pool defined, but we want to only use
  # separately managed node pools. So we create the smallest possible default
  # node pool and immediately delete it.
  remove_default_node_pool = true
  initial_node_count       = 1
}