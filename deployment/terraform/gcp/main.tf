resource "google_container_cluster" "main" {
  name     = var.cluster_name
  location = local.cluster_location

  # Default node pool which immediately gets deleted
  remove_default_node_pool = true
  initial_node_count       = 1

  # Allows cluster to be deleted by terraform
  deletion_protection = false
}