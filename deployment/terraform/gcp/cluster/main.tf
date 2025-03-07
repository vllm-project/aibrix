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