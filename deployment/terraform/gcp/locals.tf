locals {
  # If cluster region is set, use it, otherwise use default.
  cluster_location = var.cluster_zone != "" ? var.cluster_zone : var.default_region

  # Available GPU machine types within provided node pool location.
  available_gpu_machine_types = toset(flatten([for zone in data.google_compute_machine_types.available : [for type in zone.machine_types : type.name]]))

  # Available zones for provided GPU machine type. Prevents runtime errors due to scarcity of machines.
  available_node_pool_zones = [for zone in data.google_compute_machine_types.available : zone.zone if provider::assert::contains([for type in zone.machine_types : type.name], var.node_pool_machine_type)]

  # Node pool service account id.
  service_account_id = substr("${var.node_pool_name}", 0, 30)

  # Node pool service account display name.
  service_account_display_name = title(replace(local.service_account_id, "-", " "))
}