# NOTE: this is a hack to prevent resources not being applied because the namespace cannot be found
resource "kubernetes_namespace" "aibrix_dependency" {
  metadata {
    name = "envoy-gateway-system"
  }

  lifecycle {
    ignore_changes = [metadata[0].labels]
  }
}

resource "kubectl_manifest" "aibrix_dependency" {
  for_each = local.aibrix_dependency_yaml

  yaml_body = each.value

  # Server side apply to prevent errors installing CRDs
  server_side_apply = true

  depends_on = [kubernetes_namespace.aibrix_dependency]
}

# NOTE: this is a hack to prevent resources not being applied because the namespace cannot be found
resource "kubernetes_namespace" "aibrix_core" {
  metadata {
    name = "aibrix-system"
  }

  lifecycle {
    ignore_changes = [metadata[0].labels]
  }
}

resource "kubectl_manifest" "aibrix_core" {
  for_each = local.aibrix_core_yaml

  yaml_body = each.value

  # Server side apply to prevent errors installing CRDs
  server_side_apply = true

  depends_on = [kubernetes_namespace.aibrix_core, kubectl_manifest.aibrix_dependency]
}

resource "kubectl_manifest" "model_deployment" {
  yaml_body = local.model_yaml["Deployment"]

  depends_on = [kubectl_manifest.aibrix_core]

  # Only create if deploy model is set true
  count = var.deploy_example_model ? 1 : 0
}

resource "kubectl_manifest" "model_service" {
  yaml_body = local.model_yaml["Service"]

  depends_on = [kubectl_manifest.aibrix_core]

  # Only create if deploy model is set true
  count = var.deploy_example_model ? 1 : 0
}