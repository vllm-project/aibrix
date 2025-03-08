/*
Copyright 2025 The Aibrix Team.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

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

data "kubernetes_service" "aibrix_service" {
  metadata {
    name      = "envoy-aibrix-system-aibrix-eg-903790dc"
    namespace = kubernetes_namespace.aibrix_dependency.metadata[0].name
  }

  depends_on = [kubectl_manifest.aibrix_dependency, kubectl_manifest.aibrix_core]
}
