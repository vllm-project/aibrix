# Aibrix Integration with Envoy AI Gateway Deployment Guide

This guide walks you through deploying a multi-model AI inference gateway using **Envoy AI Gateway**, **Gateway API Inference Extension**, and custom Aibrix-branded routing rules.

### Project Structure

```bash
samples/ai-gateway-integration
├── gateway.yaml                  # GatewayClass + Gateway
├── aigatewayroute.yaml           # Multi-model routing rules (llama2-7b, mistral-7b)
├── llama-7b-inferencepool.yaml   # InferencePool + EPP for Llama2-7B
├── mistral-7b-inferencepool.yaml # InferencePool + EPP for Mistral-7B
└── llama-7b.yaml                 # Mock model llama-7b deployments
└── mistral-7b.yaml               # Mock model mistral-7b deployments
```

### Prerequisites
- Kubernetes cluster (v1.24+)
- kubectl configured
- helm v3.8+
- Internet access to pull images from docker.io and GitHub

### Installation Steps

1. Install Aibrix Custom Application (Optional)

If you have an internal Aibrix [Helm chart](../../dist/chart):
```bash
helm install aibrix dist/chart \
  -n aibrix-system --create-namespace \
  --set gateway.enable=false
```

> **Note**: If you are using an internal Aibrix Helm chart, **you must set `gateway.enable: false`** in `values.yaml`.  
> This is critical because **Steps 2–5 below will install the AI Gateway controller and Envoy data plane independently**. 
> Enabling the built-in gateway here would cause resource conflicts or duplicate deployments.

```yaml
...
gateway:
  enable: false  # ← Set this to false to skip internal gateway deployment
...
```

2. Install AI Gateway CRDs

```bash
helm upgrade -i aieg-crd oci://docker.io/envoyproxy/ai-gateway-crds-helm \
  --version v0.0.0-latest \
  --namespace envoy-ai-gateway-system \
  --create-namespace
```

