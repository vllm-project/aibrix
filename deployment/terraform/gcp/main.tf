resource "google_container_cluster" "main" {
  name     = var.cluster_name
  location = local.cluster_location

  # Default node pool which immediately gets deleted.
  remove_default_node_pool = true
  initial_node_count       = 1

  # Allows cluster to be deleted by terraform.
  deletion_protection = false
}

resource "google_service_account" "gpu_node_pool" {
  account_id   = local.service_account_id
  display_name = local.service_account_display_name
}

resource "google_container_node_pool" "gpu_node_pool" {
  name     = var.node_pool_name
  location = google_container_cluster.main.location
  cluster  = google_container_cluster.main.id

  node_locations = [local.available_node_pool_zones[0]]
  node_count     = var.node_pool_machine_count

  node_config {
    machine_type = var.node_pool_machine_type

    # Google recommends custom service accounts that have cloud-platform scope and permissions granted via IAM Roles.
    service_account = google_service_account.gpu_node_pool.email
    oauth_scopes = [
      "https://www.googleapis.com/auth/cloud-platform"
    ]
  }
}

module "aibrix" {
  source = "../kubernetes"

  aibrix_release_version = var.aibrix_release_version

  deploy_example_model = var.deploy_example_model
}