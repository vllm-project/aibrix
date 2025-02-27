provider "google" {
  project = var.project_id
  region  = var.default_region
}

provider "assert" {}