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

data "google_client_config" "default" {
  depends_on = [google_container_cluster.main, google_container_node_pool.gpu_node_pool]
}