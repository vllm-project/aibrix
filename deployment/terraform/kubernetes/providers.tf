provider "kubernetes" {
  config_path = var.kube_config_path
}

provider "kubectl" {
  config_path = var.kube_config_path
}

provider "http" {}