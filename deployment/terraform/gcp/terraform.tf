terraform {
  required_providers {
    google = {
      source  = "hashicorp/google"
      version = "6.22.0"
    }
    assert = {
      source  = "hashicorp/assert"
      version = "0.15.0"
    }
    kubernetes = {
      source  = "hashicorp/kubernetes"
      version = "2.36.0"
    }
    kubectl = {
      source  = "gavinbunney/kubectl"
      version = "1.19.0"
    }
  }
}