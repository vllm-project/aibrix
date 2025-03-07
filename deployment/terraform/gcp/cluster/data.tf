data "google_service_account" "node_pool" {
  account_id = var.node_pool_service_account_id
}