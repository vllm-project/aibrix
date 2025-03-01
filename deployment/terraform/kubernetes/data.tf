data "http" "aibrix_depenency" {
  url = "https://github.com/vllm-project/aibrix/releases/download/${var.aibrix_release_version}/aibrix-dependency-${var.aibrix_release_version}.yaml"

  # Optional request headers
  request_headers = {
    Accept = "application/json"
  }
}

data "http" "aibrix_core" {
  url = "https://github.com/vllm-project/aibrix/releases/download/${var.aibrix_release_version}/aibrix-core-${var.aibrix_release_version}.yaml"

  # Optional request headers
  request_headers = {
    Accept = "application/json"
  }
}

data "http" "model" {
  url = "https://raw.githubusercontent.com/vllm-project/aibrix/refs/heads/main/samples/quickstart/model.yaml"

  # Optional request headers
  request_headers = {
    Accept = "application/json"
  }
}

data "kubernetes_service" "aibrix_service" {
  metadata {
    name      = "envoy-aibrix-system-aibrix-eg-903790dc"
    namespace = kubernetes_namespace.aibrix_dependency.metadata[0].name
  }

  depends_on = [kubectl_manifest.aibrix_dependency, kubectl_manifest.aibrix_core]
}