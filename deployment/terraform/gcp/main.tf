locals {}

resource "google_container_cluster" "main" {
  name = var.cluster_name

  # If cluster region is set, use it, otherwise use default
  location = var.cluster_region != "" ? var.cluster_region : var.default_region

  # Default node pool which immediately gets deleted
  remove_default_node_pool = true
  initial_node_count       = 1

  # Allows cluster to be deleted by terraform
  deletion_protection = false
}