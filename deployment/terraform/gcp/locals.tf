locals {
  # If cluster region is set, use it, otherwise use default
  cluster_location = var.cluster_zone != "" ? var.cluster_zone : var.default_region

  # If node pool region is set, use it, otherwise use default
  node_pool_location = var.node_pool_zone != "" ? var.node_pool_zone : var.default_region

  # Available GPU machine types within provided node pool location
  available_gpu_machine_types = toset(flatten([for zone in data.google_compute_machine_types.available : [for type in zone.machine_types : type.name]]))
}