provider "kubernetes" {
  host = var.kube_host
  token = var.kube_access_token
  cluster_ca_certificate = var.kube_cluster_ca_certificate
}

provider "kubectl" {
  host = var.kube_host
  token = var.kube_access_token
  cluster_ca_certificate = var.kube_cluster_ca_certificate
}