> For more details, see the official [installation guide](https://aigateway.envoyproxy.io/docs/getting-started/installation#step-1-install-ai-gateway-crds) for AI Gateway CRDs.


3. Install AI Gateway Controller

```bash
helm upgrade -i aieg oci://docker.io/envoyproxy/ai-gateway-helm \
  --version v0.0.0-latest \
  --namespace envoy-ai-gateway-system \
  --create-namespace
```

> For more details, see the official [installation guide](https://aigateway.envoyproxy.io/docs/getting-started/installation#step-2-install-ai-gateway-resources) for AI Gateway Resources.

Wait for the controller to be ready:
```bash
kubectl wait --timeout=2m -n envoy-ai-gateway-system deployment/ai-gateway-controller --for=condition=Available
```

4. Install Gateway API Inference Extension (EPP Framework)

```bash
kubectl apply -f https://github.com/kubernetes-sigs/gateway-api-inference-extension/releases/download/v1.0.1/manifests.yaml
```

> For more details, see the official [installation guide](https://aigateway.envoyproxy.io/docs/capabilities/inference/httproute-inferencepool#step-1-install-gateway-api-inference-extension) for Gateway API Inference Extension.


This deploys:
CRDs (InferencePool, InferenceObjective)
RBAC, webhooks, and core controllers

5. Install Envoy Gateway (Data Plane)

```bash
helm upgrade -i eg oci://docker.io/envoyproxy/gateway-helm \
  --version v0.0.0-latest \
  --namespace envoy-gateway-system \
  --create-namespace \
  -f https://raw.githubusercontent.com/envoyproxy/ai-gateway/main/manifests/envoy-gateway-values.yaml \
  -f https://raw.githubusercontent.com/envoyproxy/ai-gateway/main/examples/inference-pool/envoy-gateway-values-addon.yaml
```

> For more details, see the official [installation guide](https://aigateway.envoyproxy.io/docs/getting-started/prerequisites#additional-features-rate-limiting-inferencepool-etc) for Envoy Gateway.


6. Deploy Aibrix AI Gateway Resources

Apply your custom gateway and routing configuration:

```bash
cd samples/ai-gateway-integration

# Deploy for each model
kubectl apply -f llama-7b.yaml
kubectl apply -f mistral-7b.yaml

# Deploy GatewayClass, Gateway, and AIGatewayRoute
kubectl apply -f gateway.yaml
kubectl apply -f aigatewayroute.yaml

# Deploy backend resources for each model
kubectl apply -f llama-7b-inferencepool.yaml
kubectl apply -f mistral-7b-inferencepool.yaml
```

### Verify Deployment Status

After installation, you can verify that all components are running correctly. Below is an example of expected output from a successful deployment:

-  Pods in `aibrix-system`
```bash
$ kubectl get pods -n aibrix-system
NAME                                         READY   STATUS    RESTARTS   AGE
aibrix-controller-manager-7dcf4b8d97-9mgw8   1/1     Running   0          3h35m
aibrix-gpu-optimizer-556d946fbb-gzh85        1/1     Running   0          3h35m
aibrix-metadata-service-bdfd4459d-678k5      1/1     Running   0          3h35m
aibrix-redis-master-74945dc65d-sr2sq         1/1     Running   0          3h35m
```

- Pods in `envoy-ai-gateway-system`
```bash
$ kubectl get pods -n envoy-ai-gateway-system
NAME                                     READY   STATUS    RESTARTS   AGE
ai-gateway-controller-5558c7cf7c-bzh65   1/1     Running   0          3h34m 
```

- Pods in `envoy-gateway-system`
```bash
$ kubectl get pods -n envoy-gateway-system
NAME                                                       READY   STATUS    RESTARTS   AGE
envoy-default-aibrix-ai-gateway-588291e8-54d5f9b6f-2psp6   3/3     Running   0          128m
envoy-gateway-6dd8f9b8f-kjngn                              1/1     Running   0          3h33m 
```

- AI Gateway CRDs
```bash
$ kubectl get InferencePool
NAME         AGE
llama2-7b    121m
mistral-7b   121m

$ kubectl get InferenceObjective
NAME         INFERENCE POOL   PRIORITY   AGE
llama2-7b    llama2-7b        10         121m
mistral-7b   mistral-7b       10         121m 
```

- Model and EPP Backend Pods (in default namespace)

```bash
$ kubectl get pods
NAME                               READY   STATUS    RESTARTS   AGE
llama2-7b-epp-6fb99fd7df-7xlxq     1/1     Running   0          121m
mistral-7b-epp-7c7f7fcb66-bw87d    1/1     Running   0          121m
mock-llama2-7b-6444f9b459-7gzmx    1/1     Running   0          131m
mock-llama2-7b-6444f9b459-92bsl    1/1     Running   0          131m
mock-llama2-7b-6444f9b459-krj8c    1/1     Running   0          131m
mock-mistral-7b-5fddcff595-5268f   1/1     Running   0          131m
mock-mistral-7b-5fddcff595-t65cp   1/1     Running   0          131m
```

### Test the Setup

Once all pods are ready, test routing via curl:

- Llama2-7B

```bash
curl -v http://<GATEWAY_IP>/v1/chat/completions \
  -H "Content-Type: application/json" \
  -H "x-ai-eg-model: llama2-7b" \
  -H "Authorization: Bearer test-key-1234567890" \
  -d '{
        "model": "llama2-7b",
        "messages": [{"role": "user", "content": "Say this is a test!"}],
        "temperature": 0.7
      }'
```

- Mistral-7B

```bash
curl -v http://<GATEWAY_IP>/v1/chat/completions \
  -H "Content-Type: application/json" \
  -H "x-ai-eg-model: mistral-7b" \
  -H "Authorization: Bearer test-key-0987654321" \
  -d '{
        "model": "mistral-7b",
        "messages": [{"role": "user", "content": "Say this is a test!"}],
        "temperature": 0.7
      }'
```

Replace `<GATEWAY_IP>` with:
- localhost:8080 if using

```bash
kubectl port-forward -n envoy-gateway-system svc/envoy-default-aibrix-ai-gateway-588291e8 8080:80
```

Or the external IP of the `eg-envoy` Service if exposed via LoadBalancer.

### References
- [Envoy AI Gateway](https://github.com/envoyproxy/ai-gateway)
- [Gateway API Inference Extension](https://github.com/kubernetes-sigs/gateway-api-inference-extension)
