provider "google" {
  project = var.project_id
  region  = var.default_region
}

provider "assert" {}

provider "kubernetes" {
  host = "https://${google_container_cluster.main.endpoint}"
  token = data.google_client_config.default.access_token
  cluster_ca_certificate = base64decode(google_container_cluster.main.master_auth.0.cluster_ca_certificate)
}

provider "kubectl" {
  host = "https://${google_container_cluster.main.endpoint}"
  token = data.google_client_config.default.access_token
  cluster_ca_certificate = base64decode(google_container_cluster.main.master_auth.0.cluster_ca_certificate)
}