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
  yaml_body = <<EOT
apiVersion: apps/v1
kind: Deployment
metadata:
  labels:
    model.aibrix.ai/name: deepseek-r1-distill-llama-8b # Note: The label value `model.aibrix.ai/name` here must match with the service name.
    model.aibrix.ai/port: "8000"
  name: deepseek-r1-distill-llama-8b
  namespace: default
spec:
  replicas: 1
  selector:
    matchLabels:
      model.aibrix.ai/name: deepseek-r1-distill-llama-8b
  template:
    metadata:
      labels:
        model.aibrix.ai/name: deepseek-r1-distill-llama-8b
    spec:
      containers:
        - command:
            - python3
            - -m
            - vllm.entrypoints.openai.api_server
            - --host
            - "0.0.0.0"
            - --port
            - "8000"
            - --uvicorn-log-level
            - warning
            - --model
            - deepseek-ai/DeepSeek-R1-Distill-Llama-8B
            - --served-model-name
            # Note: The `--served-model-name` argument value must also match the Service name and the Deployment label `model.aibrix.ai/name`
            - deepseek-r1-distill-llama-8b
            - --max-model-len
            - "12288" # 24k length, this is to avoid "The model's max seq len (131072) is larger than the maximum number of tokens that can be stored in KV cache" issue.
          image: vllm/vllm-openai:v0.7.1
          imagePullPolicy: IfNotPresent
          name: vllm-openai
          ports:
            - containerPort: 8000
              protocol: TCP
          resources:
            limits:
              nvidia.com/gpu: "1"
            requests:
              nvidia.com/gpu: "1"
          livenessProbe:
            failureThreshold: 3
            httpGet:
              path: /health
              port: 8000
              scheme: HTTP
            initialDelaySeconds: 240
            periodSeconds: 5
            successThreshold: 1
            timeoutSeconds: 1
          readinessProbe:
            failureThreshold: 5
            httpGet:
              path: /health
              port: 8000
              scheme: HTTP
            initialDelaySeconds: 240
            periodSeconds: 5
            successThreshold: 1
            timeoutSeconds: 1
EOT

  depends_on = [kubectl_manifest.aibrix_core]

  # Only create if deploy model is set true
  count = var.deploy_example_model ? 1 : 0
}

resource "kubectl_manifest" "model_service" {
  yaml_body = <<EOT
apiVersion: v1
kind: Service
metadata:
  labels:
    model.aibrix.ai/name: deepseek-r1-distill-llama-8b
    prometheus-discovery: "true"
  annotations:
    prometheus.io/scrape: "true"
    prometheus.io/port: "8080"
  name: deepseek-r1-distill-llama-8b # Note: The Service name must match the label value `model.aibrix.ai/name` in the Deployment
  namespace: default
spec:
  ports:
    - name: serve
      port: 8000
      protocol: TCP
      targetPort: 8000
    - name: http
      port: 8080
      protocol: TCP
      targetPort: 8080
  selector:
    model.aibrix.ai/name: deepseek-r1-distill-llama-8b
  type: ClusterIP
EOT

  depends_on = [kubectl_manifest.aibrix_core]

  # Only create if deploy model is set true
  count = var.deploy_example_model ? 1 : 0
}